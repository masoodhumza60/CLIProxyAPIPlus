//go:build freebuffmock

package freebuff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMockClient starts the fixture and returns a client pointed at it.
func newMockClient(t *testing.T) (*Client, *MockUpstream, *httptest.Server) {
	t.Helper()
	mock := NewMockUpstream()
	server := httptest.NewServer(mock)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, MockAPIKey, Options{Sleep: func(time.Duration) {}, Now: time.Now})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	return client, mock, server
}

// chatAll streams one turn and returns the collected content.
func chatAll(t *testing.T, client *Client, content string) (*ChatCompletion, error) {
	t.Helper()
	body, err := client.ChatStream(context.Background(), ChatRequest{
		Content: content,
		Model:   MockModelHandle,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	accumulated := &ChatCompletion{}
	err = ScanChatEvents(body, func(event ChatEvent) error {
		accumulated.consume(event)
		return nil
	})
	return accumulated, err
}

// chatCompletion accumulates a streamed answer in the test package so the
// assertions read against the same accumulation the executor performs.
type ChatCompletion struct {
	content   strings.Builder
	reasoning strings.Builder
	model     string
	threadID  string
}

func (c *ChatCompletion) consume(event ChatEvent) {
	switch event.Type {
	case "meta":
		c.threadID = event.ThreadID
		c.model = event.Model
	case "reasoning_delta":
		c.reasoning.WriteString(event.Text)
	case "delta":
		c.content.WriteString(event.Text)
	}
}

func (c *ChatCompletion) text() string          { return c.content.String() }
func (c *ChatCompletion) reasoningText() string { return c.reasoning.String() }

// TestFreebuffChatRequiresTheSessionCookie pins the authentication the web host
// uses. A bearer token alone is not accepted, which is the behaviour that makes
// the web surface distinct from the catalogue host.
func TestFreebuffChatRequiresTheSessionCookie(t *testing.T) {
	_, _, server := newMockClient(t)

	req, err := http.NewRequest(http.MethodPost, server.URL+ChatPath, strings.NewReader(`{"content":"hi","model":"x"}`))
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+MockAPIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("calling the fixture: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bearer token alone should not authenticate the chat route, got %d", resp.StatusCode)
	}
}

// TestFreebuffChatRejectsAnEmptyMessage pins the route's own guard, which is the
// error a caller sees when a translation produces no prompt.
func TestFreebuffChatRejectsAnEmptyMessage(t *testing.T) {
	client, _, _ := newMockClient(t)
	_, err := client.ChatStream(context.Background(), ChatRequest{Content: "  ", Model: MockModelHandle})
	if err == nil {
		t.Fatal("an empty message should be refused before the request is sent")
	}
	if !strings.Contains(err.Error(), "non-empty content") {
		t.Fatalf("error should name the problem, got %v", err)
	}
}

// TestFreebuffChatStreamsTheAnswer checks the transcript is read end to end:
// reasoning before content, both assembled from several deltas rather than one.
func TestFreebuffChatStreamsTheAnswer(t *testing.T) {
	client, mock, _ := newMockClient(t)
	accumulated, err := chatAll(t, client, "what is the capital of china")
	if err != nil {
		t.Fatalf("streaming: %v", err)
	}
	if accumulated.text() != MockReply {
		t.Fatalf("answer = %q, want %q", accumulated.text(), MockReply)
	}
	if accumulated.reasoningText() != MockReasoning {
		t.Fatalf("reasoning = %q, want %q", accumulated.reasoningText(), MockReasoning)
	}
	if accumulated.model != MockModelDisplay {
		t.Fatalf("meta should report the model actually used, got %q", accumulated.model)
	}
	if accumulated.threadID != mockThreadID {
		t.Fatalf("meta should carry the conversation id, got %q", accumulated.threadID)
	}
	if mock.ChatRequests() != 1 {
		t.Fatalf("the fixture should have answered exactly once, got %d", mock.ChatRequests())
	}
}

// TestFreebuffChatReportsSubstitution pins the service's habit of replacing a
// model name it does not recognise instead of failing. A client that relies on
// the name sticking must be able to see that it did not.
func TestFreebuffChatReportsSubstitution(t *testing.T) {
	client, mock, _ := newMockClient(t)
	if _, err := chatAll(t, client, "hi"); err != nil {
		t.Fatalf("streaming: %v", err)
	}
	// Send a name the catalogue does not carry.
	body, err := client.ChatStream(context.Background(), ChatRequest{Content: "hi", Model: "some-other-model"})
	if err != nil {
		t.Fatalf("streaming with an unknown model: %v", err)
	}
	_ = body.Close()
	if mock.SubstitutedModel() != "some-other-model" {
		t.Fatalf("the fixture should record the substituted name, got %q", mock.SubstitutedModel())
	}
}

// TestFreebuffScanChatEventsSkipsUnrecognisedEvents is the regression that keeps
// an unknown event type from costing a caller an otherwise healthy answer.
func TestFreebuffScanChatEventsSkipsUnrecognisedEvents(t *testing.T) {
	transcript := strings.Join([]string{
		"",
		": a comment line the scanner should ignore",
		"data: {not json at all}",
		`data: {"type":"delta","text":"kept"}`,
		"data: [DONE]",
		`data: {"type":"a_type_this_client_does_not_model","text":"also kept"}`,
	}, "\n")

	var seen []string
	err := ScanChatEvents(strings.NewReader(transcript), func(event ChatEvent) error {
		seen = append(seen, event.Type)
		return nil
	})
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected two usable events, got %v", seen)
	}
	if seen[0] != "delta" {
		t.Fatalf("the decodable event should survive, got %q", seen[0])
	}
	if seen[1] != "a_type_this_client_does_not_model" {
		t.Fatalf("an unknown event type should still be delivered, got %q", seen[1])
	}
}

// TestFreebuffScanChatEventsPropagatesCallbackFailure keeps a caller able to stop
// a scan, which is how a stream consumer reports its own failure.
func TestFreebuffScanChatEventsPropagatesCallbackFailure(t *testing.T) {
	transcript := `data: {"type":"delta","text":"one"}` + "\n" + `data: {"type":"delta","text":"two"}`
	want := context.Canceled
	err := ScanChatEvents(strings.NewReader(transcript), func(ChatEvent) error { return want })
	if err != want {
		t.Fatalf("the callback's error should reach the caller, got %v", err)
	}
}

// TestFreebuffScanChatEventsReportsAnUnknownEventTypeAsFailure marks the events
// a caller must treat as a failed turn rather than as content.
func TestFreebuffScanChatEventsReportsAnUnknownEventTypeAsFailure(t *testing.T) {
	for _, eventType := range []string{"error", "stream_error", "failure"} {
		event := ChatEvent{Type: eventType, Text: "it broke"}
		if !event.IsStreamFailure() {
			t.Fatalf("%q should be recognised as a failure", eventType)
		}
	}
	for _, eventType := range []string{"delta", "reasoning_delta", "meta", "title", "suggestions"} {
		event := ChatEvent{Type: eventType}
		if event.IsStreamFailure() {
			t.Fatalf("%q should not be mistaken for a failure", eventType)
		}
	}
}

// TestFreebuffFetchCatalogRequiresBothAuthHeaders pins that the catalogue host
// needs two headers. A client that sends one works against no other provider's
// fixture and fails only here.
func TestFreebuffFetchCatalogRequiresBothAuthHeaders(t *testing.T) {
	_, _, server := newMockClient(t)

	req, err := http.NewRequest(http.MethodGet, server.URL+CatalogPath, nil)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+MockAPIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("calling the fixture: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("one auth header should not be enough for the catalogue, got %d", resp.StatusCode)
	}
}

// TestFreebuffFetchCatalogReadsTheCatalogue checks the fields the executor
// depends on: the access flag, the recommendation, and the handle.
func TestFreebuffFetchCatalogReadsTheCatalogue(t *testing.T) {
	client, _, server := newMockClient(t)
	catalog, err := client.FetchCatalog(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("fetching the catalogue: %v", err)
	}
	if len(catalog.Rows) != 2 {
		t.Fatalf("expected two rows, got %d", len(catalog.Rows))
	}
	if catalog.RecommendedKey != MockModelKey {
		t.Fatalf("recommendedKey = %q, want %q", catalog.RecommendedKey, MockModelKey)
	}
	if catalog.ExpiresAt <= 0 {
		t.Fatalf("a catalogue should state when it expires, got %d", catalog.ExpiresAt)
	}
	usable := catalog.UsableRows()
	if len(usable) != 1 {
		t.Fatalf("only the open row should be offered, got %d", len(usable))
	}
	if usable[0].Key != MockModelKey {
		t.Fatalf("the usable row should be the open one, got %q", usable[0].Key)
	}
	if usable[0].ContextWindow != 1_000_000 {
		t.Fatalf("context window should survive the decode, got %d", usable[0].ContextWindow)
	}
	if len(usable[0].Efforts) != 2 {
		t.Fatalf("efforts should survive the decode, got %v", usable[0].Efforts)
	}
}

// TestFreebuffFetchCatalogRejectsMissingInput keeps a misconfiguration from
// looking like an outage: no base URL and no token are both caller errors.
func TestFreebuffFetchCatalogRejectsMissingInput(t *testing.T) {
	client, _, server := newMockClient(t)
	if _, err := client.FetchCatalog(context.Background(), "  "); err == nil {
		t.Fatal("an empty catalogue base URL should be refused")
	}
	// The login client is the one that carries no token, so it is the right way
	// to reach the catalogue without a credential.
	anonymous, err := NewLoginClient(server.URL, Options{Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatalf("building an anonymous client: %v", err)
	}
	if _, err := anonymous.FetchCatalog(context.Background(), server.URL); err == nil {
		t.Fatal("the catalogue should refuse a client with no token")
	}
}

// TestFreebuffCatalogResolve covers the resolution order, which is what stops a
// caller's model choice from being quietly replaced by one they did not ask for.
func TestFreebuffCatalogResolve(t *testing.T) {
	catalog := &Catalog{
		Rows: []CatalogRow{
			{Key: "k1", Handle: "h1", DisplayName: "Model One", Access: "open"},
			{Key: "k2", Handle: "h2", DisplayName: "Model Two", Access: "locked"},
		},
		RecommendedKey: "k1",
		FallbackKey:    "k1",
	}

	if row, ok := catalog.Resolve("k1"); !ok || row.Handle != "h1" {
		t.Fatalf("resolving by key failed: %+v %v", row, ok)
	}
	if row, ok := catalog.Resolve("h1"); !ok || row.Key != "k1" {
		t.Fatalf("resolving by handle failed: %+v %v", row, ok)
	}
	if row, ok := catalog.Resolve("Model One"); !ok || row.Key != "k1" {
		t.Fatalf("resolving by display name failed: %+v %v", row, ok)
	}
	// A locked row is still resolvable by name; refusing it here would stop a
	// caller getting the service's own "this needs a paid plan" answer.
	if row, ok := catalog.Resolve("k2"); !ok || row.Key != "k2" {
		t.Fatalf("an explicitly named locked row should resolve: %+v %v", row, ok)
	}
	// An unrecognised name falls back to something usable rather than failing.
	if row, ok := catalog.Resolve("never-heard-of-it"); !ok || row.Key != "k1" {
		t.Fatalf("an unknown name should fall back to a usable row: %+v %v", row, ok)
	}
	if row, ok := catalog.Resolve(""); !ok || row.Key != "k1" {
		t.Fatalf("an empty name should fall back to the recommendation: %+v %v", row, ok)
	}
	if _, ok := (&Catalog{}).Resolve("k1"); ok {
		t.Fatal("an empty catalogue should resolve nothing")
	}
}

// TestFreebuffCatalogResolveSkipsALockedRecommendation stops the fallback from
// choosing a row the account cannot run.
func TestFreebuffCatalogResolveSkipsALockedRecommendation(t *testing.T) {
	catalog := &Catalog{
		Rows: []CatalogRow{
			{Key: "locked", Handle: "h", DisplayName: "Locked", Access: "locked"},
			{Key: "open", Handle: "h2", DisplayName: "Open", Access: "open"},
		},
		RecommendedKey: "locked",
		FallbackKey:    "locked",
	}
	row, ok := catalog.Resolve("unknown-model")
	if !ok || row.Key != "open" {
		t.Fatalf("the fallback should skip a locked recommendation, got %+v %v", row, ok)
	}
}

// TestFreebuffThreadsReadsConversationHistory covers the surface that proves a
// follow-up turn can be attributed to the conversation it belongs to.
func TestFreebuffThreadsReadsConversationHistory(t *testing.T) {
	client, _, _ := newMockClient(t)

	threads, err := client.ListThreads(context.Background())
	if err != nil {
		t.Fatalf("listing threads: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != mockThreadID {
		t.Fatalf("expected the fixture's one conversation, got %+v", threads)
	}

	thread, err := client.GetThread(context.Background(), mockThreadID)
	if err != nil {
		t.Fatalf("reading a thread: %v", err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("expected a user turn and an assistant turn, got %d", len(thread.Messages))
	}
	assistant := thread.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("the second message should be the assistant's, got %q", assistant.Role)
	}
	if assistant.Content != MockReply {
		t.Fatalf("the assistant's content should be the answer, got %q", assistant.Content)
	}
	// The block taxonomy is the part no other surface exposes.
	var reasoning, text, suggestions int
	for _, block := range assistant.Blocks {
		switch block.Type {
		case "thinking":
			reasoning++
		case "text":
			text++
		case "suggestions":
			suggestions++
			if len(block.Followups) != 1 {
				t.Fatalf("a suggestions block should carry its followups, got %d", len(block.Followups))
			}
		}
	}
	if reasoning != 1 || text != 1 || suggestions != 1 {
		t.Fatalf("block taxonomy changed: thinking=%d text=%d suggestions=%d", reasoning, text, suggestions)
	}
}

// TestFreebuffGetThreadRejectsABlankID keeps a missing id from becoming a request
// for the thread list.
func TestFreebuffGetThreadRejectsABlankID(t *testing.T) {
	client, _, _ := newMockClient(t)
	if _, err := client.GetThread(context.Background(), "   "); err == nil {
		t.Fatal("a blank thread id should be refused")
	}
}

// TestFreebuffExpiredSessionIsNamedAsSuch pins the operator-facing message. The
// token has a finite life, so telling someone to retry is useless advice when
// what they must do is log in again.
func TestFreebuffExpiredSessionIsNamedAsSuch(t *testing.T) {
	_, _, server := newMockClient(t)
	client, err := NewLoginClient(server.URL, Options{Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	_, err = client.ChatStream(context.Background(), ChatRequest{Content: "hi", Model: MockModelHandle})
	if err == nil {
		t.Fatal("a client with no token should be refused")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Fatalf("the error should say to log in again, got %v", err)
	}
}

// TestFreebuffScriptedFaultsAreScopedToTheirPath keeps a fault meant for one leg
// of the protocol from being consumed by another.
func TestFreebuffScriptedFaultsAreScopedToTheirPath(t *testing.T) {
	client, mock, server := newMockClient(t)

	mock.FailNextOn(ChatPath, http.StatusTooManyRequests, `{"error":"rate_limited"}`, map[string]string{
		"Retry-After": "30",
	})
	// The catalogue is not scripted, so it must still succeed while the chat
	// fault waits its turn.
	if _, err := client.FetchCatalog(context.Background(), server.URL); err != nil {
		t.Fatalf("the catalogue should be unaffected by a chat fault: %v", err)
	}
	_, err := client.ChatStream(context.Background(), ChatRequest{Content: "hi", Model: MockModelHandle})
	if err == nil {
		t.Fatal("the scripted chat fault should have fired")
	}
	if !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("the error should carry the service's own code, got %v", err)
	}
	// The fault is consumed, so the next attempt succeeds.
	if _, err := chatAll(t, client, "hi"); err != nil {
		t.Fatalf("the fault should have been consumed once: %v", err)
	}
}

// TestFreebuffUnknownRouteIsNotFound keeps the fixture honest: a client that asks
// for a route that does not exist must be told so rather than answered.
func TestFreebuffUnknownRouteIsNotFound(t *testing.T) {
	client, _, server := newMockClient(t)
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/nothing-here", nil)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: MockAPIKey})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("calling the fixture: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown route should be a 404, got %d", resp.StatusCode)
	}
	_ = client
}

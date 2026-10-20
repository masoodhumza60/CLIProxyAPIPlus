//go:build freebuffmock

package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// chatAll starts a run and streams one turn against it, the lifecycle every
// real client follows.
func chatAll(t *testing.T, client *Client, content string) (*ChatCompletion, error) {
	t.Helper()
	runID, err := client.StartAgentRun(context.Background(), client.BaseURL(), RegisterAgentID)
	if err != nil {
		return nil, err
	}
	return chatPayload(t, client, fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"codebuff_metadata":{"run_id":%q,"client_id":"test-client"}}`, MockModelHandle, content, runID))
}

// chatPayload streams a pre-rendered request body and returns the collected
// answer. Sending raw bodies keeps the transport tests honest about what
// actually crosses the wire.
func chatPayload(t *testing.T, client *Client, payload string) (*ChatCompletion, error) {
	t.Helper()
	body, err := client.ChatStream(context.Background(), client.BaseURL(), []byte(payload))
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	accumulated := &ChatCompletion{}
	err = ScanChatCompletionEvents(body, func(delta ChatCompletionDelta) error {
		accumulated.consume(delta)
		return nil
	})
	return accumulated, err
}

// ChatCompletion accumulates a streamed answer in the test package so the
// assertions read against the same accumulation the executor performs.
type ChatCompletion struct {
	content   strings.Builder
	reasoning strings.Builder
	model     string
	finish    string
	usage     *ChatCompletionUsage
}

func (c *ChatCompletion) consume(delta ChatCompletionDelta) {
	if delta.Model != "" {
		c.model = delta.Model
	}
	c.content.WriteString(delta.Content)
	c.reasoning.WriteString(delta.Reasoning)
	if delta.FinishReason != "" {
		c.finish = delta.FinishReason
	}
	if delta.Usage != nil {
		c.usage = delta.Usage
	}
}

func (c *ChatCompletion) text() string          { return c.content.String() }
func (c *ChatCompletion) reasoningText() string { return c.reasoning.String() }

// TestFreebuffChatRequiresBothAuthHeaders pins the authentication the chat host
// uses: the two headers the catalogue route also requires, either alone
// refused — which is the behaviour a third-party client meets on the live
// route.
func TestFreebuffChatRequiresBothAuthHeaders(t *testing.T) {
	_, _, server := newMockClient(t)

	for name, headers := range map[string][]string{
		"apiKeyHeaderOnly": {APIKeyHeader},
		"bearerOnly":       {"Authorization"},
	} {
		req, err := http.NewRequest(http.MethodPost, server.URL+ChatPath, strings.NewReader(`{"messages":[]}`))
		if err != nil {
			t.Fatalf("building a request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for _, header := range headers {
			switch header {
			case "Authorization":
				req.Header.Set("Authorization", "Bearer "+MockAPIKey)
			default:
				req.Header.Set(header, MockAPIKey)
			}
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("calling the fixture: %v", err)
		}
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status != http.StatusUnauthorized {
			t.Fatalf("%s: one auth header should not authenticate the chat route, got %d", name, status)
		}
	}
}

// TestFreebuffChatRejectsAnEmptyRequest pins the client-side guard, which is
// the error a caller sees when it has nothing to send, and the route's own
// guard for a body that carries no messages.
func TestFreebuffChatRejectsAnEmptyRequest(t *testing.T) {
	client, _, _ := newMockClient(t)

	if _, err := client.ChatStream(context.Background(), client.BaseURL(), []byte("   ")); err == nil {
		t.Fatal("a blank body should be refused before the request is sent")
	} else if !strings.Contains(err.Error(), "request body") {
		t.Fatalf("error should name the problem, got %v", err)
	}

	_, err := chatPayload(t, client, `{"model":"x","messages":[]}`)
	if err == nil {
		t.Fatal("a request with no messages should be refused")
	}
	if !strings.Contains(err.Error(), "empty_message") {
		t.Fatalf("the error should carry the service's own code, got %v", err)
	}
}

// TestFreebuffChatStreamsTheAnswer checks the transcript is read end to end:
// reasoning before content, both assembled from several deltas rather than one,
// with the model the upstream echoed and the usage the final chunk carries.
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
		t.Fatalf("the chunks should echo the model actually used, got %q", accumulated.model)
	}
	if accumulated.finish != "stop" {
		t.Fatalf("the stream should finish normally, got %q", accumulated.finish)
	}
	if accumulated.usage == nil {
		t.Fatal("the final chunk should carry usage")
	} else if accumulated.usage.PromptTokens == 0 || accumulated.usage.CompletionTokens == 0 {
		t.Fatalf("usage should be non-zero, got %+v", *accumulated.usage)
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
	// Send a name the catalogue does not carry, inside a fresh run.
	runID, err := client.StartAgentRun(context.Background(), client.BaseURL(), RegisterAgentID)
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	unknownModelPayload := fmt.Sprintf(`{"model":"some-other-model","messages":[{"role":"user","content":"hi"}],"codebuff_metadata":{"run_id":%q,"client_id":"test-client"}}`, runID)
	if _, err := chatPayload(t, client, unknownModelPayload); err != nil {
		t.Fatalf("streaming with an unknown model: %v", err)
	}
	if mock.SubstitutedModel() != "some-other-model" {
		t.Fatalf("the fixture should record the substituted name, got %q", mock.SubstitutedModel())
	}
}

// TestFreebuffScanChatCompletionEventsSkipsUnreadableChunks is the regression
// that keeps an undecodable or empty chunk from costing a caller an otherwise
// healthy answer.
func TestFreebuffScanChatCompletionEventsSkipsUnreadableChunks(t *testing.T) {
	transcript := strings.Join([]string{
		"",
		": a comment line the scanner should ignore",
		"data: {not json at all}",
		`data: {"type":null}`,
		`data: {"unknown_field_certainly_not_a_choice":"kept?"}`,
		`data: {"model":"m","choices":[{"index":0,"delta":{"content":"kept"}}]}`,
		"data: [DONE]",
		`data: {"model":"m","choices":[{"index":0,"delta":{"content":"never seen"}}]}`,
	}, "\n")

	var text strings.Builder
	err := ScanChatCompletionEvents(strings.NewReader(transcript), func(delta ChatCompletionDelta) error {
		text.WriteString(delta.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	if text.String() != "kept" {
		t.Fatalf("only the decodable chunk should survive, got %q", text.String())
	}
}

// TestFreebuffScanChatCompletionEventsPropagatesCallbackFailure keeps a caller
// able to stop a scan, which is how a stream consumer reports its own failure.
func TestFreebuffScanChatCompletionEventsPropagatesCallbackFailure(t *testing.T) {
	transcriptLen := strings.Join([]string{
		`data: {"model":"m","choices":[{"index":0,"delta":{"content":"one"}}]}`,
		`data: {"model":"m","choices":[{"index":0,"delta":{"content":"two"}}]}`,
	}, "\n")
	want := context.Canceled
	err := ScanChatCompletionEvents(strings.NewReader(transcriptLen), func(ChatCompletionDelta) error {
		return want
	})
	if err != want {
		t.Fatalf("the callback's error should reach the caller, got %v", err)
	}
}

// TestFreebuffScanChatCompletionEventsReportsAnInStreamErrorAsFailure turns a
// failure the service delivers inside the stream into an error rather than
// content, since a half-answer presented as complete would be worse than the
// request failing.
func TestFreebuffScanChatCompletionEventsReportsAnInStreamErrorAsFailure(t *testing.T) {
	transcript := `data: {"error":{"message":"it broke","code":"boom"}}` + "\n" + `data: {"model":"m","choices":[{"index":0,"delta":{"content":"a partial answer"}}]}`
	err := ScanChatCompletionEvents(strings.NewReader(transcript), func(ChatCompletionDelta) error {
		return nil
	})
	var se *SessionError
	if !errors.As(err, &se) {
		t.Fatalf("an in-stream error should surface as a SessionError, got %v", err)
	}
	if se.Message != "it broke" {
		t.Fatalf("the error should carry the service's message, got %q", se.Message)
	}
	if se.StatusCode() != http.StatusBadGateway {
		t.Fatalf("an in-stream error is an upstream failure, got %d", se.StatusCode())
	}
}

// TestFreebuffScanChatCompletionEventsStopsAtDone pins that [DONE] ends the
// scan rather than being an error, with nothing after it delivered.
func TestFreebuffScanChatCompletionEventsStopsAtDone(t *testing.T) {
	transcript := strings.Join([]string{
		`data: {"model":"m","choices":[{"index":0,"delta":{"content":"kept"}}]}`,
		"data: [DONE]",
		`data: {"model":"m","choices":[{"index":0,"delta":{"content":"ignore me"}}]}`,
	}, "\n")

	var text strings.Builder
	err := ScanChatCompletionEvents(strings.NewReader(transcript), func(delta ChatCompletionDelta) error {
		text.WriteString(delta.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	if text.String() != "kept" {
		t.Fatalf("only pre-done content should be delivered, got %q", text.String())
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

// TestFreebuffMissingTokenIsNamed pins the operator-facing message. The token
// has a finite life, so telling someone to retry is useless advice when what
// they must do is log in again.
func TestFreebuffMissingTokenIsNamed(t *testing.T) {
	_, _, server := newMockClient(t)
	client, err := NewLoginClient(server.URL, Options{Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"model": MockModelHandle, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatalf("building a payload: %v", err)
	}
	_, err = client.ChatStream(context.Background(), client.BaseURL(), payload)
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
	// The chat route only accepts a body naming a run the fixture minted, so
	// the scripted chat payload starts a run first — the same lifecycle every
	// real client follows.
	runID, err := client.StartAgentRun(context.Background(), server.URL, RegisterAgentID)
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"codebuff_metadata":{"run_id":%q,"client_id":"test-client"}}`, MockModelHandle, runID)
	requestErr := func() error {
		_, err := client.ChatStream(context.Background(), client.BaseURL(), []byte(payload))
		return err
	}()
	if requestErr == nil {
		t.Fatal("the scripted chat fault should have fired")
	}
	requestErrText := requestErr.Error()
	if !strings.Contains(requestErrText, "rate_limited") {
		t.Fatalf("the error should carry the service's own code, got %v", requestErr)
	}
	// The fault is consumed, so the next attempt succeeds.
	if _, err := chatPayload(t, client, payload); err != nil {
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

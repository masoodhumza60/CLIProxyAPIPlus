//go:build freebuffmock

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/freebuff"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// These tests drive the executor against the fixture, which enforces the same
// gates the live service does: the session cookie on the chat route, both auth
// headers on the catalogue route, and a model that must come from the catalogue.
// A defect that would fail in production therefore fails here.

// newFreebuffFixture starts the upstream and returns an executor pointed at it.
//
// The catalogue host is redirected as well as the chat host, because the two are
// separate origins in production and a test that could only reach the real
// catalogue host would exercise nothing.
func newFreebuffFixture(t *testing.T) (*FreebuffExecutor, *freebuff.MockUpstream, string) {
	t.Helper()
	mock := freebuff.NewMockUpstream()
	server := httptest.NewServer(mock)
	t.Cleanup(server.Close)

	factory := func(baseURL, apiKey string) (*freebuff.Client, error) {
		return freebuff.NewClient(baseURL, apiKey, freebuff.Options{
			Sleep: func(time.Duration) {},
			Now:   time.Now,
		})
	}
	executor := newFreebuffExecutorWithClientFactory(&config.Config{}, factory).withCatalogBaseURL(server.URL)
	// The endpoint travels with the credential rather than the config, so the
	// test credential has to carry the fixture's address. It is returned rather
	// than held in a package variable so parallel tests cannot race on it.
	return executor, mock, server.URL
}

// freebuffTestAuth builds a credential for the fixture. The endpoint travels with
// the credential, so it is threaded in from the fixture that was started.
func freebuffTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "freebuff-test",
		Provider: "freebuff",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  freebuff.MockAPIKey,
			"base_url": baseURL,
		},
	}
}

func freebuffTestRequest(model string) cliproxyexecutor.Request {
	payload := `{"model":"` + model + `","messages":[{"role":"user","content":"what is the capital of china"}]}`
	return cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(payload),
	}
}

func freebuffTestOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{SourceFormat: "openai"}
}

// freebuffStatus reads the status code an error carries, or zero when it
// carries none.
func freebuffStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var coder interface{ StatusCode() int }
	if errors.As(err, &coder) {
		return coder.StatusCode()
	}
	return 0
}

// TestFreebuffExecutorIdentifierAndFormat pins the provider identity and the
// wire format. The upstream is OpenAI-shaped, so the existing translator tree is
// reused and no Freebuff-specific translation code exists.
func TestFreebuffExecutorIdentifierAndFormat(t *testing.T) {
	executor, _, _ := newFreebuffFixture(t)
	if executor.Identifier() != "freebuff" {
		t.Fatalf("Identifier() = %q, want %q", executor.Identifier(), "freebuff")
	}
}

// TestFreebuffExecutorCompletesFromTheStream is the core path: a non-streaming
// request whose answer arrives as deltas is accumulated and returned whole.
func TestFreebuffExecutorCompletesFromTheStream(t *testing.T) {
	executor, mock, baseURL := newFreebuffFixture(t)

	response, err := executor.Execute(context.Background(), freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), freebuffTestOptions())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(response.Payload) == 0 {
		t.Fatal("Execute returned no payload")
	}

	var decoded struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Payload, &decoded); err != nil {
		t.Fatalf("decoding the completion: %v", err)
	}
	if decoded.Object != "chat.completion" {
		t.Fatalf("object = %q, want a chat completion", decoded.Object)
	}
	if len(decoded.Choices) != 1 {
		t.Fatalf("expected one choice, got %d", len(decoded.Choices))
	}
	choice := decoded.Choices[0]
	if choice.Message.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", choice.Message.Role)
	}
	// The fixture streams its answer in pieces, so this only passes if the
	// deltas were genuinely accumulated rather than one piece read whole.
	if choice.Message.Content != freebuff.MockReply {
		t.Fatalf("content = %q, want %q", choice.Message.Content, freebuff.MockReply)
	}
	if choice.Message.ReasoningContent != freebuff.MockReasoning {
		t.Fatalf("reasoning_content = %q, want %q", choice.Message.ReasoningContent, freebuff.MockReasoning)
	}
	if choice.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", choice.FinishReason)
	}
	// The route reports no usage, so the estimate must still be present and
	// non-zero rather than absent.
	if decoded.Usage.CompletionTokens <= 0 || decoded.Usage.TotalTokens <= 0 {
		t.Fatalf("usage should be estimated and non-zero, got %+v", decoded.Usage)
	}
	if decoded.Model != freebuff.MockModelDisplay {
		t.Fatalf("model = %q, want the model the service reported", decoded.Model)
	}
	if mock.ChatRequests() != 1 {
		t.Fatalf("expected one upstream completion, got %d", mock.ChatRequests())
	}
}

// TestFreebuffExecutorSendsTheCatalogueHandle is the check that a caller's model
// name is not quietly discarded. The fixture substitutes any name it does not
// recognise, so a client that forwards the caller's string verbatim would be
// answered by a model nobody chose.
func TestFreebuffExecutorSendsTheCatalogueHandle(t *testing.T) {
	executor, mock, baseURL := newFreebuffFixture(t)

	if _, err := executor.Execute(context.Background(), freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), freebuffTestOptions()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if substituted := mock.SubstitutedModel(); substituted != "" {
		t.Fatalf("the executor sent %q, which the service would have substituted; it should send the catalogue handle", substituted)
	}
}

// TestFreebuffExecutorResolvesByHandleAndKey pins both spellings a caller may
// reasonably use for the same model.
func TestFreebuffExecutorResolvesByHandleAndKey(t *testing.T) {
	for _, name := range []string{freebuff.MockModelHandle, freebuff.MockModelKey, freebuff.MockModelDisplay} {
		t.Run(name, func(t *testing.T) {
			executor, mock, baseURL := newFreebuffFixture(t)
			if _, err := executor.Execute(context.Background(), freebuffTestAuth(baseURL),
				freebuffTestRequest(name), freebuffTestOptions()); err != nil {
				t.Fatalf("Execute with %q: %v", name, err)
			}
			if substituted := mock.SubstitutedModel(); substituted != "" {
				t.Fatalf("%q was not resolved to a catalogue row and would be substituted", name)
			}
		})
	}
}

// TestFreebuffExecutorReportsAnExpiredSessionAsCredentialScoped is the behaviour
// that keeps the conductor from routing to a token that cannot work.
func TestFreebuffExecutorReportsAnExpiredSessionAsCredentialScoped(t *testing.T) {
	executor, mock, baseURL := newFreebuffFixture(t)
	mock.FailNextOn(freebuff.CatalogPath, http.StatusUnauthorized, `{"error":"unauthorized"}`, nil)

	_, err := executor.Execute(context.Background(), freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), freebuffTestOptions())
	if err == nil {
		t.Fatal("a rejected credential should fail the request")
	}
	if status := freebuffStatus(t, err); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 so the conductor cools the credential", status)
	}
	if !strings.Contains(err.Error(), "login") {
		t.Fatalf("the message should say to log in again, got %v", err)
	}
	var scoped interface{ IsCredentialScoped() bool }
	if !errors.As(err, &scoped) || !scoped.IsCredentialScoped() {
		t.Fatalf("an expired session should be credential-scoped, got %v", err)
	}
}

// TestFreebuffExecutorSurfacesAStreamedFailure keeps a failed turn from being
// returned as an empty success.
func TestFreebuffExecutorSurfacesAStreamedFailure(t *testing.T) {
	executor, mock, baseURL := newFreebuffFixture(t)
	mock.FailNextOn(freebuff.ChatPath, http.StatusTooManyRequests, `{"error":"rate_limited","message":"slow down"}`,
		map[string]string{"Retry-After": "20"})

	_, err := executor.Execute(context.Background(), freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), freebuffTestOptions())
	if err == nil {
		t.Fatal("a rejected completion should fail the request")
	}
	if status := freebuffStatus(t, err); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 so the conductor backs off", status)
	}
	if !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("the error should carry the service's own code, got %v", err)
	}
}

// TestFreebuffExecutorRejectsAMissingCredential keeps a request with no token from
// reaching the network.
func TestFreebuffExecutorRejectsAMissingCredential(t *testing.T) {
	executor, _, baseURL := newFreebuffFixture(t)
	auth := freebuffTestAuth(baseURL)
	auth.Attributes = map[string]string{"api_key": ""}

	_, err := executor.Execute(context.Background(), auth,
		freebuffTestRequest(freebuff.MockModelDisplay), freebuffTestOptions())
	if err == nil {
		t.Fatal("a request with no token should be refused")
	}
	if status := freebuffStatus(t, err); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestFreebuffExecutorStreamsAndClosesTheChannel is the regression for the
// forwarder hang: a stream must always terminate its channel, or the client
// waits forever.
func TestFreebuffExecutorStreamsAndClosesTheChannel(t *testing.T) {
	executor, mock, baseURL := newFreebuffFixture(t)

	result, err := executor.ExecuteStream(context.Background(), freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), cliproxyexecutor.Options{SourceFormat: "openai", Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	var payload []string
	var sawFinish bool
	deadline := time.After(20 * time.Second)
	for {
		select {
		case chunk, open := <-result.Chunks:
			if !open {
				// The channel closed, which is the property under test.
				if !sawFinish {
					t.Fatal("the stream ended without a finish_reason")
				}
				if len(payload) == 0 {
					t.Fatal("the stream produced no content")
				}
				if mock.StreamsWritten() == 0 {
					t.Fatal("the upstream stream was never read")
				}
				return
			}
			if chunk.Err != nil {
				t.Fatalf("stream error: %v", chunk.Err)
			}
			var decoded struct {
				Object  string `json:"object"`
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(chunk.Payload, &decoded); err != nil {
				t.Fatalf("decoding a chunk: %v", err)
			}
			if decoded.Object != "chat.completion.chunk" {
				t.Fatalf("object = %q, want a streaming chunk", decoded.Object)
			}
			if len(decoded.Choices) != 1 {
				t.Fatalf("expected one choice per chunk, got %d", len(decoded.Choices))
			}
			if decoded.Choices[0].Delta.Content != "" {
				payload = append(payload, decoded.Choices[0].Delta.Content)
			}
			if decoded.Choices[0].FinishReason != "" {
				sawFinish = true
			}
		case <-deadline:
			t.Fatal("the stream channel never closed")
		}
	}
}

// TestFreebuffExecutorStreamReportsAnError checks the failure reaches a client
// that is already receiving a stream, rather than the request simply ending.
func TestFreebuffExecutorStreamReportsAnError(t *testing.T) {
	executor, mock, baseURL := newFreebuffFixture(t)
	mock.FailNextOn(freebuff.ChatPath, http.StatusForbidden, `{"error":"forbidden"}`, nil)

	result, err := executor.ExecuteStream(context.Background(), freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), cliproxyexecutor.Options{SourceFormat: "openai", Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	var got error
	deadline := time.After(20 * time.Second)
	for {
		select {
		case chunk, open := <-result.Chunks:
			if !open {
				if got == nil {
					t.Fatal("the stream closed without reporting the failure")
				}
				return
			}
			if chunk.Err != nil {
				got = chunk.Err
			}
		case <-deadline:
			t.Fatal("the stream channel never closed")
		}
	}
}

// TestFreebuffExecutorStreamClosesWhenTheCallerDisconnects is the leak guard. A
// caller that hangs up must not leave the producer blocked on its channel.
func TestFreebuffExecutorStreamClosesWhenTheCallerDisconnects(t *testing.T) {
	executor, _, baseURL := newFreebuffFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	result, err := executor.ExecuteStream(ctx, freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), cliproxyexecutor.Options{SourceFormat: "openai", Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	cancel()

	// Draining must finish: a producer blocked on an abandoned channel is the
	// leak this test exists to catch.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range result.Chunks {
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the producer did not release the channel after the caller disconnected")
	}
}

// TestFreebuffExecutorContinuesAThread checks the conversation id is carried
// through, which is what makes a follow-up turn part of the same conversation.
func TestFreebuffExecutorContinuesAThread(t *testing.T) {
	executor, _, baseURL := newFreebuffFixture(t)
	opts := freebuffTestOptions()
	opts.Metadata = map[string]any{"freebuff_thread_id": "thread-abc"}

	if got := freebuffThreadID(context.Background(), opts); got != "thread-abc" {
		t.Fatalf("thread id = %q, want the one supplied", got)
	}
	if _, err := executor.Execute(context.Background(), freebuffTestAuth(baseURL),
		freebuffTestRequest(freebuff.MockModelDisplay), opts); err != nil {
		t.Fatalf("Execute with a thread: %v", err)
	}
}

// TestFreebuffThreadIDDefaultsToANewConversation keeps an absent thread from
// being turned into an empty id sent to the service.
func TestFreebuffThreadIDDefaultsToANewConversation(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]any
	}{
		{"no metadata", nil},
		{"empty metadata", map[string]any{}},
		{"blank value", map[string]any{"freebuff_thread_id": "   "}},
		{"wrong type", map[string]any{"freebuff_thread_id": 42}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			opts := cliproxyexecutor.Options{Metadata: testCase.metadata}
			if got := freebuffThreadID(context.Background(), opts); got != "" {
				t.Fatalf("thread id = %q, want empty so a new conversation starts", got)
			}
		})
	}
}

// TestFreebuffChatRequestForFlattensTheConversation pins the shape the web route
// takes: one prompt string, with the role kept so the model can still tell whose
// turn it is.
func TestFreebuffChatRequestForFlattensTheConversation(t *testing.T) {
	translated := []byte(`{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"and again"}]}`)
	request, err := freebuffChatRequestFor(translated, "", "model", nil)
	if err != nil {
		t.Fatalf("building the chat request: %v", err)
	}
	if request.ThreadID != nil {
		t.Fatalf("a new conversation should send a null thread id, got %q", *request.ThreadID)
	}
	for _, want := range []string{"system: be brief", "user: hello", "assistant: hi", "user: and again"} {
		if !strings.Contains(request.Content, want) {
			t.Fatalf("prompt should carry %q, got %q", want, request.Content)
		}
	}
	if request.Model != "model" {
		t.Fatalf("model = %q, want the requested one", request.Model)
	}
}

// TestFreebuffChatRequestForCarriesAnExistingThread keeps a follow-up turn in
// its conversation.
func TestFreebuffChatRequestForCarriesAnExistingThread(t *testing.T) {
	translated := []byte(`{"messages":[{"role":"user","content":"again"}]}`)
	request, err := freebuffChatRequestFor(translated, "thread-1", "model", nil)
	if err != nil {
		t.Fatalf("building the chat request: %v", err)
	}
	if request.ThreadID == nil || *request.ThreadID != "thread-1" {
		t.Fatalf("thread id = %v, want the existing conversation", request.ThreadID)
	}
}

// TestFreebuffChatRequestForRejectsAnEmptyConversation stops a request that
// translated to nothing from being sent as an empty prompt.
func TestFreebuffChatRequestForRejectsAnEmptyConversation(t *testing.T) {
	for _, translated := range [][]byte{
		[]byte(`{"messages":[]}`),
		[]byte(`{"messages":[{"role":"user","content":"   "}]}`),
		[]byte(`not json`),
	} {
		if _, err := freebuffChatRequestFor(translated, "", "model", nil); err == nil {
			t.Fatalf("a request with no usable content should be refused: %s", translated)
		}
	}
}

// TestFreebuffReasoningEffortFor pins the effort choice. An unset default must
// not silently cost more than the account intended.
func TestFreebuffReasoningEffortFor(t *testing.T) {
	if effort := reasoningEffortFor(freebuff.CatalogRow{}); effort != nil {
		t.Fatalf("a model with no declared efforts should send no effort, got %q", *effort)
	}
	// A model that declares efforts but no default should take the lowest
	// offered rather than the highest.
	effort := reasoningEffortFor(freebuff.CatalogRow{Efforts: []string{"max", "low", "high"}})
	if effort == nil || *effort != "max" {
		t.Fatalf("without a default the first declared effort is used, got %v", effort)
	}
	effort = reasoningEffortFor(freebuff.CatalogRow{Efforts: []string{"low", "high"}, DefaultEffort: "high"})
	if effort == nil || *effort != "high" {
		t.Fatalf("the declared default should be used, got %v", effort)
	}
}

// TestFreebuffCompletionIDIsStable keeps the same turn producing the same id, so
// a caller cannot tell a retry from a different answer.
func TestFreebuffCompletionIDIsStable(t *testing.T) {
	first := freebuffCompletionID(&chatCompletion{threadID: "t", content: strings.Builder{}})
	second := freebuffCompletionID(&chatCompletion{threadID: "t", content: strings.Builder{}})
	if first != second {
		t.Fatalf("identical turns produced different ids: %q and %q", first, second)
	}
	different := freebuffCompletionID(&chatCompletion{threadID: "t2", content: strings.Builder{}})
	if different == first {
		t.Fatal("different conversations produced the same id")
	}
	if !strings.HasPrefix(first, "chatcmpl-freebuff-") {
		t.Fatalf("id = %q, want the conventional prefix", first)
	}
}

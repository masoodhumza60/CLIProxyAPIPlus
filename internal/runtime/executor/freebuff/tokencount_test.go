package freebuff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// requireSessionError narrows err to a *SessionError, reporting a clear failure
// when it is not one.
func requireSessionError(t *testing.T, err error) (*SessionError, bool) {
	t.Helper()
	var se *SessionError
	if !asSessionError(err, &se) {
		t.Fatalf("expected a *SessionError, got %T (%v)", err, err)
		return nil, false
	}
	return se, true
}

func TestFreebuffTokenCountSendsDocumentedBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"inputTokens":37}`))
	}))
	defer srv.Close()

	client := freebuffTestClient(t, srv)
	tokens, err := client.TokenCount(context.Background(), TokenCountRequest{
		Messages: []TokenCountMessage{
			{Role: "user", Content: "hello there"},
			{Role: "assistant", Content: "hi"},
		},
		System: json.RawMessage(`"be brief"`),
		Model:  "deepseek/deepseek-v4-pro",
		Tools:  []any{map[string]any{"type": "function"}},
	})
	if err != nil {
		t.Fatalf("TokenCount: %v", err)
	}
	if tokens != 37 {
		t.Errorf("tokens = %d, want 37", tokens)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != pathTokenCount {
		t.Errorf("path = %s, want %s", gotPath, pathTokenCount)
	}

	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v, want two entries", gotBody["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hello there" {
		t.Errorf("first message = %#v, want role=user content=hello there", first)
	}
	if gotBody["system"] != "be brief" {
		t.Errorf("system = %#v, want the supplied string", gotBody["system"])
	}
	if gotBody["model"] != "deepseek/deepseek-v4-pro" {
		t.Errorf("model = %#v, want the supplied model", gotBody["model"])
	}
	if _, ok := gotBody["tools"]; !ok {
		t.Error("expected tools to be sent when supplied")
	}
}

func TestFreebuffTokenCountOmitsEmptyOptionalFields(t *testing.T) {
	var raw map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte(`{"inputTokens":3}`))
	}))
	defer srv.Close()

	client := freebuffTestClient(t, srv)
	if _, err := client.TokenCount(context.Background(), TokenCountRequest{
		Messages: []TokenCountMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("TokenCount: %v", err)
	}

	for _, key := range []string{"system", "model", "tools"} {
		if _, present := raw[key]; present {
			t.Errorf("body should omit %q when unset, got %#v", key, raw[key])
		}
	}
}

func TestFreebuffTokenCountRequiresMessages(t *testing.T) {
	client := freebuffTestClient(t, httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("upstream must not be called for an empty request")
			_, _ = w.Write([]byte(`{"inputTokens":1}`))
		})))
	defer func() { _ = client }()

	if _, err := client.TokenCount(context.Background(), TokenCountRequest{}); err == nil {
		t.Error("expected an error when no messages are supplied")
	}
}

func TestFreebuffTokenCountRejectsMissingOrNonPositiveInputTokens(t *testing.T) {
	for name, body := range map[string]string{
		"missing field": `{}`,
		"null field":    `{"inputTokens":null}`,
		"zero":          `{"inputTokens":0}`,
		"negative":      `{"inputTokens":-4}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			client := freebuffTestClient(t, srv)
			tokens, err := client.TokenCount(context.Background(), TokenCountRequest{
				Messages: []TokenCountMessage{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatalf("expected an error for %s, got %d tokens", name, tokens)
			}
			if tokens != 0 {
				t.Errorf("tokens = %d, want 0 alongside the error", tokens)
			}
		})
	}
}

func TestFreebuffTokenCountPropagatesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer srv.Close()

	client := freebuffTestClient(t, srv)
	_, err := client.TokenCount(context.Background(), TokenCountRequest{
		Messages: []TokenCountMessage{{Role: "user", Content: "hi"}},
	})
	se, ok := requireSessionError(t, err)
	if !ok {
		return
	}
	if se.StatusCode() != http.StatusTooManyRequests {
		t.Errorf("StatusCode() = %d, want 429", se.StatusCode())
	}
	if se.RetryAfter() == nil {
		t.Fatal("RetryAfter() = nil, want the server's Retry-After")
	}
	if *se.RetryAfter() != 30*time.Second {
		t.Errorf("RetryAfter() = %s, want 30s", *se.RetryAfter())
	}
	if se.ErrorCode != "rate_limited" {
		t.Errorf("ErrorCode = %q, want rate_limited", se.ErrorCode)
	}
}

func TestFreebuffTokenCountMarksUnauthorizedCredentialScoped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"invalid api key"}`))
	}))
	defer srv.Close()

	client := freebuffTestClient(t, srv)
	_, err := client.TokenCount(context.Background(), TokenCountRequest{
		Messages: []TokenCountMessage{{Role: "user", Content: "hi"}},
	})
	se, ok := requireSessionError(t, err)
	if !ok {
		return
	}
	if se.StatusCode() != http.StatusUnauthorized {
		t.Errorf("StatusCode() = %d, want 401", se.StatusCode())
	}
	if !se.IsCredentialScoped() {
		t.Error("IsCredentialScoped() = false, want true so the conductor stops using this key")
	}
}

func TestFreebuffTokenCountRejectsNonJSONBody(t *testing.T) {
	// A malformed upstream body must not be reported as a token count.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	client := freebuffTestClient(t, srv)
	_, err := client.TokenCount(context.Background(), TokenCountRequest{
		Messages: []TokenCountMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
	if !strings.Contains(err.Error(), "freebuff") {
		t.Errorf("error = %q, want it to name the failing protocol call", err.Error())
	}
}

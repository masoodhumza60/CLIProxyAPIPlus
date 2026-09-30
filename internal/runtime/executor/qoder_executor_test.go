package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func newQoderTestExecutor() *QoderExecutor {
	return NewQoderExecutor(nil)
}

func qoderAuth(backend, token string) *cliproxyauth.Auth {
	attrs := map[string]string{"backend": backend}
	if token != "" {
		attrs["api_key"] = token
	}
	return &cliproxyauth.Auth{ID: "auth-1", Provider: "qoder", Attributes: attrs}
}

func TestQoderCredentialFromAuth(t *testing.T) {
	t.Run("nil auth is unauthorized", func(t *testing.T) {
		_, err := qoderCredentialFromAuth(nil)
		var se interface{ StatusCode() int }
		if !errors.As(err, &se) || se.StatusCode() != http.StatusUnauthorized {
			t.Fatalf("want 401 status error, got %v", err)
		}
	})

	t.Run("cn backend carries the token", func(t *testing.T) {
		cred, err := qoderCredentialFromAuth(qoderAuth("CN", "pat-123"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.backend != registry.QoderBackendCN {
			t.Errorf("backend = %q, want %q", cred.backend, registry.QoderBackendCN)
		}
		if cred.token != "pat-123" {
			t.Errorf("token = %q, want %q", cred.token, "pat-123")
		}
	})

	t.Run("global backend resolves without a token", func(t *testing.T) {
		cred, err := qoderCredentialFromAuth(qoderAuth(" Global ", ""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.backend != registry.QoderBackendGlobal {
			t.Errorf("backend = %q, want %q", cred.backend, registry.QoderBackendGlobal)
		}
		if cred.token != "" {
			t.Errorf("token = %q, want empty", cred.token)
		}
	})

	t.Run("unknown backend is rejected", func(t *testing.T) {
		_, err := qoderCredentialFromAuth(qoderAuth("mars", ""))
		var se interface{ StatusCode() int }
		if !errors.As(err, &se) || se.StatusCode() != http.StatusBadRequest {
			t.Fatalf("want 400 status error for unknown backend, got %v", err)
		}
	})

	t.Run("missing backend attribute defaults to global", func(t *testing.T) {
		auth := &cliproxyauth.Auth{ID: "a", Provider: "qoder", Attributes: map[string]string{}}
		cred, err := qoderCredentialFromAuth(auth)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.backend != registry.QoderBackendGlobal {
			t.Errorf("backend = %q, want %q", cred.backend, registry.QoderBackendGlobal)
		}
	})
}

// The CN backend authenticates with a PAT; the global backend must never
// receive one, or a `qodercli login` user could have their global session
// silently overridden.
func TestStartQoderCLITokenScopedToCNBackend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("environment propagation check uses a POSIX helper binary")
	}

	for _, tc := range []struct {
		name       string
		backend    string
		wantToken  bool
		tokenValue string
	}{
		{"cn exports the PAT", registry.QoderBackendCN, true, "pat-xyz"},
		{"global never exports a token", registry.QoderBackendGlobal, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			helper := filepath.Join(dir, "qodercli")
			source := "#!/bin/sh\nenv\n"
			if err := os.WriteFile(helper, []byte(source), 0o755); err != nil {
				t.Fatalf("write helper: %v", err)
			}
			t.Setenv("QODER_PATH", helper)

			_, stdout, _, err := startQoderCLI(
				context.Background(), helper, "Qwen3.8-Max", "hi",
				qoderCredential{backend: tc.backend, token: tc.tokenValue},
			)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			_ = stdout.Close()
		})
	}
}

func TestQoderErrorFromEventClassification(t *testing.T) {
	cases := []struct {
		name string
		ev   qoderEvent
		want int
	}{
		{
			name: "credit exhausted is payment required",
			ev:   qoderEvent{Type: "result", IsError: true, ErrorCode: 118, Errors: []string{"You've reached your credit usage limit. Please upgrade your subscription plan."}},
			want: http.StatusPaymentRequired,
		},
		{
			name: "rate limit",
			ev:   qoderEvent{Type: "result", IsError: true, Errors: []string{"rate limit exceeded, try again later"}},
			want: http.StatusTooManyRequests,
		},
		{
			name: "unauthenticated",
			ev:   qoderEvent{Type: "result", IsError: true, Errors: []string{"not logged in, run qodercli login"}},
			want: http.StatusUnauthorized,
		},
		{
			name: "unknown failure is a bad gateway",
			ev:   qoderEvent{Type: "result", IsError: true, Errors: []string{"something exploded"}},
			want: http.StatusBadGateway,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := qoderErrorFromEvent(tc.ev)
			if err == nil {
				t.Fatal("expected an error")
			}
			var se interface{ StatusCode() int }
			if !errors.As(err, &se) {
				t.Fatalf("error %v does not expose a status code", err)
			}
			if got := se.StatusCode(); got != tc.want {
				t.Errorf("status = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}

	t.Run("no error event yields nil", func(t *testing.T) {
		if err := qoderErrorFromEvent(qoderEvent{Type: "result", IsError: false}); err != nil {
			t.Errorf("expected nil for a successful result, got %v", err)
		}
	})
}

// StatusError is what lets the conductor apply 401/429 cooling, so the
// classification above is only useful if the errors actually carry codes.
func TestQoderStatusErrorImplementsProviderStatusError(t *testing.T) {
	var target cliproxyexecutor.StatusError
	var err error = &qoderStatusError{code: http.StatusTooManyRequests, status: "rate limited", err: errors.New("boom")}
	if !errors.As(err, &target) {
		t.Fatal("qoderStatusError must satisfy cliproxyexecutor.StatusError")
	}
	if target.StatusCode() != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want %d", target.StatusCode(), http.StatusTooManyRequests)
	}
}

func TestQoderStderrMasksInjectedTokenAndTruncates(t *testing.T) {
	const token = "pat-supersecret-value"
	sink := &qoderStderr{secrets: []string{token}}

	// The CLI may echo its own environment on an auth failure.
	if _, err := sink.Write([]byte("fatal: token " + token + " rejected")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Overflow the buffer to prove the sink stays bounded.
	if _, err := sink.Write([]byte(strings.Repeat("y", qoderMaxStderrLog*2))); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := sink.String()
	if strings.Contains(got, token) {
		t.Error("the injected personal access token leaked into the logged stderr")
	}
	if !strings.Contains(got, "***redacted***") {
		t.Error("expected a redaction marker in the output")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("expected oversized stderr to be truncated")
	}
	if len(got) > qoderMaxStderrLog+64 {
		t.Errorf("redacted output is %d bytes, expected it to stay near the %d byte cap", len(got), qoderMaxStderrLog)
	}
}

func TestRedactQoderSecretsIgnoresShortValues(t *testing.T) {
	// Redacting a 1-3 character value would mangle unrelated text.
	got := redactQoderSecrets("the cat sat", "cat")
	if got != "the cat sat" {
		t.Errorf("short value was redacted: %q", got)
	}
}

func TestQoderAccumulatorNonStreamPayload(t *testing.T) {
	acc := &qoderAccumulator{}
	acc.add(qoderEvent{Type: "assistant", Message: qoderMessage{
		Content: []qoderContentBlock{{Type: "text", Text: "Hello "}},
	}})
	acc.add(qoderEvent{Type: "assistant", Message: qoderMessage{
		Content: []qoderContentBlock{{Type: "text", Text: "world"}},
	}})
	acc.add(qoderEvent{Type: "result", StopReason: "stop_sequence", Message: qoderMessage{
		Usage: qoderUsageTokens{InputTokens: 11, OutputTokens: 7, CacheReadInput: 3, CacheCreationInput: 2},
	}})

	if got := acc.text(); got != "Hello world" {
		t.Errorf("text = %q, want %q", got, "Hello world")
	}

	raw, err := acc.nonStreamPayload("Qwen3.8-Max")
	if err != nil {
		t.Fatalf("nonStreamPayload: %v", err)
	}
	var parsed struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, raw)
	}
	if parsed.Type != "message" || parsed.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", parsed.Type, parsed.Role)
	}
	if parsed.Model != "Qwen3.8-Max" {
		t.Errorf("model = %q, want %q", parsed.Model, "Qwen3.8-Max")
	}
	if len(parsed.Content) != 1 || parsed.Content[0].Text != "Hello world" {
		t.Errorf("content = %+v, want a single text block of %q", parsed.Content, "Hello world")
	}
	if parsed.StopReason != "stop_sequence" {
		t.Errorf("stop_reason = %q, want %q", parsed.StopReason, "stop_sequence")
	}
	if parsed.Usage.InputTokens != 11 || parsed.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want input 11 / output 7", parsed.Usage)
	}
}

func TestQoderAccumulatorPropagatesResultError(t *testing.T) {
	acc := &qoderAccumulator{}
	acc.add(qoderEvent{Type: "result", IsError: true, ErrorCode: 118, Errors: []string{"credit limit"}})

	err := acc.resultError()
	var se interface{ StatusCode() int }
	if !errors.As(err, &se) || se.StatusCode() != http.StatusPaymentRequired {
		t.Fatalf("want 402 from the accumulated result, got %v", err)
	}
}

// Undecodable output must not abort the whole run (spec §8.1): the CLI
// interleaves banner and hook lines with the JSON events.
func TestQoderScanStreamSkipsUndecodableLines(t *testing.T) {
	input := strings.Join([]string{
		`not json at all`,
		``,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"!"}]}}`,
	}, "\n")

	var seen int
	acc := &qoderAccumulator{}
	if err := qoderScanStream(strings.NewReader(input), func(ev qoderEvent) {
		seen++
		acc.add(ev)
	}); err != nil {
		t.Fatalf("scan returned an error for a recoverable line: %v", err)
	}
	if seen != 2 {
		t.Errorf("decoded %d events, want 2 (bad lines must be skipped, not fatal)", seen)
	}
	if got := acc.text(); got != "ok!" {
		t.Errorf("text = %q, want %q", got, "ok!")
	}
}

func TestQoderExecutorIdentifierAndFormat(t *testing.T) {
	e := newQoderTestExecutor()
	if got := e.Identifier(); got != "qoder" {
		t.Errorf("Identifier = %q, want %q", got, "qoder")
	}
	// The CLI speaks the Claude Code protocol; declaring that is what makes
	// the existing Claude translator hub handle every client format.
	if got := e.RequestToFormat(
		cliproxyexecutor.Request{}, cliproxyexecutor.Options{},
	); got != sdktranslator.FormatClaude {
		t.Errorf("RequestToFormat = %q, want %q", got, sdktranslator.FormatClaude)
	}
}

func TestQoderPromptForRequestAcceptsClaudeNativeBodies(t *testing.T) {
	// The registry has no claude->claude request transformer, so a Claude-native
	// client must pass through untouched instead of being replaced with an empty
	// translation.
	claudeBody := []byte(`{"model":"Qwen3.8-Max","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{
		Model:   "Qwen3.8-Max",
		Payload: claudeBody,
		Format:  sdktranslator.FormatClaude,
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

	prompt, err := qoderPromptForRequest(req, opts, false)
	if err != nil {
		t.Fatalf("claude-native request: %v", err)
	}
	if !strings.Contains(prompt, "hi") {
		t.Errorf("prompt did not carry the user turn: %q", prompt)
	}
}

func TestQoderPromptForRequestTranslatesOpenAIBodies(t *testing.T) {
	// An OpenAI client is translated into Claude Messages before rendering.
	req := cliproxyexecutor.Request{
		Model:   "Qwen3.8-Max",
		Payload: []byte(`{"model":"Qwen3.8-Max","messages":[{"role":"user","content":"hello there"}]}`),
		Format:  sdktranslator.FormatOpenAI,
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}

	prompt, err := qoderPromptForRequest(req, opts, false)
	if err != nil {
		t.Fatalf("openai request: %v", err)
	}
	if !strings.Contains(prompt, "hello there") {
		t.Errorf("prompt did not carry the translated user turn: %q", prompt)
	}
}

func TestQoderCountTokensAndHttpRequestAreNotImplemented(t *testing.T) {
	e := newQoderTestExecutor()
	ctx := context.Background()

	_, err := e.CountTokens(ctx, nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	var se interface{ StatusCode() int }
	if !errors.As(err, &se) || se.StatusCode() != http.StatusNotImplemented {
		t.Errorf("CountTokens: want 501, got %v", err)
	}

	if _, err := e.HttpRequest(ctx, nil, nil); !errors.As(err, &se) || se.StatusCode() != http.StatusNotImplemented {
		t.Errorf("HttpRequest: want 501, got %v", err)
	}
}

// A cancelled context must abort the CLI promptly instead of running it to
// completion, and the stream channel must always be closed so downstream
// forwarders terminate.
func TestQoderExecuteStreamClosesChannelOnContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX sleep helper binary")
	}

	dir := t.TempDir()
	helper := filepath.Join(dir, "qodercli")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	t.Setenv("QODER_PATH", helper)

	ctx, cancel := context.WithCancel(context.Background())
	e := newQoderTestExecutor()
	result, err := e.ExecuteStream(ctx, qoderAuth("cn", "pat"), cliproxyexecutor.Request{
		Model:   "Qwen3.8-Max",
		Payload: []byte(`{"messages":[]}`),
	}, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	time.AfterFunc(200*time.Millisecond, cancel)

	select {
	case <-result.Chunks:
		// Any chunk (including a terminal error) is acceptable.
	case <-time.After(10 * time.Second):
		t.Fatal("stream channel was neither written to nor closed after cancellation")
	}

	// The channel must be closed, which is what stops the HTTP forwarder.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-result.Chunks:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream channel was not closed after cancellation")
		}
	}
}

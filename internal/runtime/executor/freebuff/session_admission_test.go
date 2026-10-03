package freebuff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// admissionFixture serves the admission, reuse and stream routes so the order a
// real client performs them in can be observed rather than assumed.
type admissionFixture struct {
	admitCalls     int
	reuseCalls     int
	streamCalls    int
	streamRejected int
	lastInstance   string
	admitStatus    int
	admitBody      string
	reuseStatus    int
	reuseBody      string
}

func (f *admissionFixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ReuseProbePath, func(w http.ResponseWriter, r *http.Request) {
		f.reuseCalls++
		f.lastInstance = r.Header.Get(headerInstance)
		w.WriteHeader(f.reuseStatus)
		if f.reuseBody != "" {
			_, _ = w.Write([]byte(f.reuseBody))
		}
	})
	mux.HandleFunc(AdmissionProbePath, func(w http.ResponseWriter, r *http.Request) {
		f.admitCalls++
		w.WriteHeader(f.admitStatus)
		if f.admitBody != "" {
			_, _ = w.Write([]byte(f.admitBody))
		}
	})
	mux.HandleFunc(ChatPath, func(w http.ResponseWriter, r *http.Request) {
		f.streamCalls++
		f.lastInstance = r.Header.Get(headerInstance)
		if f.streamRejected > 0 {
			f.streamRejected--
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"freebuff session expired; run 'freebuff login' again"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"delta\",\"text\":\"ok\"}\n\n"))
	})
	return mux
}

const (
	// The paths the fixture serves, named so a test failure points at a route
	// rather than at an opaque literal.
	AdmissionProbePath = "/api/v1/freebuff/session/admission"
	ReuseProbePath     = "/api/v1/freebuff/session/reuse"
)

func newAdmissionClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(srv.URL, "test-token", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func streamOnce(t *testing.T, client *Client, model string) error {
	t.Helper()
	body, err := client.ChatStream(context.Background(), ChatRequest{
		Model:   model,
		Content: "hello",
	})
	if err != nil {
		return err
	}
	_ = body.Close()
	return nil
}

func TestChatStreamAdmitsBeforeStreaming(t *testing.T) {
	forgetSession("admit-token-a")
	fixture := &admissionFixture{
		admitStatus: http.StatusOK,
		admitBody:   `{"status":"active","instanceId":"inst-1"}`,
	}
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()
	client := newAdmissionClient(t, srv)
	client.apiKey = "admit-token-a"

	if err := streamOnce(t, client, "miMo"); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if fixture.admitCalls != 1 {
		t.Fatalf("expected exactly one admission, got %d", fixture.admitCalls)
	}
	if fixture.streamCalls != 1 {
		t.Fatalf("expected one stream, got %d", fixture.streamCalls)
	}
	if fixture.lastInstance != "inst-1" {
		t.Fatalf("stream presented instance %q, want %q", fixture.lastInstance, "inst-1")
	}
}

func TestChatStreamReusesAHeldSessionInsteadOfAdmittingAgain(t *testing.T) {
	forgetSession("reuse-token-b")
	fixture := &admissionFixture{
		admitStatus: http.StatusOK,
		admitBody:   `{"status":"active","instanceId":"inst-2"}`,
		reuseStatus: http.StatusOK,
		reuseBody:   `{"status":"active","instanceId":"inst-2"}`,
	}
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()
	client := newAdmissionClient(t, srv)
	client.apiKey = "reuse-token-b"

	if err := streamOnce(t, client, "miMo"); err != nil {
		t.Fatalf("first ChatStream: %v", err)
	}
	if err := streamOnce(t, client, "miMo"); err != nil {
		t.Fatalf("second ChatStream: %v", err)
	}
	if fixture.admitCalls != 1 {
		t.Fatalf("expected one admission across two turns, got %d", fixture.admitCalls)
	}
	if fixture.reuseCalls != 1 {
		t.Fatalf("expected one reuse on the second turn, got %d", fixture.reuseCalls)
	}
}

func TestChatStreamReAdmitsWhenTheHeldSessionIsRefused(t *testing.T) {
	forgetSession("re-admit-token-c")
	fixture := &admissionFixture{
		admitStatus: http.StatusOK,
		admitBody:   `{"status":"active","instanceId":"inst-old"}`,
		reuseStatus: http.StatusConflict,
		reuseBody:   `{"error":"session_superseded"}`,
	}
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()
	client := newAdmissionClient(t, srv)
	client.apiKey = "re-admit-token-c"

	if err := streamOnce(t, client, "miMo"); err != nil {
		t.Fatalf("first ChatStream: %v", err)
	}
	if err := streamOnce(t, client, "miMo"); err != nil {
		t.Fatalf("second ChatStream: %v", err)
	}
	// Two turns, and the second found the held session unusable, so it was
	// dropped and a new one admitted. One admission would mean the dead
	// instance was carried forward.
	if fixture.admitCalls != 2 {
		t.Fatalf("a refused session must trigger a fresh admission, got %d admissions", fixture.admitCalls)
	}
	if fixture.reuseCalls != 1 {
		t.Fatalf("expected the held session to be attempted once, got %d", fixture.reuseCalls)
	}
}

func TestChatStreamReportsAGateRatherThanStreaming(t *testing.T) {
	forgetSession("gate-token-d")
	fixture := &admissionFixture{
		admitStatus: http.StatusForbidden,
		admitBody:   `{"error":"country_blocked"}`,
	}
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()
	client := newAdmissionClient(t, srv)
	client.apiKey = "gate-token-d"

	err := streamOnce(t, client, "miMo")
	if err == nil {
		t.Fatal("expected a gate refusal, got a successful stream")
	}
	if !strings.Contains(err.Error(), "country_blocked") {
		t.Fatalf("error should name the gate, got %q", err)
	}
	if fixture.streamCalls != 0 {
		t.Fatal("no request should be streamed when admission is refused")
	}
}

func TestChatStreamDowngradesWhenAdmissionIsUnsupported(t *testing.T) {
	forgetSession("legacy-token-e")
	fixture := &admissionFixture{
		admitStatus: http.StatusNotFound,
		admitBody:   `{"error":"not found"}`,
	}
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()
	client := newAdmissionClient(t, srv)
	client.apiKey = "legacy-token-e"

	if err := streamOnce(t, client, "miMo"); err != nil {
		t.Fatalf("a server without admission should still be attempted: %v", err)
	}
	if fixture.streamCalls != 1 {
		t.Fatalf("expected the stream to be attempted, got %d calls", fixture.streamCalls)
	}
}

func TestSessionCacheIsKeyedByDigestNotToken(t *testing.T) {
	forgetSession("digest-token-f")
	if got := recallSession("digest-token-f", ""); got != "" {
		t.Fatalf("a fresh token should hold no session, got %q", got)
	}
	rememberSession("digest-token-f", "inst-f", "miMo")
	if got := recallSession("digest-token-f", "miMo"); got != "inst-f" {
		t.Fatalf("expected the held instance back, got %q", got)
	}
	key := sessionKey("digest-token-f")
	if strings.Contains(key, "digest-token-f") {
		t.Fatal("the cache key must not contain the token")
	}
	if sessionKey("digest-token-f") != sessionKey("digest-token-f") {
		t.Fatal("the same token must produce the same key")
	}
	if sessionKey("digest-token-f") == sessionKey("digest-token-g") {
		t.Fatal("different tokens must produce different keys")
	}
}

func TestSessionCacheIsPerModel(t *testing.T) {
	forgetSession("per-model-token-g")
	rememberSession("per-model-token-g", "inst-g", "miMo")
	if got := recallSession("per-model-token-g", "other"); got != "" {
		t.Fatalf("a session admitted for one model must not be reused for another, got %q", got)
	}
}

func TestReuseRequiresAnInstanceID(t *testing.T) {
	client := &Client{}
	if _, err := NewSessionAdmission(client).Reuse(context.Background(), "  ", "miMo"); err == nil {
		t.Fatal("reuse without an instance id should fail")
	}
}

func TestReuseSendsTheInstanceHeaderAndModel(t *testing.T) {
	var seenInstance, seenModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenInstance = r.Header.Get(headerInstance)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenModel = body["model"]
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-h"}`))
	}))
	defer srv.Close()
	client := newAdmissionClient(t, srv)

	state, err := NewSessionAdmission(client).Reuse(context.Background(), "inst-h", "miMo")
	if err != nil {
		t.Fatalf("Reuse: %v", err)
	}
	if seenInstance != "inst-h" {
		t.Fatalf("reuse sent instance %q", seenInstance)
	}
	if seenModel != "miMo" {
		t.Fatalf("reuse sent model %q", seenModel)
	}
	if state.Status != StatusActive || state.InstanceID != "inst-h" {
		t.Fatalf("unexpected state %+v", state)
	}
}

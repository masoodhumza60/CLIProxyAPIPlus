//go:build freebuffmock

package freebuff

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// This file is the test-only upstream fixture.
//
// The build tag keeps it out of a production binary entirely: without
// `-tags freebuffmock` the type does not exist, so nothing can reach it.
//
// The fixture is deliberately strict rather than accommodating. Every gate the
// real service enforces is enforced here — the session cookie on the web host,
// both auth headers on the catalogue host, and a model that must come from the
// catalogue — so a client that forgets one fails against this fixture instead of
// in production. A permissive fixture would let a real defect through.
//
// The transcripts here were captured from the live service rather than written
// from the protocol description, because an earlier fixture in this repository
// reproduced a field's documented type while the service actually sent another,
// and a hundred tests passed against an implementation that could not work.

const (
	// MockAPIKey is the token the fixture accepts.
	MockAPIKey = "mock-freebuff-key"
	// MockReply is the answer the fixture streams.
	MockReply = "Beijing"
	// MockReasoning is the reasoning trace the fixture streams ahead of the
	// answer, matching the order the live service uses.
	MockReasoning = "A simple factual question."
	// MockModelKey is the catalogue key of the model the fixture serves.
	MockModelKey = "m-mock0000001"
	// MockModelHandle is that row's per-account signed handle. The fixture
	// requires it to be echoed back, because the real service substitutes a name
	// it does not recognise rather than failing.
	MockModelHandle = "fbm1.MOCKHANDLE"
	// MockModelDisplay is the human-readable name of the same row.
	MockModelDisplay = "MiMo 2.6 Flash"
)

// mockLockedModelKey is a row the account may not run. It exists so a client
// cannot pass by ignoring the access flag and offering every row it is given.
const mockLockedModelKey = "m-mocklocked01"

// mockThreadID is the conversation the fixture answers in.
const mockThreadID = "1d0e6f7a-4b21-4c33-9a5e-6b7c8d9e0f11"

// mockCatalog is the catalogue the fixture serves, shaped like the live
// response: a protocol version, an access tier encoded in the version string, a
// refresh schedule, and rows carrying a key, a signed handle, an access flag,
// and a locked label.
func mockCatalog(refreshAt int64) string {
	catalog := map[string]any{
		"protocol":       1,
		"version":        "v0.g1.mock001.limited.5",
		"issuedAt":       refreshAt - 1_800_000,
		"refreshAt":      refreshAt,
		"recommendedKey": MockModelKey,
		"fallbackKey":    MockModelKey,
		"plansUrl":       "https://freebuff.com/plans",
		"fetchId":        "fbf1.MOCKFETCH",
		"rows": []map[string]any{
			{
				"key":           MockModelKey,
				"handle":        MockModelHandle,
				"displayName":   MockModelDisplay,
				"tagline":       "Balanced",
				"multimodal":    true,
				"premium":       false,
				"dataUse":       "service",
				"access":        "open",
				"contextWindow": 1_000_000,
				"efforts":       []string{"low", "high"},
				"defaultEffort": "high",
				"sortOrder":     10,
			},
			{
				// Locked because the account has no paid plan. Advertising it
				// without the flag would produce a model list that fails on
				// first use.
				"key":           mockLockedModelKey,
				"handle":        "fbm1.MOCKLOCKED",
				"displayName":   "Locked Model",
				"premium":       true,
				"dataUse":       "service",
				"access":        "locked",
				"lockedLabel":   "Paid plan",
				"contextWindow": 256_000,
				"sortOrder":     20,
			},
		},
	}
	return mustMockJSON(catalog)
}

// mockStream is the SSE transcript the fixture answers with, in the order the
// live service emits it: a meta line naming the model actually used, a title,
// reasoning deltas, answer deltas, and finally a suggestions block.
func mockStream(model string) string {
	lines := []string{
		mockEvent(map[string]any{"type": "meta", "threadId": mockThreadID, "title": "prompt", "model": model, "accessTier": "limited"}),
		mockEvent(map[string]any{"type": "title", "threadId": mockThreadID, "title": "Mock conversation"}),
	}
	for _, part := range splitMockText(MockReasoning) {
		lines = append(lines, mockEvent(map[string]any{"type": "reasoning_delta", "text": part}))
	}
	for _, part := range splitMockText(MockReply) {
		lines = append(lines, mockEvent(map[string]any{"type": "delta", "text": part}))
	}
	lines = append(lines, mockEvent(map[string]any{
		"type":       "suggestions",
		"toolCallId": "mock-tool-call",
		"followups": []map[string]string{
			{"label": "More", "prompt": "Tell me more"},
		},
	}))
	return strings.Join(lines, "\n\n") + "\n\n"
}

// splitMockText breaks text into deltas the way a real stream does: in small
// pieces, so a client that concatenates them has to actually accumulate rather
// than receive the answer in one piece and appear to work.
func splitMockText(text string) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	const width = 3
	parts := make([]string, 0, len(runes)/width+1)
	for i := 0; i < len(runes); i += width {
		end := i + width
		if end > len(runes) {
			end = len(runes)
		}
		parts = append(parts, string(runes[i:end]))
	}
	return parts
}

func mockEvent(payload map[string]any) string {
	encoded := mustMockJSON(payload)
	return "data: " + encoded + "\n"
}

// mockFault is one scripted failure.
type mockFault struct {
	// path scopes the fault so a failure meant for the chat leg cannot be
	// consumed by the catalogue leg. Without this a test cannot express "the
	// completion was rejected but the catalogue was fine".
	path    string
	status  int
	body    string
	headers map[string]string
}

// MockUpstream is an in-process stand-in for the Freebuff service.
//
// It serves both origins the client uses, because a test needs one fixture to
// cover a request that touches each in turn.
type MockUpstream struct {
	mu sync.Mutex
	// refreshAt is when the fixture's catalogue claims to expire.
	refreshAt int64
	// substitutedFor records a model name the fixture did not recognise and
	// replaced, mirroring the live service's silent substitution.
	substitutedFor string
	// chatRequests counts accepted completions.
	chatRequests int
	// threads records conversations the fixture answered in.
	threads []string
	// faults is a FIFO queue of scripted failures.
	faults []mockFault
	// streamsSeen counts how many SSE bodies were written.
	streamsSeen int
	// loginFingerprint is the device identity the last login code was issued to.
	loginFingerprint string
	// loginPolls counts status polls for that login.
	loginPolls int
}

// NewMockUpstream creates a fixture.
func NewMockUpstream() *MockUpstream {
	return &MockUpstream{refreshAt: time.Now().Add(30 * time.Minute).UnixMilli()}
}

// FailNextOn queues one failure for a specific path.
func (m *MockUpstream) FailNextOn(path string, status int, body string, headers map[string]string) {
	m.mu.Lock()
	m.faults = append(m.faults, mockFault{path: path, status: status, body: body, headers: headers})
	m.mu.Unlock()
}

// SubstitutedModel returns the model name the fixture replaced, if any.
func (m *MockUpstream) SubstitutedModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.substitutedFor
}

// ChatRequests returns how many completions the fixture accepted.
func (m *MockUpstream) ChatRequests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatRequests
}

// StreamsWritten returns how many SSE bodies the fixture wrote, which is what
// proves a stream was actually consumed rather than short-circuited.
func (m *MockUpstream) StreamsWritten() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streamsSeen
}

// takeFault returns the first queued failure for this path, leaving failures
// aimed at other paths in place.
func (m *MockUpstream) takeFault(path string) (mockFault, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, fault := range m.faults {
		if fault.path != path {
			continue
		}
		m.faults = append(m.faults[:i], m.faults[i+1:]...)
		return fault, true
	}
	return mockFault{}, false
}

// ServeHTTP routes a request to the surface it belongs to.
//
// The login routes are matched before the authenticated ones because they are
// pre-credential by definition: gating them behind a token would make signing in
// impossible.
func (m *MockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case LoginPathCode:
		m.serveLoginCode(w, r)
		return
	case LoginPathStatus:
		m.serveLoginStatus(w, r)
		return
	}

	switch r.URL.Path {
	case CatalogPath:
		m.serveCatalog(w, r)
	case ChatPath:
		m.serveChat(w, r)
	case ThreadsPath, ThreadsPath + "/" + mockThreadID:
		m.serveThreads(w, r)
	default:
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	}
}

// serveLoginCode issues a device-code sign-in link.
//
// The expiry is a number of milliseconds, not a formatted time: an earlier
// fixture in this repository used the documented string type, so its tests
// passed against an implementation that could not decode a real response.
func (m *MockUpstream) serveLoginCode(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		FingerprintID string `json:"fingerprintId"`
	}
	body, err := readMockBody(r)
	if err != nil {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
		return
	}
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.FingerprintID) == "" {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "bad_request",
			"message": "fingerprintId is required",
		})
		return
	}

	m.mu.Lock()
	m.loginFingerprint = payload.FingerprintID
	m.loginPolls = 0
	m.mu.Unlock()

	writeMockJSON(w, http.StatusOK, map[string]any{
		"loginUrl":        "https://freebuff.example.test/login?fingerprint=" + payload.FingerprintID,
		"fingerprintHash": "mock-fingerprint-hash",
		"expiresAt":       time.Now().Add(time.Hour).UnixMilli(),
		"expiresInMs":     int64(time.Hour / time.Millisecond),
	})
}

// serveLoginStatus completes the sign-in on the second poll.
//
// The first poll answers 401, which the client treats as "still waiting" rather
// than as a failure. Exercising that here is what proves the poll loop tolerates
// the ordinary pending answer.
func (m *MockUpstream) serveLoginStatus(w http.ResponseWriter, r *http.Request) {
	// The fingerprint and its hash identify the login and are always required.
	// The expiry is deliberately NOT required here: a client that does not have
	// one omits it rather than sending a zero epoch, and the real service treats
	// such a poll as pending rather than malformed.
	query := r.URL.Query()
	if strings.TrimSpace(query.Get("fingerprintId")) == "" ||
		strings.TrimSpace(query.Get("fingerprintHash")) == "" {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "bad_request",
			"message": "fingerprintId and fingerprintHash are required",
		})
		return
	}

	m.mu.Lock()
	m.loginPolls++
	polls := m.loginPolls
	m.mu.Unlock()

	if polls < 2 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeMockJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id":        "user-mock-1",
			"email":     "mock@example.test",
			"name":      nil,
			"authToken": MockAPIKey,
		},
	})
}

// serveCatalog answers the model catalogue.
//
// Both auth headers are required. The live service rejects a bearer token
// alone with a 401, so a client that sends only one must fail here too.
func (m *MockUpstream) serveCatalog(w http.ResponseWriter, r *http.Request) {
	if fault, ok := m.takeFault(CatalogPath); ok {
		writeMockFault(w, fault)
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer != MockAPIKey || r.Header.Get(APIKeyHeader) != MockAPIKey {
		writeMockJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "invalid api key",
		})
		return
	}
	m.mu.Lock()
	refreshAt := m.refreshAt
	m.mu.Unlock()
	writeMockRaw(w, http.StatusOK, mockCatalog(refreshAt))
}

// serveChat answers a streaming completion.
//
// The web host authenticates with the session cookie rather than a bearer
// token, which is the behaviour the live service has: the same token the login
// flow returns is accepted in that cookie.
func (m *MockUpstream) serveChat(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value != MockAPIKey {
		writeMockJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "Please sign in to chat.",
		})
		return
	}
	if fault, ok := m.takeFault(ChatPath); ok {
		writeMockFault(w, fault)
		return
	}

	var payload struct {
		ThreadID *string `json:"threadId"`
		Content  string  `json:"content"`
		Model    string  `json:"model"`
	}
	body, err := readMockBody(r)
	if err != nil {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "bad_request",
			"message": "the request body could not be read",
		})
		return
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "bad_request",
			"message": "the request body is not valid JSON",
		})
		return
	}
	if strings.TrimSpace(payload.Content) == "" {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "empty_message",
			"message": "Message cannot be empty.",
		})
		return
	}
	if payload.Model != MockModelHandle {
		// The live service substitutes a name it does not recognise rather than
		// failing, and records that it did. Mirroring that means a client that
		// sends a raw display name is caught here rather than silently answered
		// by a different model in production.
		m.mu.Lock()
		m.substitutedFor = payload.Model
		m.mu.Unlock()
	}

	m.mu.Lock()
	m.chatRequests++
	m.streamsSeen++
	m.threads = append(m.threads, mockThreadID)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(mockStream(MockModelDisplay)))
}

// serveThreads answers the conversation endpoints.
func (m *MockUpstream) serveThreads(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookie); err != nil || cookie.Value != MockAPIKey {
		writeMockJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "Please sign in to chat.",
		})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == ThreadsPath {
		writeMockJSON(w, http.StatusOK, map[string]any{
			"threads": []map[string]any{{
				"id":         mockThreadID,
				"title":      "Mock conversation",
				"model":      MockModelDisplay,
				"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
			}},
			"accessTier": "limited",
		})
		return
	}
	writeMockJSON(w, http.StatusOK, map[string]any{
		"thread": map[string]any{
			"id":         mockThreadID,
			"title":      "Mock conversation",
			"model":      MockModelDisplay,
			"created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
		"messages": []map[string]any{
			{
				"id":      "mock-user-message",
				"role":    "user",
				"content": "prompt",
				"model":   nil,
			},
			{
				"id":      "mock-assistant-message",
				"role":    "assistant",
				"content": MockReply,
				"model":   MockModelDisplay,
				"blocks": []map[string]any{
					{"text": MockReasoning, "type": "thinking", "status": "done"},
					{"text": MockReply, "type": "text"},
					{
						"type":       "suggestions",
						"toolCallId": "mock-tool-call",
						"followups":  []map[string]string{{"label": "More", "prompt": "Tell me more"}},
					},
				},
			},
		},
	})
}

// CatalogPathForTest exposes the catalogue path so a test can scope a fault
// without duplicating the constant.
func CatalogPathForTest() string { return CatalogPath }

// ChatPathForTest exposes the chat path for the same reason.
func ChatPathForTest() string { return ChatPath }

// ThreadsPathForTest exposes the threads path for the same reason.
func ThreadsPathForTest() string { return ThreadsPath }

func mustMockJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func writeMockJSON(w http.ResponseWriter, status int, payload any) {
	writeMockRaw(w, status, mustMockJSON(payload))
}

func writeMockRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func writeMockFault(w http.ResponseWriter, fault mockFault) {
	for key, value := range fault.headers {
		w.Header().Set(key, value)
	}
	status := fault.status
	if status == 0 {
		status = http.StatusBadRequest
	}
	writeMockRaw(w, status, fault.body)
}

func readMockBody(r *http.Request) ([]byte, error) {
	defer func() {
		if r.Body != nil {
			_ = r.Body.Close()
		}
	}()
	if r.Body == nil {
		return nil, fmt.Errorf("freebuff mock: request has no body")
	}
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 1024)
	for {
		n, err := r.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 1<<20 {
			break
		}
	}
	return buf, nil
}

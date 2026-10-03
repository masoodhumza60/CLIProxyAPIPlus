package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// freebuffTestClient builds a Client pointed at srv with a recording sleep so no
// test ever waits on the wall clock.
func freebuffTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(srv.URL, "test-key", Options{
		Sleep: func(time.Duration) {},
		Now:   time.Now,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// jsonHandler replies with the given status and body on every request, and
// records the last request's method/path/headers for assertions.
type recordedRequest struct {
	method  string
	path    string
	headers http.Header
	body    []byte
}

type recordingHandler struct {
	status   int
	body     string
	headers  map[string]string
	last     recordedRequest
	requests int
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var payload []byte
	if r.Body != nil {
		payload, _ = io.ReadAll(r.Body)
	}
	h.last = recordedRequest{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: payload}
	h.requests++
	for k, v := range h.headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(h.body))
}

func TestFreebuffAdmitReturnsActiveState(t *testing.T) {
	h := &recordingHandler{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-1","model":"google/gemini-2.5-flash-lite","rateLimit":{"remaining":9},"subscription":{"tier":"free"},"freebucks":42}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	state, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{Model: "google/gemini-2.5-flash-lite"})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if state.Status != StatusActive {
		t.Errorf("status = %q, want active", state.Status)
	}
	if state.InstanceID != "inst-1" {
		t.Errorf("instance id = %q, want inst-1", state.InstanceID)
	}
	if state.Model != "google/gemini-2.5-flash-lite" {
		t.Errorf("model = %q", state.Model)
	}
	// The loosely-typed passthrough fields must survive verbatim.
	if len(state.RateLimit) == 0 || len(state.Subscription) == 0 || string(state.Freebucks) != "42" {
		t.Errorf("passthrough fields not preserved: rateLimit=%s subscription=%s freebucks=%s", state.RateLimit, state.Subscription, state.Freebucks)
	}
	if h.last.method != http.MethodPost {
		t.Errorf("method = %s, want POST", h.last.method)
	}
	if h.last.path != pathSessionAdmission {
		t.Errorf("path = %s, want %s", h.last.path, pathSessionAdmission)
	}
}

// A 404 on a session *poll* means there is no such session, which is a value.
// The 404-means-none rule applies to GET only; on the admission POST a 404 means
// the endpoint does not exist, which TestFreebuffAdmitRejectsUnsupportedAdmissionEndpoint covers.
func TestFreebuffPollTreats404AsNoSession(t *testing.T) {
	h := &recordingHandler{status: http.StatusNotFound, body: `{"error":"not found"}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	state, err := NewSessionAdmission(freebuffTestClient(t, srv)).Poll(context.Background(), "inst-1", false)
	if err != nil {
		t.Fatalf("Poll returned an error for 404, want a none value: %v", err)
	}
	if state.Status != StatusNone {
		t.Errorf("status = %q, want none", state.Status)
	}
}

func TestFreebuffAdmitRejectsUnsupportedAdmissionEndpoint(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		h := &recordingHandler{status: status, body: `{"error":"nope"}`}
		srv := httptest.NewServer(h)

		_, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		var se *SessionError
		if !errors.As(err, &se) {
			t.Fatalf("status %d: error is %T, want *SessionError", status, err)
		}
		if se.ErrorCode != errCodeAdmissionUnsupported {
			t.Errorf("status %d: error code = %q, want %q", status, se.ErrorCode, errCodeAdmissionUnsupported)
		}
	}
}

// 403 is only a value when the body names a country block or a ban; any other
// 403 is a genuine failure.
func TestFreebuffAdmitReturnsGateValuesOn403(t *testing.T) {
	for _, code := range []string{"country_blocked", "banned"} {
		h := &recordingHandler{status: http.StatusForbidden, body: `{"status":"` + code + `"}`}
		srv := httptest.NewServer(h)

		state, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: Admit returned an error, want a value: %v", code, err)
		}
		if string(state.Status) != code {
			t.Errorf("status = %q, want %q", state.Status, code)
		}
	}

	h := &recordingHandler{status: http.StatusForbidden, body: `{"error":"forbidden"}`}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{}); err == nil {
		t.Error("expected a plain 403 to be an error")
	}
}

func TestFreebuffAdmitReturnsConflictGateValues(t *testing.T) {
	conflicts := []string{
		"session_superseded", "session_model_mismatch", "session_limit_reached",
		"premium_slot_taken", "purchase_claim_released", "purchase_in_use",
		"purchase_capacity", "model_locked", "model_unavailable",
		"first_tab_discount_changed", "consent_required",
	}
	for _, code := range conflicts {
		h := &recordingHandler{status: http.StatusConflict, body: `{"status":"` + code + `"}`}
		srv := httptest.NewServer(h)

		state, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{})
		srv.Close()
		if err != nil {
			t.Errorf("%s: Admit returned an error, want a value: %v", code, err)
			continue
		}
		if string(state.Status) != code {
			t.Errorf("status = %q, want %q", state.Status, code)
		}
	}

	// A 409 whose body names no known status is a real conflict, not a gate.
	h := &recordingHandler{status: http.StatusConflict, body: `{"error":"conflict"}`}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{}); err == nil {
		t.Error("expected an unknown 409 to be an error")
	}
}

func TestFreebuffAdmitReturnsThrottleGateValues(t *testing.T) {
	for _, code := range []string{"rate_limited", "spend_limited", "ip_capped"} {
		h := &recordingHandler{
			status:  http.StatusTooManyRequests,
			body:    `{"status":"` + code + `"}`,
			headers: map[string]string{"Retry-After": "30"},
		}
		srv := httptest.NewServer(h)

		state, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{})
		srv.Close()
		if err != nil {
			t.Errorf("%s: Admit returned an error, want a value: %v", code, err)
			continue
		}
		if string(state.Status) != code {
			t.Errorf("status = %q, want %q", state.Status, code)
		}
	}

	h := &recordingHandler{status: http.StatusTooManyRequests, body: `{"error":"slow down"}`}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{}); err == nil {
		t.Error("expected an unknown 429 to be an error")
	}
}

func TestFreebuffAdmitRetriesServerErrors(t *testing.T) {
	h := &recordingHandler{status: http.StatusBadGateway, body: `{"error":"upstream down"}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, err := NewSessionAdmission(freebuffTestClient(t, srv)).Admit(context.Background(), AdmissionRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}
	var se *SessionError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T, want *SessionError", err)
	}
	if se.StatusCode() != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", se.StatusCode())
	}
	// 502 is retryable, so all three attempts must have been spent.
	if h.requests != MaxAttempts {
		t.Errorf("attempts = %d, want %d", h.requests, MaxAttempts)
	}
}

func TestFreebuffPollUsesSessionEndpoint(t *testing.T) {
	h := &recordingHandler{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-9","model":"google/gemini-2.5-flash-lite"}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	state, err := NewSessionAdmission(freebuffTestClient(t, srv)).Poll(context.Background(), "inst-9", true)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if state.InstanceID != "inst-9" {
		t.Errorf("instance id = %q", state.InstanceID)
	}
	if h.last.method != http.MethodGet {
		t.Errorf("method = %s, want GET", h.last.method)
	}
	if h.last.path != pathSession {
		t.Errorf("path = %s, want %s", h.last.path, pathSession)
	}
	if got := h.last.headers.Get(headerInstance); got != "inst-9" {
		t.Errorf("%s = %q, want inst-9", headerInstance, got)
	}
	if got := h.last.headers.Get(headerCompactSession); got != "1" {
		t.Errorf("%s = %q, want 1 when compact polling is requested", headerCompactSession, got)
	}
}

func TestFreebuffPollOmitsCompactHeaderWhenNotRequested(t *testing.T) {
	h := &recordingHandler{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-9"}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	if _, err := NewSessionAdmission(freebuffTestClient(t, srv)).Poll(context.Background(), "inst-9", false); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if _, ok := h.last.headers[headerCompactSession]; ok {
		t.Error("compact-session header must not be sent unless requested")
	}
}

func TestFreebuffReleaseUsesDelete(t *testing.T) {
	h := &recordingHandler{status: http.StatusNoContent}
	srv := httptest.NewServer(h)
	defer srv.Close()

	if err := NewSessionAdmission(freebuffTestClient(t, srv)).Release(context.Background(), "inst-3"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if h.last.method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", h.last.method)
	}
	if h.last.path != pathSession {
		t.Errorf("path = %s, want %s", h.last.path, pathSession)
	}
	if got := h.last.headers.Get(headerInstance); got != "inst-3" {
		t.Errorf("%s = %q, want inst-3", headerInstance, got)
	}
}

func TestFreebuffReleaseRejectsEmptyInstance(t *testing.T) {
	srv := httptest.NewServer(&recordingHandler{status: http.StatusNoContent})
	defer srv.Close()

	if err := NewSessionAdmission(freebuffTestClient(t, srv)).Release(context.Background(), "  "); err == nil {
		t.Error("expected an error when releasing an empty instance id")
	}
}

// ClassifyFailure encodes upstream's asymmetric retry safety: a POST that
// produced no response may already have committed its mutation, so it is only
// retried on the statuses that are produced before the mutation lands.
func TestFreebuffClassifyFailureIsAsymmetric(t *testing.T) {
	sessionErr := func(status int) error {
		return &SessionError{Status: status, Method: http.MethodPost, Path: pathSessionAdmission, Message: "boom"}
	}

	post := []struct {
		status int
		want   Disposition
	}{
		{http.StatusRequestTimeout, DispositionRetry},
		{http.StatusTooManyRequests, DispositionRetry},
		{http.StatusServiceUnavailable, DispositionRetry},
		{http.StatusBadGateway, DispositionUnknown},
		{http.StatusInternalServerError, DispositionUnknown},
		{http.StatusBadRequest, DispositionStop},
		{http.StatusUnauthorized, DispositionStop},
		{http.StatusConflict, DispositionStop},
		{http.StatusPreconditionRequired, DispositionStop},
	}
	for _, tc := range post {
		if got := ClassifyFailure(http.MethodPost, sessionErr(tc.status)); got != tc.want {
			t.Errorf("POST %d = %q, want %q", tc.status, got, tc.want)
		}
	}

	get := []struct {
		status int
		want   Disposition
	}{
		{http.StatusRequestTimeout, DispositionRetry},
		{http.StatusTooManyRequests, DispositionRetry},
		{http.StatusInternalServerError, DispositionRetry},
		{http.StatusBadGateway, DispositionRetry},
		{http.StatusServiceUnavailable, DispositionRetry},
		{http.StatusBadRequest, DispositionStop},
		{http.StatusUnauthorized, DispositionStop},
		{http.StatusConflict, DispositionStop},
	}
	for _, tc := range get {
		if got := ClassifyFailure(http.MethodGet, sessionErr(tc.status)); got != tc.want {
			t.Errorf("GET %d = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestFreebuffClassifyFailureTreatsTransportErrorsAsRetryable(t *testing.T) {
	transport := errors.New("connection reset")
	if got := ClassifyFailure(http.MethodGet, transport); got != DispositionRetry {
		t.Errorf("GET transport error = %q, want retry", got)
	}
	// A POST that never reached the server is safe to repeat, because no
	// mutation can have committed.
	if got := ClassifyFailure(http.MethodPost, transport); got != DispositionRetry {
		t.Errorf("POST transport error = %q, want retry", got)
	}
}

func TestFreebuffClassifyFailureHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ClassifyFailure(http.MethodPost, ctx.Err()); got != DispositionStop {
		t.Errorf("cancelled POST = %q, want stop", got)
	}
	if got := ClassifyFailure(http.MethodGet, ctx.Err()); got != DispositionStop {
		t.Errorf("cancelled GET = %q, want stop", got)
	}
}

// A compact poll is only meaningful when it confirms the same live slot; losing
// the freebucks balance would blank the client's usage meter, so a mismatch has
// to discard the merged view rather than silently drop those fields.
func TestFreebuffMergeCompactSession(t *testing.T) {
	current := &SessionState{
		Status:            StatusActive,
		InstanceID:        "inst-1",
		Model:             "google/gemini-2.5-flash-lite",
		RateLimit:         json.RawMessage(`{"remaining":9}`),
		RateLimitsByModel: map[string]json.RawMessage{"a": json.RawMessage(`{"r":1}`)},
		Subscription:      json.RawMessage(`{"tier":"free"}`),
		Freebucks:         json.RawMessage(`42`),
	}

	same := &SessionState{Status: StatusActive, InstanceID: "inst-1", Model: "google/gemini-2.5-flash-lite"}
	merged := MergeCompactSession(current, same)
	if merged == nil {
		t.Fatal("same live slot merged to nil")
	}
	if string(merged.Freebucks) != "42" {
		t.Errorf("freebucks = %s, want the carried-forward 42", merged.Freebucks)
	}
	if string(merged.RateLimit) != `{"remaining":9}` {
		t.Errorf("rate limit = %s, want carried forward", merged.RateLimit)
	}
	if string(merged.Subscription) != `{"tier":"free"}` {
		t.Errorf("subscription = %s, want carried forward", merged.Subscription)
	}
	if len(merged.RateLimitsByModel) != 1 {
		t.Errorf("rate limits by model = %v, want carried forward", merged.RateLimitsByModel)
	}
	if merged.Status != StatusActive {
		t.Errorf("status = %q, want active", merged.Status)
	}

	differentInstance := &SessionState{Status: StatusActive, InstanceID: "inst-2", Model: current.Model}
	if got := MergeCompactSession(current, differentInstance); got != nil {
		t.Error("a different instance must not merge")
	}
	differentModel := &SessionState{Status: StatusActive, InstanceID: current.InstanceID, Model: "other/model"}
	if got := MergeCompactSession(current, differentModel); got != nil {
		t.Error("a different model must not merge")
	}
	ended := &SessionState{Status: StatusEnded, InstanceID: current.InstanceID, Model: current.Model}
	if got := MergeCompactSession(current, ended); got != nil {
		t.Error("an ended session must not merge")
	}
	if got := MergeCompactSession(nil, same); got != nil {
		t.Error("a nil current session must not merge")
	}
	if got := MergeCompactSession(current, nil); got != nil {
		t.Error("a nil poll result must not merge")
	}
}

// MergeCompactSession must not alias the caller's maps or raw messages.
func TestFreebuffMergeCompactSessionCopies(t *testing.T) {
	current := &SessionState{
		Status:            StatusActive,
		InstanceID:        "inst-1",
		Model:             "m",
		RateLimitsByModel: map[string]json.RawMessage{"a": json.RawMessage(`{"r":1}`)},
	}
	merged := MergeCompactSession(current, &SessionState{Status: StatusActive, InstanceID: "inst-1", Model: "m"})
	if merged == nil {
		t.Fatal("merge returned nil")
	}
	merged.RateLimitsByModel["b"] = json.RawMessage(`{"r":2}`)
	if len(current.RateLimitsByModel) != 1 {
		t.Error("mutating the merged map changed the original")
	}
}

// HoldsLiveFreebuffSlot decides whether a session is still worth polling: an
// active slot, or an ended one that still names the instance that was held.
func TestFreebuffHoldsLiveFreebuffSlot(t *testing.T) {
	if !HoldsLiveFreebuffSlot(&SessionState{Status: StatusActive}) {
		t.Error("active must hold a slot")
	}
	if !HoldsLiveFreebuffSlot(&SessionState{Status: StatusEnded, InstanceID: "inst-1"}) {
		t.Error("ended with an instance id must hold a slot")
	}
	if HoldsLiveFreebuffSlot(&SessionState{Status: StatusEnded}) {
		t.Error("ended without an instance id must not hold a slot")
	}
	if HoldsLiveFreebuffSlot(&SessionState{Status: StatusRateLimited}) {
		t.Error("rate limited must not hold a slot")
	}
	if HoldsLiveFreebuffSlot(nil) {
		t.Error("nil must not hold a slot")
	}
}

func TestFreebuffSessionStatusIsJSONRoundTrippable(t *testing.T) {
	// The server sends `status` as a bare string, so the constant must match
	// the wire spelling exactly or gates would never be recognised.
	cases := map[SessionStatus]string{
		StatusActive:                "active",
		StatusEnded:                 "ended",
		StatusNone:                  "none",
		StatusCountryBlocked:        "country_blocked",
		StatusBanned:                "banned",
		StatusModelLocked:           "model_locked",
		StatusModelUnavailable:      "model_unavailable",
		StatusPremiumSlotTaken:      "premium_slot_taken",
		StatusPurchaseClaimReleased: "purchase_claim_released",
		StatusPurchaseInUse:         "purchase_in_use",
		StatusPurchaseCapacity:      "purchase_capacity",
		StatusFirstTabDiscount:      "first_tab_discount_changed",
		StatusConsentRequired:       "consent_required",
		StatusRateLimited:           "rate_limited",
		StatusSpendLimited:          "spend_limited",
		StatusIPCapped:              "ip_capped",
		StatusSuperseded:            "superseded",
	}
	for status, want := range cases {
		if string(status) != want {
			t.Errorf("status %q != wire spelling %q", status, want)
		}
		encoded, err := json.Marshal(status)
		if err != nil {
			t.Fatalf("marshal %q: %v", status, err)
		}
		if string(encoded) != `"`+want+`"` {
			t.Errorf("encoded %s, want %q", encoded, want)
		}
	}
}

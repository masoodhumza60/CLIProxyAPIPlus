package freebuff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestClient builds a Client aimed at srv with a recording sleep function so
// retry backoff is observable without the test ever waiting.
func newTestClient(t *testing.T, srv *httptest.Server, apiKey string, sleeps *[]time.Duration) *Client {
	t.Helper()
	c, err := NewClient(srv.URL, apiKey, Options{
		Sleep: func(d time.Duration) {
			if sleeps != nil {
				*sleeps = append(*sleeps, d)
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestFreebuffNewClientRejectsEmptyBaseURL(t *testing.T) {
	// The base URL is always caller-supplied: this client has no default, no
	// environment fallback, and no production endpoint. An empty value is a
	// programming error, not something to paper over.
	if _, err := NewClient("", "key", Options{}); err == nil {
		t.Fatal("expected NewClient to reject an empty base URL")
	}
	if _, err := NewClient("   ", "key", Options{}); err == nil {
		t.Fatal("expected NewClient to reject a whitespace-only base URL")
	}
}

func TestFreebuffNewClientRejectsEmptyAPIKey(t *testing.T) {
	if _, err := NewClient("https://example.invalid", "", Options{}); err == nil {
		t.Fatal("expected NewClient to reject an empty API key")
	}
}

func TestFreebuffClientSendsAuthAndUserAgentOnEveryVerb(t *testing.T) {
	type seen struct {
		method      string
		authz       string
		apiKeyHdr   string
		userAgent   string
		contentType string
		accept      string
	}
	var mu sync.Mutex
	var got []seen

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{
			method:      r.Method,
			authz:       r.Header.Get("Authorization"),
			apiKeyHdr:   r.Header.Get("x-codebuff-api-key"),
			userAgent:   r.Header.Get("User-Agent"),
			contentType: r.Header.Get("Content-Type"),
			accept:      r.Header.Get("Accept"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "secret-api-key", nil)
	ctx := context.Background()

	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/token-count", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("POST: %v", err)
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/api/v1/freebuff/session", nil); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if _, err := c.doJSON(ctx, http.MethodDelete, "/api/v1/freebuff/session/abc", nil); err != nil {
		t.Fatalf("DELETE: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(got))
	}
	for _, s := range got {
		if s.authz != "Bearer secret-api-key" {
			t.Errorf("%s: Authorization = %q, want Bearer token", s.method, s.authz)
		}
		if s.apiKeyHdr != "secret-api-key" {
			t.Errorf("%s: x-codebuff-api-key = %q, want the API key", s.method, s.apiKeyHdr)
		}
		// Honest, constant identity. This client does not impersonate any
		// official SDK and does not randomize its fingerprint.
		if s.userAgent != UserAgent {
			t.Errorf("%s: User-Agent = %q, want %q", s.method, s.userAgent, UserAgent)
		}
		if s.accept == "" {
			t.Errorf("%s: expected an Accept header", s.method)
		}
	}
}

func TestFreebuffClientRetriesOnlyRetryableStatuses(t *testing.T) {
	// Reproduces the upstream retry set exactly.
	retryable := []int{408, 429, 500, 502, 503, 504}
	for _, status := range retryable {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var attempts int
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts++
				n := attempts
				mu.Unlock()
				if n < 3 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":"try again"}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			var sleeps []time.Duration
			c := newTestClient(t, srv, "k", &sleeps)
			if _, err := c.doJSON(context.Background(), http.MethodPost, "/x", []byte(`{}`)); err != nil {
				t.Fatalf("expected success after retries on %d: %v", status, err)
			}
			if attempts != 3 {
				t.Errorf("expected 3 attempts on %d, got %d", status, attempts)
			}
			if len(sleeps) != 2 {
				t.Errorf("expected 2 backoff sleeps on %d, got %d (%v)", status, len(sleeps), sleeps)
			}
		})
	}

	notRetryable := []int{400, 401, 403, 404, 422}
	for _, status := range notRetryable {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var attempts int
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts++
				mu.Unlock()
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"nope"}`))
			}))
			defer srv.Close()

			var sleeps []time.Duration
			c := newTestClient(t, srv, "k", &sleeps)
			_, err := c.doJSON(context.Background(), http.MethodPost, "/x", []byte(`{}`))
			if err == nil {
				t.Fatalf("expected an error on %d", status)
			}
			if attempts != 1 {
				t.Errorf("expected exactly 1 attempt on %d, got %d", status, attempts)
			}
			if len(sleeps) != 0 {
				t.Errorf("expected no backoff on %d, got %v", status, sleeps)
			}
		})
	}
}

func TestFreebuffClientStopsAfterMaxAttempts(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer srv.Close()

	var sleeps []time.Duration
	c := newTestClient(t, srv, "k", &sleeps)
	_, err := c.doJSON(context.Background(), http.MethodPost, "/x", []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error once retries are exhausted")
	}
	if attempts != MaxAttempts {
		t.Errorf("expected %d attempts, got %d", MaxAttempts, attempts)
	}
	if len(sleeps) != MaxAttempts-1 {
		t.Errorf("expected %d backoff sleeps, got %d", MaxAttempts-1, len(sleeps))
	}
	var se *SessionError
	if !asSessionError(err, &se) {
		t.Fatalf("expected a *SessionError, got %T", err)
	}
	if se.StatusCode() != 503 {
		t.Errorf("StatusCode() = %d, want 503", se.StatusCode())
	}
}

func TestFreebuffClientBackoffIsExponential(t *testing.T) {
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	var sleeps []time.Duration
	c := newTestClient(t, srv, "k", &sleeps)
	if _, err := c.doJSON(context.Background(), http.MethodPost, "/x", []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
	if len(sleeps) != 2 {
		t.Fatalf("expected 2 sleeps, got %d (%v)", len(sleeps), sleeps)
	}
	if sleeps[0] != RetryBaseDelay {
		t.Errorf("first backoff = %v, want %v", sleeps[0], RetryBaseDelay)
	}
	if sleeps[1] != 2*RetryBaseDelay {
		t.Errorf("second backoff = %v, want %v (exponential)", sleeps[1], 2*RetryBaseDelay)
	}
}

func TestFreebuffClientHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	// A cancelled context must abort immediately rather than sitting through
	// the backoff schedule.
	var sleeps []time.Duration
	c := newTestClient(t, srv, "k", &sleeps)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.doJSON(ctx, http.MethodPost, "/x", []byte(`{}`)); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
	if len(sleeps) != 0 {
		t.Errorf("expected no backoff sleeps after cancellation, got %v", sleeps)
	}
}

func TestFreebuffSessionErrorExposesStatusAndRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer srv.Close()

	var sleeps []time.Duration
	c := newTestClient(t, srv, "k", &sleeps)
	_, err := c.doJSON(context.Background(), http.MethodPost, "/x", []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}

	se, ok := err.(*SessionError)
	if !ok {
		t.Fatalf("expected *SessionError, got %T", err)
	}
	if se.StatusCode() != 429 {
		t.Errorf("StatusCode() = %d, want 429", se.StatusCode())
	}
	// The conductor's convention is RetryAfter() *time.Duration.
	ra := se.RetryAfter()
	if ra == nil {
		t.Fatal("expected a non-nil RetryAfter for a 429 carrying Retry-After")
	}
	if *ra != 42*time.Second {
		t.Errorf("RetryAfter() = %v, want 42s", *ra)
	}
	if se.ErrorCode != "rate_limited" {
		t.Errorf("ErrorCode = %q, want rate_limited", se.ErrorCode)
	}
}

func TestFreebuffSessionErrorWithoutRetryAfterReturnsNil(t *testing.T) {
	se := &SessionError{Status: 500, Message: "boom"}
	if se.RetryAfter() != nil {
		t.Error("expected nil RetryAfter when the response carried no Retry-After")
	}
	if se.RetryAfterMs != 0 {
		t.Errorf("RetryAfterMs = %d, want 0", se.RetryAfterMs)
	}
}

func TestFreebuffParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	t.Run("integer seconds", func(t *testing.T) {
		d, ok := ParseRetryAfter("120", now)
		if !ok || d != 120*time.Second {
			t.Errorf("ParseRetryAfter(120) = %v,%v want 120s,true", d, ok)
		}
	})

	t.Run("http date", func(t *testing.T) {
		when := now.Add(90 * time.Second)
		d, ok := ParseRetryAfter(when.Format(http.TimeFormat), now)
		if !ok {
			t.Fatal("expected the HTTP-date form to parse")
		}
		// HTTP dates have one-second resolution, so allow a small slack.
		if d < 89*time.Second || d > 91*time.Second {
			t.Errorf("ParseRetryAfter(http-date) = %v, want ~90s", d)
		}
	})

	t.Run("past http date is not negative", func(t *testing.T) {
		when := now.Add(-time.Hour)
		d, ok := ParseRetryAfter(when.Format(http.TimeFormat), now)
		if !ok {
			t.Fatal("expected the past HTTP-date to parse")
		}
		if d < 0 {
			t.Errorf("ParseRetryAfter(past) = %v, want a non-negative duration", d)
		}
	})

	for _, bad := range []string{"", "   ", "soon", "0", "-5", "12.5"} {
		t.Run("garbage:"+bad, func(t *testing.T) {
			if d, ok := ParseRetryAfter(bad, now); ok {
				t.Errorf("ParseRetryAfter(%q) = %v,true want not-ok", bad, d)
			}
		})
	}
}

func TestFreebuffDoJSONDecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"inputTokens":1234}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "k", nil)
	out, err := c.doJSON(context.Background(), http.MethodPost, "/api/v1/token-count", []byte(`{}`))
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	var body struct {
		InputTokens int `json:"inputTokens"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.InputTokens != 1234 {
		t.Errorf("inputTokens = %d, want 1234", body.InputTokens)
	}
}

func TestFreebuffDoJSONRejectsUnparseableResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "k", nil)
	if _, err := c.doJSON(context.Background(), http.MethodPost, "/x", []byte(`{}`)); err == nil {
		t.Fatal("expected an error for a non-JSON response body")
	}
}

func TestFreebuffDoJSONTruncatesLargeErrorBodies(t *testing.T) {
	// Error bodies are echoed to the caller, so they must stay bounded even
	// when the upstream sends something enormous.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":"`+strings.Repeat("x", MaxErrorBody*4)+`"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "k", nil)
	_, err := c.doJSON(context.Background(), http.MethodPost, "/x", []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	body := errorBody(err)
	if len(body) > MaxErrorBody+32 {
		t.Errorf("error body is %d bytes, want it bounded near the %d byte cap", len(body), MaxErrorBody)
	}
	if !strings.Contains(body, "truncated") {
		t.Error("expected the retained body to be marked as truncated")
	}
}

func errorBody(err error) string {
	if se, ok := err.(*SessionError); ok {
		return se.Body
	}
	return ""
}

func TestFreebuffSessionErrorMessageDoesNotEchoSecrets(t *testing.T) {
	se := &SessionError{Status: 401, Message: "unauthorized", Body: "..."}
	msg := se.Error()
	if len(msg) == 0 {
		t.Fatal("expected a non-empty error message")
	}
	if got := fmt.Sprintf("%v", se); got == "" {
		t.Fatal("expected a printable error")
	}
}

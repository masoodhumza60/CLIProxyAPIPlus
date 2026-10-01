// Package freebuff implements a client for the Codebuff / Freebuff protocol.
//
// Scope and intent are deliberately narrow. This package speaks the documented
// wire protocol against a caller-supplied base URL so that the request and
// response handling can be exercised and verified. It does not ship an
// operator-configurable production endpoint, it does not acquire credentials,
// it does not impersonate any official client, and it reports Freebuff's own
// access gates (waiting room, rate limits, spend limits, country blocks)
// faithfully rather than working around them. See protocol_notes.md for the
// upstream source references behind each constant.
package freebuff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// UserAgent is the single honest identity this client presents. It is
// deliberately constant: this client does not randomize or spoof its
// fingerprint to resemble an official SDK.
const UserAgent = "cli-proxy-api-freebuff/1.0"

// headerAnonymous is a pseudo-header key understood by attempt. It marks a
// request as pre-credential so the auth headers are omitted. It is stripped
// before the remaining entries are applied as real headers.
const headerAnonymous = "x-freebuff-anonymous"

// Retry policy, mirroring the upstream agent runtime so that retry behavior
// matches the protocol rather than being invented locally.
const (
	// MaxAttempts is the total number of attempts, including the first.
	MaxAttempts = 3
	// RetryBaseDelay is the first backoff delay; each further retry doubles it.
	RetryBaseDelay = time.Second
	// MaxErrorBody bounds how much of an error response is retained.
	MaxErrorBody = 2048
)

// retryableStatuses is the exact set the upstream client retries on. A status
// outside this set is a definitive answer and is surfaced to the caller.
var retryableStatuses = map[int]bool{
	http.StatusRequestTimeout:      true, // 408
	http.StatusTooManyRequests:     true, // 429
	http.StatusInternalServerError: true, // 500
	http.StatusBadGateway:          true, // 502
	http.StatusServiceUnavailable:  true, // 503
	http.StatusGatewayTimeout:      true, // 504
}

// Options configures a Client. Every field has a usable default so that the
// only thing a caller must supply is where to connect and with what key.
type Options struct {
	// HTTPClient overrides the transport. Mainly useful for tests and for
	// proxy-aware clients assembled by the SDK.
	HTTPClient *http.Client
	// Sleep is the delay function used between retries. Tests inject a
	// recorder here so backoff is observable without any wall-clock wait.
	Sleep func(time.Duration)
	// Now supplies the current time, used when parsing HTTP-date Retry-After
	// values. Defaults to time.Now.
	Now func() time.Time
}

// Client speaks the Codebuff / Freebuff protocol over HTTP.
//
// The base URL is a required constructor argument with no default and no
// environment fallback. That is structural: this package has no baked-in
// production endpoint to point at.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	sleep      func(time.Duration)
	now        func() time.Time
}

// NewClient creates a client for the given base URL and API key.
//
// baseURL is required. This package intentionally ships without a default
// endpoint: callers supply one (in this repository, a test harness).
func NewClient(baseURL, apiKey string, opts Options) (*Client, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return nil, fmt.Errorf("freebuff: base URL is required and has no default; callers must supply one")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("freebuff: invalid base URL %q: %w", baseURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("freebuff: base URL %q must be absolute (include scheme and host)", baseURL)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("freebuff: API key is required")
	}
	return newClient(baseURL, apiKey, opts), nil
}

// NewLoginClient builds a client for the pre-credential login endpoints, which
// are reached before any token exists.
//
// The empty key is allowed only here, and the resulting client refuses to make
// an authenticated request. That keeps the invariant a plain NewClient
// enforces — a client that will talk to the API always holds a credential —
// without forcing the login flow to invent one.
func NewLoginClient(baseURL string, opts Options) (*Client, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return nil, errors.New("freebuff: base URL is required and has no default; callers must supply one")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("freebuff: invalid base URL %q: %w", baseURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("freebuff: base URL %q must be absolute (include scheme and host)", baseURL)
	}
	return newClient(trimmed, "", opts), nil
}

func newClient(baseURL, apiKey string, opts Options) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: httpClient,
		sleep:      sleep,
		now:        now,
	}
}

// BaseURL returns the endpoint this client was constructed with.
func (c *Client) BaseURL() string { return c.baseURL }

// APIKey returns the credential this client authenticates with.
//
// It is exposed so the executor can build a streaming request carrying the same
// dual auth headers the client sets. Callers must never log the result.
func (c *Client) APIKey() string { return c.apiKey }

// SessionError is a non-success response from the Freebuff protocol.
//
// It implements the SDK's StatusError so the conductor can classify the
// failure, and RetryAfter so rate limiting can be honoured.
type SessionError struct {
	// Status is the HTTP status code of the response.
	Status int
	// Message is a short, human-readable description.
	Message string
	// Body is the (bounded) response body, retained for diagnostics.
	Body string
	// ErrorCode is the protocol-level error code when the body carries one,
	// for example "waiting_room_queued" or "rate_limited".
	ErrorCode string
	// RetryAfterMs is the server-provided retry delay in milliseconds, or 0.
	RetryAfterMs int64
	// Method and Path record which request produced the failure.
	Method string
	Path   string
}

// Error implements error.
func (e *SessionError) Error() string {
	var b strings.Builder
	b.WriteString("freebuff: ")
	if e.Method != "" {
		b.WriteString(e.Method)
		b.WriteString(" ")
	}
	b.WriteString(e.Path)
	b.WriteString(": ")
	if e.Status > 0 {
		b.WriteString(strconv.Itoa(e.Status))
		b.WriteString(" ")
	}
	if e.ErrorCode != "" {
		b.WriteString(e.ErrorCode)
		b.WriteString(": ")
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	}
	if e.Message == "" && e.ErrorCode == "" && e.Body != "" {
		b.WriteString(truncate(e.Body, 200))
	}
	return b.String()
}

// StatusCode implements executor.StatusError.
func (e *SessionError) StatusCode() int { return e.Status }

// RetryAfter returns the server-requested delay, or nil when the server did not
// ask for one. It matches the pointer-returning convention the conductor uses
// for cooling decisions.
func (e *SessionError) RetryAfter() *time.Duration {
	if e.RetryAfterMs <= 0 {
		return nil
	}
	d := time.Duration(e.RetryAfterMs) * time.Millisecond
	return &d
}

// IsCredentialScoped reports whether the failure points at the credential
// rather than at the request. A 401 means the key was rejected, so the
// conductor should stop routing to it.
func (e *SessionError) IsCredentialScoped() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// compile-time check that the error is understood by the SDK.
var (
	_ executor.StatusError                     = (*SessionError)(nil)
	_ interface{ RetryAfter() *time.Duration } = (*SessionError)(nil)
	_ interface{ IsCredentialScoped() bool }   = (*SessionError)(nil)
)

// ParseRetryAfter interprets a Retry-After header value, which may be either a
// number of seconds or an HTTP date. It returns ok=false when the value cannot
// be understood, so callers can fall back to their own policy.
//
// A date already in the past yields a zero duration rather than a negative
// one: the wait is simply over.
func ParseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(header)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		delta := when.Sub(now)
		if delta < 0 {
			return 0, true
		}
		return delta, true
	}
	return 0, false
}

// doJSON performs a request against the protocol endpoint and returns the
// response body.
//
// It sets both authentication headers the protocol expects, retries only the
// documented retryable statuses with exponential backoff, and aborts promptly
// when the context is cancelled. Non-2xx responses become a *SessionError.
func (c *Client) doJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return c.doJSONWithHeaders(ctx, method, path, body, nil)
}

// doJSONWithHeaders is doJSON plus per-call protocol headers. The session
// endpoints identify their target instance through headers rather than the URL,
// so they need a way to add them without a bespoke request path.
func (c *Client) doJSONWithHeaders(ctx context.Context, method, path string, body []byte, headers map[string]string) ([]byte, error) {
	endpoint := c.baseURL + normalizePath(path)
	reqPath := normalizePath(path)

	var lastErr error
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff, but never after the caller has given up.
			delay := RetryBaseDelay << (attempt - 1)
			if err := sleepCtx(ctx, c.sleep, delay); err != nil {
				return nil, err
			}
		}

		out, retryable, err := c.attempt(ctx, method, endpoint, reqPath, body, headers)
		if err == nil {
			return out, nil
		}
		lastErr = err

		// A definitive status is the answer; retrying cannot change it.
		var se *SessionError
		if !asSessionError(err, &se) || !retryableStatuses[se.Status] || !retryable {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt runs a single request. The second return value reports whether the
// transport considered the failure transient, which lets doJSON distinguish a
// retryable status from an exhausted request.
func (c *Client) attempt(ctx context.Context, method, endpoint, reqPath string, body []byte, extra map[string]string) ([]byte, bool, error) {
	// A login request is made before any credential exists, so the auth headers
	// are suppressed for it. Sending an empty bearer token would be worse than
	// sending none: it looks like a rejected credential rather than an
	// unauthenticated bootstrap call.
	anonymous := extra[headerAnonymous] != ""
	delete(extra, headerAnonymous)

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, false, fmt.Errorf("freebuff: building %s %s: %w", method, reqPath, err)
	}
	if !anonymous {
		// A client built for login has no credential, so it must not be used for
		// an API call: doing so would send an empty bearer token, which reads
		// as a rejected credential rather than a programming mistake.
		if c.apiKey == "" {
			return nil, false, errors.New("freebuff: this client was built for login and cannot make authenticated requests")
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("x-codebuff-api-key", c.apiKey)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json, text/event-stream")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range extra {
		req.Header.Set(name, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, &SessionError{Message: "transport error: " + err.Error(), Method: method, Path: reqPath}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody*4))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := c.newSessionError(resp, payload, method, reqPath)
		return nil, true, se
	}
	if readErr != nil {
		return nil, false, &SessionError{
			Status:  resp.StatusCode,
			Message: "reading response body: " + readErr.Error(),
			Method:  method,
			Path:    reqPath,
		}
	}
	// Every endpoint reached through doJSON answers with JSON, with one
	// exception: DELETE returns 204 with no body. Catching a non-JSON body here
	// turns a confusing downstream unmarshal failure into a precise error.
	// Streaming responses use a separate path and never come through here.
	if len(payload) > 0 && !json.Valid(payload) {
		return nil, false, &SessionError{
			Status:  resp.StatusCode,
			Message: "response body is not valid JSON: " + truncate(string(payload), 200),
			Body:    truncate(string(payload), MaxErrorBody),
			Method:  method,
			Path:    reqPath,
		}
	}
	return payload, false, nil
}

// newSessionError converts a non-2xx response into a SessionError, pulling out
// the protocol error code and any Retry-After hint.
func (c *Client) newSessionError(resp *http.Response, payload []byte, method, path string) *SessionError {
	se := &SessionError{
		Status: resp.StatusCode,
		Body:   truncate(string(payload), MaxErrorBody),
		Method: method,
		Path:   path,
	}

	// The protocol names a gate in one of two places depending on the endpoint:
	// a top-level "error" field, or the "status" field of a session union
	// response. Reading only one of them makes half the gates invisible.
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil {
		se.ErrorCode = strings.TrimSpace(envelope.Error)
		se.Message = strings.TrimSpace(envelope.Message)
		if se.ErrorCode == "" {
			se.ErrorCode = strings.TrimSpace(envelope.Status)
		}
	}
	if se.Message == "" {
		se.Message = strings.TrimSpace(se.ErrorCode)
	}
	if se.ErrorCode == "" {
		se.ErrorCode = firstQuoted(se.Body)
	}

	if delay, ok := ParseRetryAfter(resp.Header.Get("Retry-After"), c.now()); ok {
		se.RetryAfterMs = delay.Milliseconds()
	}
	return se
}

// sleepCtx waits for d unless the context finishes first. Honouring the
// context is what stops a cancelled request from sitting through a backoff.
func sleepCtx(ctx context.Context, sleep func(time.Duration), d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sleep == nil {
		return ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		sleep(d)
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

// normalizePath guarantees a single leading slash and no trailing slash.
func normalizePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "... (truncated)"
}

// firstQuoted pulls the first double-quoted token out of s, used as a
// best-effort error code when the body is not valid JSON.
func firstQuoted(s string) string {
	start := strings.Index(s, `"`)
	if start < 0 {
		return ""
	}
	rest := s[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// asSessionError unwraps err looking for a *SessionError.
func asSessionError(err error, target **SessionError) bool {
	for err != nil {
		if se, ok := err.(*SessionError); ok {
			*target = se
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

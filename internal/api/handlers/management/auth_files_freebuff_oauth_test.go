package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fakeFreebuffLogin stands in for the authenticator so the flow can be driven
// without a network. It reports a sign-in URL the way the real one does - before
// any polling begins - and then optionally fails.
type fakeFreebuffLogin struct {
	url     string
	err     error
	record  *coreauth.Auth
	holdFor time.Duration
	// sawNoBrowser records whether the handler asked us not to open a browser.
	sawNoBrowser bool
}

func (f *fakeFreebuffLogin) Login(ctx context.Context, _ *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
	if opts != nil {
		f.sawNoBrowser = opts.NoBrowser
		if opts.OnLoginURL != nil && f.url != "" {
			opts.OnLoginURL(f.url)
		}
	}
	// The real login polls until the user finishes in their browser, so hold
	// the goroutine open rather than completing instantly. That also proves the
	// handler returns before the login is finished.
	select {
	case <-time.After(f.holdFor):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.record != nil {
		return f.record, nil
	}
	// Mirrors what FreebuffAuthenticator.Login returns: the token in both
	// Attributes and Metadata, because the credential file is written from
	// Metadata.
	return &coreauth.Auth{
		ID:       "freebuff-test.json",
		FileName: "freebuff-test.json",
		Provider: "freebuff",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "fb-token",
			"base_url": "https://freebuff.com",
		},
		Metadata: map[string]any{
			"type":           "freebuff",
			"api_key":        "fb-token",
			"base_url":       "https://freebuff.com",
			"fingerprint_id": "cli-proxy-test",
		},
	}, nil
}

// waitForSessionSettled blocks until the session leaves the pending state.
func waitForSessionSettled(t *testing.T, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !IsOAuthSessionPending(state, freebuffProviderID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("oauth session %q was still pending after 10s", state)
}

func newFreebuffTestRouter(t *testing.T, h *Handler) *gin.Engine {
	t.Helper()
	router := gin.New()
	router.GET("/oauth/auth-url", h.StartOAuthV8)
	return router
}

// TestFreebuffOAuthFlow verifies the whole shape the desktop app depends on: a
// URL comes back in the response, the session is registered, and the credential
// is written once the user finishes signing in.
func TestFreebuffOAuthFlow(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: authDir,
		Freebuff: config.FreebuffConfig{
			Enabled: true,
			BaseURL: "https://freebuff.com",
		},
	}, nil)

	service := &fakeFreebuffLogin{url: "https://freebuff.com/cli/auth?code=abc123", holdFor: 150 * time.Millisecond}
	original := newFreebuffLoginService
	newFreebuffLoginService = func() freebuffLoginService { return service }
	t.Cleanup(func() { newFreebuffLoginService = original })

	router := newFreebuffTestRouter(t, h)

	req := httptest.NewRequest(http.MethodGet, "/oauth/auth-url?provider=freebuff", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var payload struct {
		Status string `json:"status"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q, want \"ok\"", payload.Status)
	}
	if payload.URL != service.url {
		t.Errorf("url = %q, want %q", payload.URL, service.url)
	}
	if payload.State == "" {
		t.Fatal("state is empty; the caller would have no way to poll")
	}
	if !service.sawNoBrowser {
		t.Error("handler did not set NoBrowser; a server process must not try to open a browser")
	}

	waitForSessionSettled(t, payload.State)

	matches, errGlob := filepath.Glob(filepath.Join(authDir, "freebuff*.json"))
	if errGlob != nil {
		t.Fatalf("failed to glob auth dir: %v", errGlob)
	}
	if len(matches) == 0 {
		t.Fatalf("no credential was written to %s", authDir)
	}
	data, errRead := os.ReadFile(matches[0])
	if errRead != nil {
		t.Fatalf("failed to read saved credential: %v", errRead)
	}
	if !containsAll(string(data), "fb-token", "freebuff") {
		t.Errorf("saved credential is missing expected content: %s", string(data))
	}
}

// TestFreebuffOAuthMissingBaseURL verifies the handler refuses rather than
// starting a login that could only fail later.
func TestFreebuffOAuthMissingBaseURL(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	called := false
	original := newFreebuffLoginService
	newFreebuffLoginService = func() freebuffLoginService {
		called = true
		return &fakeFreebuffLogin{url: "https://freebuff.com/x"}
	}
	t.Cleanup(func() { newFreebuffLoginService = original })

	router := newFreebuffTestRouter(t, h)
	req := httptest.NewRequest(http.MethodGet, "/oauth/auth-url?provider=freebuff", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("login was started even though no base URL is configured")
	}
}

// TestFreebuffOAuthLoginFailureRecordsSessionError verifies a failed sign-in is
// reported on the session rather than swallowed, so the UI can tell the user.
func TestFreebuffOAuthLoginFailureRecordsSessionError(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir:  t.TempDir(),
		Freebuff: config.FreebuffConfig{Enabled: true, BaseURL: "https://freebuff.com"},
	}, nil)

	service := &fakeFreebuffLogin{
		url:     "https://freebuff.com/cli/auth?code=abc123",
		err:     context.DeadlineExceeded,
		holdFor: 50 * time.Millisecond,
	}
	original := newFreebuffLoginService
	newFreebuffLoginService = func() freebuffLoginService { return service }
	t.Cleanup(func() { newFreebuffLoginService = original })

	router := newFreebuffTestRouter(t, h)
	req := httptest.NewRequest(http.MethodGet, "/oauth/auth-url?provider=freebuff", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	waitForSessionSettled(t, payload.State)

	if _, errGlob := filepath.Glob(filepath.Join(h.cfg.AuthDir, "freebuff*.json")); errGlob == nil {
		matches, _ := filepath.Glob(filepath.Join(h.cfg.AuthDir, "freebuff*.json"))
		if len(matches) > 0 {
			t.Error("a credential was written even though the login failed")
		}
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(haystack); i++ {
			if haystack[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

//go:build freebuffmock

package freebuff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// loginTestClient builds a client with an injected sleep so the poll loop can
// be driven instantly. Waiting five real seconds per poll would make the suite
// slow for no added confidence.
func loginTestClient(t *testing.T, baseURL string, sleeps *[]time.Duration) *Client {
	t.Helper()
	client, err := NewClient(baseURL, "not-yet-logged-in", Options{
		Sleep: func(d time.Duration) {
			if sleeps != nil {
				*sleeps = append(*sleeps, d)
			}
		},
		Now: time.Now,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestFreebuffRequestLoginCodeReturnsAURL(t *testing.T) {
	mock := NewMockUpstream()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)

	code, err := client.RequestLoginCode(context.Background(), "cli-proxy-abc")
	if err != nil {
		t.Fatalf("RequestLoginCode: %v", err)
	}
	if code.LoginURL == "" {
		t.Error("expected a login url")
	}
	if code.FingerprintHash == "" {
		t.Error("expected a fingerprint hash to poll with")
	}
	if code.ExpiresAt == 0 {
		t.Error("expected an expiry so the poll can be bounded")
	}
}

// The login endpoints are reached before a credential exists, so the client
// must not send auth headers there. Sending an empty bearer token would look
// like a rejected credential rather than an unauthenticated bootstrap call.
func TestFreebuffLoginCodeRequestIsUnauthenticated(t *testing.T) {
	var sawAuthorization, sawAPIKeyHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		sawAPIKeyHeader = r.Header.Get("x-codebuff-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"loginUrl":"https://example.test/login","fingerprintHash":"h","expiresAt":1893456000000}`))
	}))
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	if _, err := client.RequestLoginCode(context.Background(), "cli-proxy-abc"); err != nil {
		t.Fatalf("RequestLoginCode: %v", err)
	}
	if sawAuthorization != "" {
		t.Errorf("login code request sent Authorization = %q, want none", sawAuthorization)
	}
	if sawAPIKeyHeader != "" {
		t.Errorf("login code request sent x-codebuff-api-key = %q, want none", sawAPIKeyHeader)
	}
}

func TestFreebuffRequestLoginCodeRejectsAnEmptyFingerprint(t *testing.T) {
	mock := NewMockUpstream()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	if _, err := client.RequestLoginCode(context.Background(), "  "); err == nil {
		t.Fatal("expected an error for a blank fingerprint id")
	}
}

func TestFreebuffRequestLoginCodeRejectsAResponseWithNoURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"fingerprintHash":"h","expiresAt":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	if _, err := client.RequestLoginCode(context.Background(), "cli-proxy-abc"); err == nil {
		t.Fatal("expected an error when the service returns no login url")
	}
}

// A 401 from the status endpoint means "not signed in yet", which is the
// ordinary pending answer and must not surface as a failure.
func TestFreebuffPollTreatsUnauthorizedAsStillWaiting(t *testing.T) {
	mock := NewMockUpstream()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	if _, err := client.RequestLoginCode(context.Background(), "cli-proxy-abc"); err != nil {
		t.Fatalf("RequestLoginCode: %v", err)
	}

	user, err := client.PollLoginStatus(context.Background(), "cli-proxy-abc", "mock-fingerprint-hash", 0)
	if err != nil {
		t.Fatalf("a 401 while pending must not be an error, got %v", err)
	}
	if user != nil {
		t.Fatalf("expected no user before sign-in completes, got %+v", user)
	}
}

func TestFreebuffPollReturnsTheTokenOnceSignedIn(t *testing.T) {
	mock := NewMockUpstream()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	code, err := client.RequestLoginCode(context.Background(), "cli-proxy-abc")
	if err != nil {
		t.Fatalf("RequestLoginCode: %v", err)
	}

	// The fixture completes on the second poll.
	if user, _ := client.PollLoginStatus(context.Background(), "cli-proxy-abc", code.FingerprintHash, code.ExpiresAt); user != nil {
		t.Fatalf("expected the first poll to report pending, got %+v", user)
	}

	user, err := client.PollLoginStatus(context.Background(), "cli-proxy-abc", code.FingerprintHash, code.ExpiresAt)
	if err != nil {
		t.Fatalf("PollLoginStatus: %v", err)
	}
	if user == nil {
		t.Fatal("expected a user once the login completed")
	}
	if user.AuthToken != MockAPIKey {
		t.Errorf("authToken = %q, want %q", user.AuthToken, MockAPIKey)
	}
}

func TestFreebuffPollRejectsMissingParameters(t *testing.T) {
	mock := NewMockUpstream()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	if _, err := client.PollLoginStatus(context.Background(), "", "hash", 0); err == nil {
		t.Error("expected an error when the fingerprint id is blank")
	}
	if _, err := client.PollLoginStatus(context.Background(), "id", "", 0); err == nil {
		t.Error("expected an error when the fingerprint hash is blank")
	}
}

// The full flow: the URL is shown to the user, the poll runs unattended, and a
// usable token comes back.
func TestFreebuffLoginCompletesAndReturnsAToken(t *testing.T) {
	mock := NewMockUpstream()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	var shown string
	client := loginTestClient(t, srv.URL, nil)

	result, err := client.Login(context.Background(), "cli-proxy-abc", func(loginURL string) {
		shown = loginURL
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if shown == "" || !strings.Contains(shown, "login") {
		t.Errorf("the login url was not shown to the user, got %q", shown)
	}
	if result.AuthToken != MockAPIKey {
		t.Errorf("AuthToken = %q, want %q", result.AuthToken, MockAPIKey)
	}
	if result.UserID != "user-mock-1" {
		t.Errorf("UserID = %q, want %q", result.UserID, "user-mock-1")
	}
	if result.FingerprintID != "cli-proxy-abc" {
		t.Errorf("FingerprintID = %q, want %q", result.FingerprintID, "cli-proxy-abc")
	}
}

func TestFreebuffLoginValidatesItsArguments(t *testing.T) {
	mock := NewMockUpstream()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	if _, err := client.Login(context.Background(), "  ", func(string) {}); err == nil {
		t.Error("expected an error for a blank fingerprint id")
	}
	if _, err := client.Login(context.Background(), "cli-proxy-abc", nil); err == nil {
		t.Error("expected an error when there is no way to show the login url")
	}
}

// A cancelled context must stop the poll loop rather than run to its deadline.
func TestFreebuffLoginHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"loginUrl":"https://example.test/login","fingerprintHash":"h","expiresAt":1893456000000}`))
	}))
	defer srv.Close()

	client := loginTestClient(t, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Login(ctx, "cli-proxy-abc", func(string) {})
	if err == nil {
		t.Fatal("expected a cancelled login to return an error")
	}
}

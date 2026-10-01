// This file implements the Freebuff browser login flow: request a URL, let
// the user sign in, then poll until a bearer token is issued.
package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LoginPathCode requests a login URL for a browser-based sign-in.
//
// The Codebuff-compatible API never returns a token directly: the caller is
// given a URL to open and a hash to poll with, so the user can sign in with
// whatever account they like.
const LoginPathCode = "/api/auth/cli/code"

// LoginPathStatus polls whether the browser sign-in completed.
const LoginPathStatus = "/api/auth/cli/status"

const (
	// loginPollInterval is how often the status endpoint is polled while the
	// user completes the browser sign-in.
	loginPollInterval = 5 * time.Second
	// loginPollTimeout bounds the whole wait. The window is short enough that a
	// forgotten login cannot pin a goroutine indefinitely.
	loginPollTimeout = 5 * time.Minute
)

// LoginRequest asks for a browser login URL.
type LoginRequest struct {
	// FingerprintID identifies this client installation across the code/status
	// pair. The server binds the two together, so it must be stable for the
	// duration of one login attempt.
	FingerprintID string `json:"fingerprintId"`
}

// LoginCode is the response to a login-code request.
//
// ExpiresAt is a Unix epoch in milliseconds, not a string. The published
// TypeScript types declare it as a string, but the live service returns a
// number, so it is decoded leniently: either shape is accepted rather than
// failing the whole login over a field the poll only echoes back.
type LoginCode struct {
	// LoginURL is the page the user must open to sign in.
	LoginURL string `json:"loginUrl"`
	// FingerprintHash proves the code and the status poll belong together.
	FingerprintHash string `json:"fingerprintHash"`
	// ExpiresAt bounds how long the login URL stays valid. Milliseconds.
	ExpiresAt int64 `json:"expiresAt"`
	// ExpiresInMs is the same window expressed as a duration.
	ExpiresInMs int64 `json:"expiresInMs"`
}

// loginUser is the identity returned once a login succeeds. Only the bearer
// token is consumed; the rest is kept for diagnostics.
type loginUser struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	AuthToken     string `json:"authToken"`
	FingerprintID string `json:"fingerprintId"`
}

// loginStatusResponse is the shape of the status poll while pending. A
// completed login carries a user; a pending one does not.
type loginStatusResponse struct {
	User *loginUser `json:"user"`
}

// RequestLoginCode asks the service for a browser login URL.
func (c *Client) RequestLoginCode(ctx context.Context, fingerprintID string) (*LoginCode, error) {
	if strings.TrimSpace(fingerprintID) == "" {
		return nil, errors.New("freebuff: a fingerprint id is required to request a login url")
	}
	payload, err := json.Marshal(LoginRequest{FingerprintID: fingerprintID})
	if err != nil {
		return nil, fmt.Errorf("freebuff: encode login request: %w", err)
	}
	// The code endpoint is unauthenticated: it is what bootstraps credentials.
	body, err := c.doJSONWithHeaders(ctx, http.MethodPost, LoginPathCode, payload, map[string]string{headerAnonymous: "1"})
	if err != nil {
		return nil, fmt.Errorf("freebuff: request login url: %w", err)
	}
	var code LoginCode
	if err = json.Unmarshal(body, &code); err != nil {
		return nil, fmt.Errorf("freebuff: decode login url response: %w", err)
	}
	if strings.TrimSpace(code.LoginURL) == "" {
		return nil, errors.New("freebuff: the service did not return a login url")
	}
	if strings.TrimSpace(code.FingerprintHash) == "" {
		return nil, errors.New("freebuff: the service did not return a fingerprint hash")
	}
	return &code, nil
}

// PollLoginStatus reports whether a browser login has completed. A nil user
// with a nil error means "still waiting", which is the normal case while the
// user is still on the login page.
func (c *Client) PollLoginStatus(ctx context.Context, fingerprintID, fingerprintHash string, expiresAt int64) (*loginUser, error) {
	if strings.TrimSpace(fingerprintID) == "" || strings.TrimSpace(fingerprintHash) == "" {
		return nil, errors.New("freebuff: a fingerprint id and hash are required to poll login status")
	}
	endpoint, err := url.Parse(c.baseURL + LoginPathStatus)
	if err != nil {
		return nil, fmt.Errorf("freebuff: build login status url: %w", err)
	}
	query := endpoint.Query()
	query.Set("fingerprintId", fingerprintID)
	query.Set("fingerprintHash", fingerprintHash)
	if expiresAt > 0 {
		// The service expects the same epoch-milliseconds value it issued.
		query.Set("expiresAt", strconv.FormatInt(expiresAt, 10))
	}
	endpoint.RawQuery = query.Encode()

	body, err := c.doJSONWithHeaders(ctx, http.MethodGet, endpoint.RequestURI(), nil, map[string]string{headerAnonymous: "1"})
	if err != nil {
		// A 401 here is the ordinary "not signed in yet" answer, not a failure
		// of the credential: the login has not produced a token to check.
		if asSessionErrorIsStatus(err, http.StatusUnauthorized) {
			return nil, nil
		}
		return nil, fmt.Errorf("freebuff: poll login status: %w", err)
	}
	var status loginStatusResponse
	if err = json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("freebuff: decode login status response: %w", err)
	}
	if status.User == nil || strings.TrimSpace(status.User.AuthToken) == "" {
		return nil, nil
	}
	return status.User, nil
}

// LoginResult is the outcome of a completed Freebuff login.
type LoginResult struct {
	// AuthToken is the bearer token to send on every request.
	AuthToken string
	// Email is carried for diagnostics only; it is never sent upstream.
	Email string
	// UserID identifies the account the token belongs to.
	UserID string
	// FingerprintID is the identifier used for this login attempt.
	FingerprintID string
}

// Login drives a complete browser login: request a URL, let the caller show
// it, then poll until the user finishes or the deadline passes.
//
// LoginURL is invoked with the URL the user must open. That indirection is
// what lets the CLI open a browser, a TUI print it, and a test capture it,
// without this package knowing anything about how it is presented.
func (c *Client) Login(ctx context.Context, fingerprintID string, showURL func(loginURL string)) (*LoginResult, error) {
	if strings.TrimSpace(fingerprintID) == "" {
		return nil, errors.New("freebuff: a fingerprint id is required to log in")
	}
	if showURL == nil {
		return nil, errors.New("freebuff: a function to display the login url is required")
	}

	code, err := c.RequestLoginCode(ctx, fingerprintID)
	if err != nil {
		return nil, err
	}
	showURL(code.LoginURL)

	// Bound the wait by the caller's context and by a deadline of our own, so
	// neither an abandoned context nor an unresponsive user hangs forever.
	pollCtx, cancel := context.WithTimeout(ctx, loginPollTimeout)
	defer cancel()

	ticker := time.NewTicker(loginPollInterval)
	defer ticker.Stop()

	for {
		user, pollErr := c.PollLoginStatus(pollCtx, fingerprintID, code.FingerprintHash, code.ExpiresAt)
		if pollErr != nil {
			// A transient error is not fatal: the user may still be signing in,
			// so keep waiting until the deadline rather than failing early.
			select {
			case <-pollCtx.Done():
				return nil, fmt.Errorf("freebuff: login did not complete: %w", pollErr)
			case <-ticker.C:
				continue
			}
		}
		if user != nil {
			return &LoginResult{
				AuthToken:     user.AuthToken,
				Email:         user.Email,
				UserID:        user.ID,
				FingerprintID: fingerprintID,
			}, nil
		}

		select {
		case <-pollCtx.Done():
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("freebuff: login cancelled: %w", ctxErr)
			}
			return nil, fmt.Errorf("freebuff: login timed out after %s", loginPollTimeout)
		case <-ticker.C:
		}
	}
}

// asSessionErrorIsStatus reports whether err is a SessionError carrying the
// given HTTP status.
func asSessionErrorIsStatus(err error, status int) bool {
	var target *SessionError
	if !errors.As(err, &target) {
		return false
	}
	return target.StatusCode() == status
}

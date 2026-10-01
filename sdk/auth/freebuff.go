package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/freebuff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// FreebuffAuthenticator logs in to Freebuff through a browser sign-in.
//
// The service never returns a token directly. It hands back a URL to open and
// a hash to poll with, so the user signs in with whatever account they like and
// this polls until a token is issued. There is no redirect to intercept and no
// PKCE exchange, because the token is bound to the browser session rather than
// to a redirect.
type FreebuffAuthenticator struct {
	// BaseURL overrides the configured endpoint. Empty means read
	// `freebuff.base-url` from the config.
	BaseURL string
}

// NewFreebuffAuthenticator constructs a new Freebuff authenticator.
func NewFreebuffAuthenticator() *FreebuffAuthenticator {
	return &FreebuffAuthenticator{}
}

// Provider returns the unique provider identifier for Freebuff.
func (a *FreebuffAuthenticator) Provider() string {
	return "freebuff"
}

// RefreshLead returns nil because a Freebuff token does not expire on a
// schedule this client can predict. A rejected token is handled by the
// conductor cooling that credential, not by a proactive refresh.
func (a *FreebuffAuthenticator) RefreshLead() *time.Duration {
	return nil
}

// freebuffFingerprintID returns the identifier that ties a login code to its
// status poll. It is generated locally per attempt and never sent anywhere but
// the two login endpoints.
func freebuffFingerprintID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("freebuff: generate login fingerprint: %w", err)
	}
	return "cli-proxy-" + hex.EncodeToString(buf), nil
}

// Login performs a browser login and returns a credential ready to persist.
func (a *FreebuffAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	baseURL := a.BaseURL
	if baseURL == "" {
		if cfg == nil {
			return nil, fmt.Errorf("freebuff: a configuration is required to resolve the base URL")
		}
		baseURL = cfg.Freebuff.BaseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("freebuff: no base URL configured. Set `freebuff.base-url` before running login")
	}

	fingerprintID, err := freebuffFingerprintID()
	if err != nil {
		return nil, err
	}

	// The client is built without a token: the login endpoints are reached
	// before a credential exists.
	client, err := freebuff.NewClient(baseURL, "", freebuff.Options{})
	if err != nil {
		return nil, fmt.Errorf("freebuff: %w", err)
	}

	noBrowser := opts.NoBrowser
	result, err := client.Login(ctx, fingerprintID, func(loginURL string) {
		if noBrowser {
			fmt.Printf("Visit the following URL to sign in to Freebuff:\n%s\n", loginURL)
			return
		}
		fmt.Println("Opening browser for Freebuff authentication")
		if !browser.IsAvailable() {
			log.Warn("No browser available; please open the URL manually")
			fmt.Printf("Visit the following URL to sign in to Freebuff:\n%s\n", loginURL)
			return
		}
		if errOpen := browser.OpenURL(loginURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
			fmt.Printf("Visit the following URL to sign in to Freebuff:\n%s\n", loginURL)
		}
	})
	if err != nil {
		return nil, err
	}

	authID := fmt.Sprintf("freebuff:%s", result.UserID)
	if result.UserID == "" {
		// Fall back to a stable id derived from the fingerprint so repeated
		// logins from one machine do not silently create duplicates.
		authID = fmt.Sprintf("freebuff:%s", fingerprintID)
	}

	metadata := map[string]any{
		"fingerprint_id": fingerprintID,
	}
	if result.Email != "" {
		metadata["email"] = result.Email
	}

	attrs := map[string]string{
		"api_key":  result.AuthToken,
		"base_url": baseURL,
	}

	return &coreauth.Auth{
		ID:         authID,
		Provider:   "freebuff",
		Status:     coreauth.StatusActive,
		Attributes: attrs,
		Metadata:   metadata,
	}, nil
}

// compile-time check that FreebuffAuthenticator satisfies Authenticator.
var _ Authenticator = (*FreebuffAuthenticator)(nil)

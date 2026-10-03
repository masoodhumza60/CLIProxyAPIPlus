package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/freebuff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

// freebuffCredentialFileName builds the on-disk name for a Freebuff account.
//
// This value is also the credential's identity, and it becomes a file name
// verbatim, so it has to survive the filesystem it lands on. The identifier used
// to be joined with a colon - "freebuff:<user id>" - which is legal on Linux and
// macOS and rejected outright on Windows. Nothing reported the failure: the
// write produced an empty file named after the part before the colon, the
// account vanished from the credential list, and a sign-in that had reported
// success appeared to have done nothing at all.
//
// So the name is reduced to characters every filesystem accepts, keeping the
// shape the other providers already use: provider, separator, identifier, .json.
func freebuffCredentialFileName(identifier string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, strings.TrimSpace(identifier))
	if safe == "" {
		// An account that reported no identifier still needs a stable, writable
		// name; the fingerprint-derived identifier normally prevents this.
		return "freebuff-account.json"
	}
	return fmt.Sprintf("freebuff-%s.json", safe)
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

	// The login endpoints are reached before a credential exists, so this uses
	// the constructor that permits an empty key. NewClient deliberately rejects
	// one, because an authenticated client with no key would send an empty
	// bearer token, which reads as a rejected credential rather than a mistake.
	client, err := freebuff.NewLoginClient(baseURL, freebuff.Options{})
	if err != nil {
		return nil, fmt.Errorf("freebuff: %w", err)
	}

	noBrowser := opts.NoBrowser
	result, err := client.Login(ctx, fingerprintID, func(loginURL string) {
		// Hand the URL to the caller first: a desktop application has to show
		// it, and it can only do that if it is told before we try to open a
		// browser the process may not have one to open.
		if onLoginURL := opts.OnLoginURL; onLoginURL != nil {
			onLoginURL(loginURL)
		}
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

	identifier := result.UserID
	if identifier == "" {
		// Fall back to a stable id derived from the fingerprint so repeated
		// logins from one machine do not silently create duplicates.
		identifier = fingerprintID
	}
	fileName := freebuffCredentialFileName(identifier)

	// The token has to appear in both maps, not just one. Attributes drive the
	// in-memory record, but the credential file is serialised from Metadata, so
	// a record carrying the token only in Attributes saves as a credential with
	// nothing in it - present, listed, and rejected on every request.
	attrs := map[string]string{
		"api_key":  result.AuthToken,
		"base_url": baseURL,
	}
	if result.UserID != "" {
		attrs["user_id"] = result.UserID
	}
	if result.Email != "" {
		attrs["email"] = result.Email
	}

	metadata := map[string]any{
		"type":           "freebuff",
		"api_key":        result.AuthToken,
		"base_url":       baseURL,
		"fingerprint_id": fingerprintID,
	}
	if result.Email != "" {
		metadata["email"] = result.Email
	}
	if result.UserID != "" {
		metadata["user_id"] = result.UserID
	}

	return &coreauth.Auth{
		ID:         fileName,
		FileName:   fileName,
		Provider:   "freebuff",
		Status:     coreauth.StatusActive,
		Attributes: attrs,
		Metadata:   metadata,
	}, nil
}

// compile-time check that FreebuffAuthenticator satisfies Authenticator.
var _ Authenticator = (*FreebuffAuthenticator)(nil)

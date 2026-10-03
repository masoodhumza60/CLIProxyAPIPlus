package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A credential saved by the sign-in flow reaches us as a file: the loader puts
// the document in Metadata and leaves Attributes holding only routing keys.
// Reading Attributes alone therefore finds no token, and the account is listed
// while every model lookup for it comes back empty.
func TestFreebuffCredentialReadsTokenFromFileCredential(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "freebuff:54cafe30",
		Provider: "freebuff",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"path":      "..\\oauth\\freebuff:54cafe30",
		},
		Metadata: map[string]any{
			"type":     "freebuff",
			"api_key":  "test-token-from-metadata",
			"base_url": "https://freebuff.com",
		},
	}

	apiKey, webBaseURL := freebuffCredentialFor(auth)
	if apiKey != "test-token-from-metadata" {
		t.Fatalf("token was not found on a credential loaded from a file: %q", apiKey)
	}
	if webBaseURL != "https://freebuff.com" {
		t.Fatalf("base url = %q, want the saved origin", webBaseURL)
	}
}

// An auth built in memory, such as one from configuration, carries its token in
// Attributes. That shape has to keep working.
func TestFreebuffCredentialReadsTokenFromInMemoryAuth(t *testing.T) {
	auth := &coreauth.Auth{
		ID:         "freebuff:config",
		Provider:   "freebuff",
		Attributes: map[string]string{"api_key": "config-token"},
	}

	apiKey, webBaseURL := freebuffCredentialFor(auth)
	if apiKey != "config-token" {
		t.Fatalf("token = %q, want the value from Attributes", apiKey)
	}
	if webBaseURL == "" {
		t.Fatal("expected a default web origin when none is configured")
	}
}

// A document on disk is the record of what the user signed in with, so it wins
// over a routing attribute of the same name.
func TestFreebuffCredentialPrefersTheSavedDocument(t *testing.T) {
	auth := &coreauth.Auth{
		Attributes: map[string]string{"api_key": "stale-attribute"},
		Metadata:   map[string]any{"api_key": "saved-token"},
	}

	if apiKey, _ := freebuffCredentialFor(auth); apiKey != "saved-token" {
		t.Fatalf("token = %q, want the saved document to win", apiKey)
	}
}

// Metadata is not guaranteed to hold strings; a value of another type must not
// panic, and must fall through rather than shadow a usable attribute.
func TestFreebuffCredentialToleratesNonStringMetadata(t *testing.T) {
	auth := &coreauth.Auth{
		Attributes: map[string]string{"api_key": "attribute-token"},
		Metadata:   map[string]any{"api_key": 12345},
	}

	if apiKey, _ := freebuffCredentialFor(auth); apiKey != "attribute-token" {
		t.Fatalf("token = %q, want a non-string metadata value to be ignored", apiKey)
	}
}

func TestFreebuffCredentialHandlesNil(t *testing.T) {
	apiKey, webBaseURL := freebuffCredentialFor(nil)
	if apiKey != "" {
		t.Fatalf("token = %q, want empty", apiKey)
	}
	if webBaseURL == "" {
		t.Fatal("expected the default web origin for a nil credential")
	}
}

package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A credential saved by `freebuff login` keeps its token in Metadata, because
// the credential file is the document and it is loaded verbatim into Metadata
// with Attributes left holding only routing keys. Reading Attributes alone made
// every signed-in account look unusable, while a key configured in config.yaml
// worked fine - which is why this went unnoticed for as long as it did.
func TestFreebuffAuthValueReadsTheSavedDocument(t *testing.T) {
	saved := &cliproxyauth.Auth{
		ID:       "freebuff-54cafe30",
		Provider: "freebuff",
		Metadata: map[string]any{
			"type":     "freebuff",
			"api_key":  "saved-token",
			"base_url": "https://freebuff.com",
		},
		Attributes: map[string]string{
			"source_backend": "file",
		},
	}
	if got := freebuffAPIKey(saved); got != "saved-token" {
		t.Errorf("freebuffAPIKey = %q, want %q", got, "saved-token")
	}
	if got := (&FreebuffExecutor{}).freebuffBaseURL(saved); got != "https://freebuff.com" {
		t.Errorf("freebuffBaseURL = %q, want %q", got, "https://freebuff.com")
	}
}

func TestFreebuffAuthValueReadsInMemoryCredentials(t *testing.T) {
	inMemory := &cliproxyauth.Auth{
		ID:       "freebuff:apikey:abc",
		Provider: "freebuff",
		Attributes: map[string]string{
			"api_key":  "configured-token",
			"base_url": "https://freebuff.com",
		},
	}
	if got := freebuffAPIKey(inMemory); got != "configured-token" {
		t.Errorf("freebuffAPIKey = %q, want %q", got, "configured-token")
	}
	if got := (&FreebuffExecutor{}).freebuffBaseURL(inMemory); got != "https://freebuff.com" {
		t.Errorf("freebuffBaseURL = %q, want %q", got, "https://freebuff.com")
	}
}

func TestFreebuffAuthValueEdgeCases(t *testing.T) {
	if got := freebuffAPIKey(nil); got != "" {
		t.Errorf("nil credential = %q, want empty", got)
	}
	// A non-string value must not panic or be coerced.
	odd := &cliproxyauth.Auth{Metadata: map[string]any{"api_key": 42}}
	if got := freebuffAPIKey(odd); got != "" {
		t.Errorf("non-string metadata value = %q, want empty", got)
	}
	// An empty saved value falls through to Attributes rather than reporting a
	// blank key, so a partially written document still resolves.
	partial := &cliproxyauth.Auth{
		Metadata:   map[string]any{"api_key": "   "},
		Attributes: map[string]string{"api_key": "fallback-token"},
	}
	if got := freebuffAPIKey(partial); got != "fallback-token" {
		t.Errorf("blank metadata value = %q, want %q", got, "fallback-token")
	}
	if got := (&FreebuffExecutor{}).freebuffBaseURL(&cliproxyauth.Auth{}); got != "" {
		t.Errorf("empty credential base URL = %q, want empty", got)
	}
}

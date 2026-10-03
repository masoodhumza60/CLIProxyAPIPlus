package auth

import (
	"strings"
	"testing"
)

// A colon is rejected by Windows, which is where this project runs, and a
// credential name that cannot be written produces a sign-in that reports
// success while storing nothing.
func TestFreebuffCredentialFileNameIsWritableOnWindows(t *testing.T) {
	cases := []string{
		"54cafe30-da04-47d1-8b17-aa64d4ff2209",
		"54cafe30:da04:47d1:8b17:aa64d4ff2209",
		"user@example.com",
		"../../escape",
		`back\slash`,
		"a b c",
	}
	for _, identifier := range cases {
		name := freebuffCredentialFileName(identifier)
		for _, forbidden := range []string{":", `\`, "/", " ", "*", "?", "\"", "<", ">", "|"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("name %q for identifier %q contains %q", name, identifier, forbidden)
			}
		}
		if !strings.HasSuffix(name, ".json") {
			t.Errorf("name %q for identifier %q is not a .json file", name, identifier)
		}
		if !strings.HasPrefix(name, "freebuff-") {
			t.Errorf("name %q for identifier %q is missing the provider prefix", name, identifier)
		}
		if strings.Contains(name, "..") {
			t.Errorf("name %q for identifier %q can escape its directory", name, identifier)
		}
	}
}

// The same account must always produce the same name, or every sign-in would
// add another copy of the same credential.
func TestFreebuffCredentialFileNameIsStable(t *testing.T) {
	first := freebuffCredentialFileName("54cafe30-da04-47d1-8b17-aa64d4ff2209")
	second := freebuffCredentialFileName("54cafe30-da04-47d1-8b17-aa64d4ff2209")
	if first != second {
		t.Fatalf("name changed between calls: %q then %q", first, second)
	}
}

func TestFreebuffCredentialFileNameFallsBackWhenUnusable(t *testing.T) {
	if name := freebuffCredentialFileName(""); name != "freebuff-account.json" {
		t.Fatalf("empty identifier produced %q", name)
	}
	if name := freebuffCredentialFileName("///"); name != "freebuff-account.json" {
		t.Fatalf("identifier of only separators produced %q", name)
	}
}

// The shape every other provider already uses, so the credential list reads the
// same way regardless of who signed in.
func TestFreebuffCredentialFileNameMatchesProviderConvention(t *testing.T) {
	name := freebuffCredentialFileName("54cafe30-da04-47d1-8b17-aa64d4ff2209")
	if name != "freebuff-54cafe30-da04-47d1-8b17-aa64d4ff2209.json" {
		t.Fatalf("name = %q", name)
	}
}

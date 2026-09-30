package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func qoderSynthesisContext(cfg *config.Config) *SynthesisContext {
	return &SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
}

func TestSynthesizeQoderKeysAttributes(t *testing.T) {
	cfg := &config.Config{
		Qoder: config.QoderConfig{
			Enabled: true,
			Keys: []config.QoderKey{
				{
					Name:    "cn-key",
					Token:   "pat-test-token",
					Backend: "cn",
					Models:  []string{"qoder-cn"},
				},
				{
					// A global key authenticates through the `qodercli login`
					// session, so it carries no token.
					Name:    "global-key",
					Backend: "Global ",
				},
			},
		},
	}

	entries := (&ConfigSynthesizer{}).synthesizeQoderKeys(qoderSynthesisContext(cfg))
	if len(entries) != 2 {
		t.Fatalf("expected 2 auth entries, got %d", len(entries))
	}

	cn := entries[0]
	if cn.Provider != "qoder" {
		t.Errorf("provider = %q, want %q", cn.Provider, "qoder")
	}
	if got := cn.Attributes["backend"]; got != registry.QoderBackendCN {
		t.Errorf("cn backend = %q, want %q", got, registry.QoderBackendCN)
	}
	if got := cn.Attributes["api_key"]; got != "pat-test-token" {
		t.Errorf("cn api_key = %q, want %q", got, "pat-test-token")
	}
	if got := cn.Attributes["name"]; got != "cn-key" {
		t.Errorf("cn name = %q, want %q", got, "cn-key")
	}
	// The ID must come from the stable ID generator, not the user-facing name,
	// so renaming a key does not churn cooldown/usage state.
	if cn.ID == "cn-key" {
		t.Error("auth ID must not be the config key name")
	}
	if !hasPrefix(cn.ID, "qoder:apikey:") {
		t.Errorf("auth ID = %q, want an idGen-generated qoder:apikey id", cn.ID)
	}

	global := entries[1]
	if got := global.Attributes["backend"]; got != registry.QoderBackendGlobal {
		t.Errorf("global backend = %q, want %q (backend must be normalized)", got, registry.QoderBackendGlobal)
	}
	if _, ok := global.Attributes["api_key"]; ok {
		t.Error("a tokenless global key must not publish an api_key attribute")
	}
	if cn.ID == global.ID {
		t.Error("distinct qoder keys must produce distinct auth IDs")
	}
}

func TestSynthesizeQoderKeysBackendDefaultsToGlobal(t *testing.T) {
	cfg := &config.Config{
		Qoder: config.QoderConfig{
			Enabled: true,
			Keys:    []config.QoderKey{{Name: "no-backend"}},
		},
	}

	entries := (&ConfigSynthesizer{}).synthesizeQoderKeys(qoderSynthesisContext(cfg))
	if len(entries) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(entries))
	}
	// Defaulting to CN would demand a PAT and a second binary the user most
	// likely does not have; global works with `qodercli login` alone.
	if got := entries[0].Attributes["backend"]; got != registry.QoderBackendGlobal {
		t.Errorf("default backend = %q, want %q", got, registry.QoderBackendGlobal)
	}
}

func TestSynthesizeQoderKeysDisabledProducesNoCredentials(t *testing.T) {
	// Keys are deliberately non-empty: the flag alone must suppress synthesis.
	cfg := &config.Config{
		Qoder: config.QoderConfig{
			Enabled: false,
			Keys: []config.QoderKey{
				{Name: "a", Token: "pat-a", Backend: "cn"},
				{Name: "b", Token: "pat-b", Backend: "global"},
			},
		},
	}

	entries := (&ConfigSynthesizer{}).synthesizeQoderKeys(qoderSynthesisContext(cfg))
	if len(entries) != 0 {
		t.Fatalf("expected 0 auth entries when qoder.enabled is false, got %d", len(entries))
	}
}

func TestSynthesizeQoderKeysSkipsUnnamedKeys(t *testing.T) {
	cfg := &config.Config{
		Qoder: config.QoderConfig{
			Enabled: true,
			Keys:    []config.QoderKey{{Name: "   "}, {Name: "kept", Backend: "global"}},
		},
	}

	entries := (&ConfigSynthesizer{}).synthesizeQoderKeys(qoderSynthesisContext(cfg))
	if len(entries) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(entries))
	}
	if got := entries[0].Attributes["name"]; got != "kept" {
		t.Errorf("name = %q, want %q", got, "kept")
	}
}

func TestSynthesizeQoderKeysIDsAreStableAcrossRuns(t *testing.T) {
	cfg := &config.Config{
		Qoder: config.QoderConfig{
			Enabled: true,
			Keys:    []config.QoderKey{{Name: "stable", Token: "pat", Backend: "cn"}},
		},
	}

	first := (&ConfigSynthesizer{}).synthesizeQoderKeys(qoderSynthesisContext(cfg))
	second := (&ConfigSynthesizer{}).synthesizeQoderKeys(qoderSynthesisContext(cfg))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected 1 entry per run, got %d and %d", len(first), len(second))
	}
	if first[0].ID != second[0].ID {
		t.Errorf("auth ID is not stable across synthesis runs: %q vs %q", first[0].ID, second[0].ID)
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

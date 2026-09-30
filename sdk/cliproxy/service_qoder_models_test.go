package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// qoderTestService builds a Service carrying the supplied Qoder keys.
func qoderTestService(keys ...config.QoderKey) *Service {
	return &Service{
		cfg: &config.Config{
			Qoder: config.QoderConfig{Enabled: true, Keys: keys},
		},
	}
}

func TestResolveConfigQoderKeyMatchesOnTokenAndBackend(t *testing.T) {
	svc := qoderTestService(
		config.QoderKey{Name: "cn", Token: "cn-pat", Backend: registry.QoderBackendCN},
		config.QoderKey{Name: "global", Backend: registry.QoderBackendGlobal},
	)

	auth := &coreauth.Auth{
		ID:       "qoder:cn",
		Provider: "qoder",
		Attributes: map[string]string{
			"api_key": "cn-pat",
			"backend": registry.QoderBackendCN,
		},
	}

	entry := svc.resolveConfigQoderKey(auth)
	if entry == nil {
		t.Fatal("expected the CN key to resolve, got nil")
	}
	if entry.Name != "cn" {
		t.Errorf("resolved %q, want the CN key", entry.Name)
	}
}

func TestResolveConfigQoderKeyMatchesTokenlessGlobalKey(t *testing.T) {
	// The global backend authenticates through the `qodercli login` OAuth
	// session, so its synthesized auth carries no token. It must still resolve
	// to the global key, otherwise its `models:` allow-list is silently lost.
	svc := qoderTestService(
		config.QoderKey{Name: "cn", Token: "cn-pat", Backend: registry.QoderBackendCN},
		config.QoderKey{Name: "global", Backend: registry.QoderBackendGlobal},
	)

	auth := &coreauth.Auth{
		ID:         "qoder:global",
		Provider:   "qoder",
		Attributes: map[string]string{"backend": registry.QoderBackendGlobal},
	}

	entry := svc.resolveConfigQoderKey(auth)
	if entry == nil {
		t.Fatal("expected the tokenless global key to resolve, got nil")
	}
	if entry.Name != "global" {
		t.Errorf("resolved %q, want the global key", entry.Name)
	}
}

func TestResolveConfigQoderKeyReturnsNilWhenUnmatched(t *testing.T) {
	svc := qoderTestService(config.QoderKey{Name: "global", Backend: registry.QoderBackendGlobal})

	if got := svc.resolveConfigQoderKey(nil); got != nil {
		t.Errorf("nil auth resolved to %q, want nil", got.Name)
	}
	unrelated := &coreauth.Auth{
		ID:         "qoder:other",
		Provider:   "qoder",
		Attributes: map[string]string{"api_key": "some-other-token"},
	}
	if got := svc.resolveConfigQoderKey(unrelated); got != nil {
		t.Errorf("unrelated auth resolved to %q, want nil", got.Name)
	}
}

func TestResolveConfigQoderKeyHonoursConfigIndex(t *testing.T) {
	// The synthesizer records the source config index, which is the most precise
	// way to map an auth back to its key. It is only trusted for config-sourced
	// auth, so the `source` attribute must be present as well.
	svc := qoderTestService(
		config.QoderKey{Name: "first", Backend: registry.QoderBackendGlobal},
		config.QoderKey{Name: "second", Backend: registry.QoderBackendGlobal},
	)

	auth := &coreauth.Auth{
		ID:       "qoder:apikey:abc",
		Provider: "qoder",
		Attributes: map[string]string{
			coreauth.AttributeSource:      "config:qoder[abc]",
			coreauth.AttributeConfigIndex: "1",
		},
	}

	entry := svc.resolveConfigQoderKey(auth)
	if entry == nil {
		t.Fatal("expected the indexed key to resolve, got nil")
	}
	if entry.Name != "second" {
		t.Errorf("resolved %q, want the indexed key %q", entry.Name, "second")
	}
}

func TestBuildQoderConfigModelsAppliesAllowList(t *testing.T) {
	discovered := []*ModelInfo{{ID: "Qwen3.8-Max"}, {ID: "Qwen3.8-Flash"}}

	t.Run("no allow-list keeps everything", func(t *testing.T) {
		got := buildQoderConfigModels(&config.QoderKey{Name: "global"}, discovered)
		if len(got) != 2 {
			t.Errorf("got %d models, want both discovered models", len(got))
		}
	})

	t.Run("allow-list narrows case-insensitively", func(t *testing.T) {
		got := buildQoderConfigModels(&config.QoderKey{Models: []string{" qwen3.8-flash "}}, discovered)
		if len(got) != 1 || got[0].ID != "Qwen3.8-Flash" {
			t.Errorf("got %v, want only Qwen3.8-Flash", got)
		}
	})

	t.Run("allow-listing an undiscovered model yields nothing", func(t *testing.T) {
		got := buildQoderConfigModels(&config.QoderKey{Models: []string{"qoder-cn"}}, discovered)
		if len(got) != 0 {
			t.Errorf("got %v, want nothing", got)
		}
	})

	t.Run("nil entry yields nothing", func(t *testing.T) {
		if got := buildQoderConfigModels(nil, discovered); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

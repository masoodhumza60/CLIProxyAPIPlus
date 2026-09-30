package executor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestParseQoderModelListSkipsBannerAndHeader(t *testing.T) {
	// The CLI prints a banner, then a MODEL header, then one name per line.
	output := "Qoder CLI 1.1.64\nMODEL\nQwen3.8-Max\nQwen3.8-Flash\n\n"

	got := ParseQoderModelList(output)
	want := []string{"Qwen3.8-Max", "Qwen3.8-Flash"}
	if len(got) != len(want) {
		t.Fatalf("got %d models %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseQoderModelListDeduplicates(t *testing.T) {
	got := ParseQoderModelList("MODEL\nQwen3.8-Max\nQwen3.8-Max\nQwen3.8-Flash\n")
	if len(got) != 2 {
		t.Fatalf("got %d models %v, want 2 after de-duplication", len(got), got)
	}
}

func TestParseQoderModelListRequiresHeader(t *testing.T) {
	// Without the header line nothing is treated as a model, so a banner-only
	// response can never be mistaken for a model list.
	if got := ParseQoderModelList("some banner\nmore banner\n"); len(got) != 0 {
		t.Errorf("got %v, want no models when the header is absent", got)
	}
}

func TestQoderBinaryForBackend(t *testing.T) {
	t.Setenv(qoderPathEnv, "")

	cases := map[string]string{
		registry.QoderBackendCN:     "qoderclicn",
		registry.QoderBackendGlobal: "qodercli",
		"GLOBAL":                    "qodercli",
		// Unknown backends are rejected at config load; the lookup falls back to
		// the CN binary so a typo cannot silently reach the global endpoint.
		"mars": "qoderclicn",
	}
	for backend, want := range cases {
		if got := QoderBinaryForBackend(backend); got != want {
			t.Errorf("QoderBinaryForBackend(%q) = %q, want %q", backend, got, want)
		}
	}
}

func TestQoderBinaryForBackendHonoursPathOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "qodercli-custom")
	t.Setenv(qoderPathEnv, override)

	if got := QoderBinaryForBackend(registry.QoderBackendGlobal); got != override {
		t.Errorf("QODER_PATH was ignored: got %q, want %q", got, override)
	}
}

func TestQoderModelInfosPreserveDiscoveredNames(t *testing.T) {
	// The CLI substitutes unknown model names and prints a warning to stderr,
	// so the ID must be the exact discovered name or `--model` will not
	// round-trip.
	infos := qoderModelInfos([]string{"Qwen3.8-Max", ""})
	if len(infos) != 1 {
		t.Fatalf("got %d infos, want 1 (empty names are dropped)", len(infos))
	}
	if infos[0].ID != "Qwen3.8-Max" || infos[0].Name != "Qwen3.8-Max" {
		t.Errorf("ID/Name = %q/%q, want the discovered name verbatim", infos[0].ID, infos[0].Name)
	}
	if infos[0].OwnedBy != "qoder" || infos[0].Object != "model" {
		t.Errorf("unexpected ownership metadata: %+v", infos[0])
	}
}

func TestDiscoverQoderModelsServesCacheWithoutCLI(t *testing.T) {
	ResetQoderModelCache()
	t.Cleanup(ResetQoderModelCache)

	// Point the lookup at a directory with no CLI so any attempt to shell out
	// is guaranteed to fail; a cache hit must not need it.
	t.Setenv(qoderPathEnv, filepath.Join(t.TempDir(), "absent-cli"))
	seedQoderModelCache(registry.QoderBackendGlobal, qoderModelInfos([]string{"Qwen3.8-Max"}))

	got := DiscoverQoderModels(context.Background(), registry.QoderBackendGlobal)
	if len(got) != 1 || got[0].ID != "Qwen3.8-Max" {
		t.Fatalf("cached discovery returned %v, want the seeded model", got)
	}

	// The cache must be copied on read so a caller cannot corrupt it.
	got[0].ID = "mutated"
	if again := DiscoverQoderModels(context.Background(), registry.QoderBackendGlobal); again[0].ID != "Qwen3.8-Max" {
		t.Errorf("cache was corrupted by a caller: %q", again[0].ID)
	}
}

func TestQoderModelSupportsBackendScopesChinaOnlyModels(t *testing.T) {
	// A global key must never advertise a China-only model.
	if registry.QoderModelSupportsBackend("qoder-cn", registry.QoderBackendGlobal) {
		t.Error("qoder-cn must not be offered on the global backend")
	}
	if !registry.QoderModelSupportsBackend("qoder-cn", registry.QoderBackendCN) {
		t.Error("qoder-cn must be offered on the CN backend")
	}
	if !registry.QoderModelSupportsBackend("Qwen3.8-Max", registry.QoderBackendGlobal) {
		t.Error("unrestricted models must be offered everywhere")
	}
}

func TestDiscoverQoderModelsReturnsNilWhenCLIUnavailable(t *testing.T) {
	ResetQoderModelCache()
	t.Cleanup(ResetQoderModelCache)

	t.Setenv(qoderPathEnv, filepath.Join(t.TempDir(), "absent-cli"))
	if got := DiscoverQoderModels(context.Background(), registry.QoderBackendGlobal); len(got) != 0 {
		t.Errorf("got %v, want no models when the CLI is missing", got)
	}
}

func TestCloneQoderModelsIsDeep(t *testing.T) {
	if cloneQoderModels(nil) != nil {
		t.Error("cloning nil should return nil")
	}
	src := qoderModelInfos([]string{"A", "B"})
	out := cloneQoderModels(src)
	out[0].ID = "changed"
	if src[0].ID == "changed" {
		t.Error("clone shares struct pointers with the source")
	}
	if len(out) != 2 {
		t.Errorf("got %d clones, want 2", len(out))
	}
}

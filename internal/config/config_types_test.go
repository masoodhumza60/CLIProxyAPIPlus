package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestQoderKeyParsing(t *testing.T) {
	yamlStr := `
qoder:
  enabled: true
qoder-api-key:
  - name: "test"
    token: "pat-xxx"
    backend: "cn"
    models: [qoder-cn, auto]
`
	var cfg Config
	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.Qoder.Enabled {
		t.Fatal("expected qoder enabled")
	}
	if len(cfg.QoderKey) != 1 {
		t.Fatalf("expected 1 key, got %d", len(cfg.QoderKey))
	}
	key := cfg.QoderKey[0]
	if key.Name != "test" || key.Token != "pat-xxx" || key.Backend != "cn" {
		t.Errorf("unexpected key: %+v", key)
	}
}

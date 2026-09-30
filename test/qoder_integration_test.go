package test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
)

func TestQoderEndToEnd(t *testing.T) {
	cfg := &config.Config{
		Qoder: config.QoderConfig{
			Enabled: true,
			Keys: []config.QoderKey{
				{
					Name:    "test",
					Token:   "pat-test",
					Backend: "cn",
					Models:  []string{"qoder-cn", "auto"},
				},
			},
		},
	}

	exec := executor.NewQoderExecutor(cfg)
	if exec.Identifier() != "qoder" {
		t.Fatal("wrong identifier")
	}

	// Test that executor is properly initialized
	if exec == nil {
		t.Fatal("executor is nil")
	}
}

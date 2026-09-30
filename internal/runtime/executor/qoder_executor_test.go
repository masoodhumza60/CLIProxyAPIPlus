package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQoderExecutorIdentifier(t *testing.T) {
	cfg := &config.Config{}
	exec := NewQoderExecutor(cfg)
	if exec.Identifier() != "qoder" {
		t.Fatalf("expected identifier 'qoder', got %q", exec.Identifier())
	}
}

func TestQoderExecutorSpawnCLI(t *testing.T) {
	cfg := &config.Config{}
	exec := NewQoderExecutor(cfg)

	key := config.QoderKey{
		Name:    "test",
		Token:   "pat-test",
		Backend: "cn",
	}

	// This will fail because qoderclicn is not installed,
	// but we can test the error handling
	_, err := exec.spawnCLI("test-session", key)
	if err == nil {
		t.Fatal("expected error when CLI not found")
	}
}

func TestQoderExecutorExecute(t *testing.T) {
	cfg := &config.Config{}
	exec := NewQoderExecutor(cfg)

	req := pluginapi.ExecutorRequest{
		Payload: []byte(`{"model":"qoder-cn","messages":[{"role":"user","content":"hello"}]}`),
	}

	// This will fail because no CLI is installed,
	// but we can test the error path
	_, err := exec.ExecutePlugin(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when CLI not available")
	}
}

func TestQoderExecutorExecuteStream(t *testing.T) {
	cfg := &config.Config{}
	exec := NewQoderExecutor(cfg)

	req := pluginapi.ExecutorRequest{
		Payload: []byte(`{"model":"qoder-cn","messages":[{"role":"user","content":"hello"}],"stream":true}`),
	}

	_, err := exec.ExecuteStreamPlugin(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when CLI not available")
	}
}

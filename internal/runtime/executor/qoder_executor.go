package executor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// QoderExecutor spawns and manages Qoder CLI subprocesses.
type QoderExecutor struct {
	cfg   *config.Config
	store *QoderSessionStore
}

// NewQoderExecutor creates a new Qoder executor.
func NewQoderExecutor(cfg *config.Config) *QoderExecutor {
	return &QoderExecutor{
		cfg:   cfg,
		store: NewQoderSessionStore(),
	}
}

// Identifier returns the provider identifier.
func (e *QoderExecutor) Identifier() string {
	return "qoder"
}

// spawnCLI starts a Qoder CLI subprocess for the given session.
func (e *QoderExecutor) spawnCLI(sessionID string, key config.QoderKey) (*QoderSession, error) {
	binary := "qoderclicn"
	if key.Backend == "global" {
		binary = "qodercli"
	}

	cmd := exec.Command(binary, "--output-format", "stream-json")

	env := os.Environ()
	if key.Token != "" {
		env = append(env, "QODERCN_PERSONAL_ACCESS_TOKEN="+key.Token)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start CLI: %w", err)
	}

	session := &QoderSession{
		SessionID:  sessionID,
		Cmd:        cmd,
		Stdin:      stdin,
		Stdout:     stdout,
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}

	return session, nil
}

// killCLI terminates the CLI subprocess for a session.
func (e *QoderExecutor) killCLI(session *QoderSession) error {
	if session.Cmd == nil {
		return nil
	}
	cmd := session.Cmd.(*exec.Cmd)
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// ExecutePlugin runs a non-streaming request through the Qoder CLI.
// This implements the pluginapi.ProviderExecutor interface.
func (e *QoderExecutor) ExecutePlugin(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	sessionID := ""
	if sid, ok := req.Metadata["session_id"].(string); ok {
		sessionID = sid
	}
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	session, ok := e.store.Get(sessionID)
	if !ok {
		if len(e.cfg.Qoder.Keys) == 0 {
			return pluginapi.ExecutorResponse{}, fmt.Errorf("no Qoder credentials configured")
		}
		key := e.cfg.Qoder.Keys[0]
		var err error
		session, err = e.spawnCLI(sessionID, key)
		if err != nil {
			return pluginapi.ExecutorResponse{}, fmt.Errorf("failed to spawn CLI: %w", err)
		}
		e.store.Put(sessionID, session)
	}

	session.LastUsedAt = time.Now()

	stdin := session.Stdin.(io.WriteCloser)
	if _, err := stdin.Write(req.Payload); err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("failed to write to CLI: %w", err)
	}
	if _, err := stdin.Write([]byte("\n")); err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("failed to write newline: %w", err)
	}

	stdout := session.Stdout.(io.ReadCloser)
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("failed to read from CLI: %w", err)
	}

	return pluginapi.ExecutorResponse{
		Payload: line,
	}, nil
}

// ExecuteStreamPlugin runs a streaming request through the Qoder CLI.
// This implements the pluginapi.ProviderExecutor interface.
// For now, delegates to ExecutePlugin and wraps in stream response.
func (e *QoderExecutor) ExecuteStreamPlugin(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	resp, err := e.ExecutePlugin(ctx, req)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	return pluginapi.ExecutorStreamResponse{
		Headers: resp.Headers,
		Chunks:  nil,
	}, nil
}

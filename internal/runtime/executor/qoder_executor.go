package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// qoderCLIResponseTimeout is the max time to wait for a CLI response.
	qoderCLIResponseTimeout = 30 * time.Second
	// qoderCLISpawnTimeout is the max time to wait for CLI to start.
	qoderCLISpawnTimeout = 10 * time.Second
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

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start CLI: %w", err)
	}

	// Wait briefly to ensure the process doesn't exit immediately.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return nil, fmt.Errorf("CLI exited immediately: %w (stderr: %s)", err, stderrBuf.String())
	case <-time.After(100 * time.Millisecond):
		// CLI is still running, good.
	case <-time.After(qoderCLISpawnTimeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("CLI spawn timeout")
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
	if session.Cmd.Process != nil {
		return session.Cmd.Process.Kill()
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

	e.store.UpdateLastUsed(sessionID)

	// Check if the CLI process is still alive.
	if session.Cmd.ProcessState != nil {
		// Process has exited, remove the stale session.
		e.store.Delete(sessionID)
		return pluginapi.ExecutorResponse{}, fmt.Errorf("CLI process has exited, session removed")
	}

	stdin := session.Stdin
	if _, err := stdin.Write(req.Payload); err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("failed to write to CLI: %w", err)
	}
	if _, err := stdin.Write([]byte("\n")); err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("failed to write newline: %w", err)
	}

	// Read response with context cancellation and timeout support.
	stdout := session.Stdout
	reader := bufio.NewReader(stdout)

	type readResult struct {
		line []byte
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		line, err := reader.ReadBytes('\n')
		resultCh <- readResult{line: line, err: err}
	}()

	var line []byte
	select {
	case <-ctx.Done():
		return pluginapi.ExecutorResponse{}, fmt.Errorf("context cancelled: %w", ctx.Err())
	case <-time.After(qoderCLIResponseTimeout):
		return pluginapi.ExecutorResponse{}, fmt.Errorf("CLI response timeout after %s", qoderCLIResponseTimeout)
	case res := <-resultCh:
		if res.err != nil {
			return pluginapi.ExecutorResponse{}, fmt.Errorf("failed to read from CLI: %w", res.err)
		}
		line = res.line
	}

	// Validate JSON output.
	var raw map[string]interface{}
	if err := json.Unmarshal(line, &raw); err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("invalid JSON from CLI: %w", err)
	}

	// Check for error field in response.
	if errMsg, ok := raw["error"].(string); ok && errMsg != "" {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("CLI error: %s", errMsg)
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

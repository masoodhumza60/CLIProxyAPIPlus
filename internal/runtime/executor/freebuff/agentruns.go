package freebuff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// This file speaks the agent-run lifecycle the chat route is gated on: a chat
// completion is only accepted inside a run the backend knows about, so every
// chat request has to acquire a run first and report how it ended afterwards.
// Both halves are plain REST on the same origin that serves the catalogue,
// authenticated with the same bearer token — there is no socket, no signature,
// and nothing here is forged: the run is registered with the caller's own
// credential exactly as the CLI does it.

// AgentRunsPath is the route that starts and finishes runs.
const AgentRunsPath = "/api/v1/agent-runs"

// RegisterAgentID is the base3 catalog root the CLI runs for every model in
// the catalogue: its definition resolves the request's catalog handle, and it
// admits no plain model id at all.
const RegisterAgentID = "base3-free-catalog"

// StartAgentRun registers a new run for the caller's account and returns the
// server-minted run id the chat route requires in codebuff_metadata.
//
// base is the origin that hosts the agent-runs route. agentID is the agent the
// run executes; register with RegisterAgentID for a catalogue-driven chat run.
//
// A run that is started but never finished is the leak this API invites: the
// caller owns finishing it, and FinishAgentRun stays best-effort for that
// reason.
func (c *Client) StartAgentRun(ctx context.Context, base, agentID string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "", errors.New("freebuff: no agent-run base URL is configured")
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return "", errors.New("freebuff: starting a run requires a token; run `freebuff login` first")
	}
	if strings.TrimSpace(agentID) == "" {
		return "", errors.New("freebuff: starting a run requires an agent id")
	}

	payload, err := json.Marshal(map[string]any{
		"action":         "START",
		"agentId":        agentID,
		"ancestorRunIds": []any{},
	})
	if err != nil {
		return "", fmt.Errorf("freebuff: building the run-start body: %w", err)
	}

	var started struct {
		RunID string `json:"runId"`
	}
	if err := c.postAgentRun(ctx, base, payload, &started); err != nil {
		return "", err
	}
	if started.RunID == "" {
		return "", fmt.Errorf("freebuff: the run-start response carried no runId: %s", truncate(string(payload), 200))
	}
	return started.RunID, nil
}

// FinishAgentRun reports how a run ended. status is one of the service's own
// outcomes — "completed", "failed" or "cancelled" (anything else is refused
// by the route's own validation). Failures here never fail the chat they
// close: the answer has already been delivered, and an unfinished run is a
// bookkeeping gap rather than a caller's problem.
func (c *Client) FinishAgentRun(ctx context.Context, base, runID, status string) error {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return errors.New("freebuff: no agent-run base URL is configured")
	}
	if strings.TrimSpace(runID) == "" {
		return errors.New("freebuff: finishing a run requires the run id")
	}

	payload, err := json.Marshal(map[string]any{
		"action":        "FINISH",
		"runId":         runID,
		"status":        status,
		"totalSteps":    1,
		"steps":         []any{},
		"directCredits": 0,
		"totalCredits":  0,
	})
	if err != nil {
		return fmt.Errorf("freebuff: building the run-finish body: %w", err)
	}
	return c.postAgentRun(ctx, base, payload, nil)
}

// postAgentRun sends one agent-runs request and decodes the response when a
// destination is given. The error carries whatever the service said, since a
// refusal here is a gate an operator may need to see verbatim.
func (c *Client) postAgentRun(ctx context.Context, base string, payload []byte, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+AgentRunsPath, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("freebuff: building %s request: %w", AgentRunsPath, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set(APIKeyHeader, c.apiKey)
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("freebuff: calling %s: %w", AgentRunsPath, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &SessionError{
			Status:    resp.StatusCode,
			Method:    http.MethodPost,
			Path:      AgentRunsPath,
			Body:      string(body),
			Message:   truncate(string(body), 200),
			ErrorCode: chatErrorCode(body),
		}
	}
	if dest != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, dest); err != nil {
			return fmt.Errorf("freebuff: reading the %s response: %w", AgentRunsPath, err)
		}
	}
	return nil
}

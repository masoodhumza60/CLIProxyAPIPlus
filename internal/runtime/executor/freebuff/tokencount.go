package freebuff

import (
	"context"
	"encoding/json"
	"fmt"
)

// pathTokenCount is the documented upstream token-count endpoint.
//
// It is declared here rather than in the test fixture because it is a constant
// of the protocol, and production code has to be able to name it. The shape of
// the protocol is not an operator's choice.
const pathTokenCount = "/api/v1/token-count"

// TokenCountMessage is one conversation turn in a token-count request.
//
// Content is left as any rather than a string because the OpenAI-shaped message
// content is either a bare string or an array of content blocks, and this
// client does not need to understand which: it is forwarding, not re-encoding.
type TokenCountMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// TokenCountRequest is the documented body of POST /api/v1/token-count.
//
// The optional fields are omitted rather than sent empty, because the upstream
// distinguishes "no system prompt" from "an empty one".
type TokenCountRequest struct {
	Messages []TokenCountMessage `json:"messages"`
	System   json.RawMessage     `json:"system,omitempty"`
	Model    string              `json:"model,omitempty"`
	Tools    []any               `json:"tools,omitempty"`
}

// tokenCountResponse is the documented reply body.
type tokenCountResponse struct {
	InputTokens *int64 `json:"inputTokens"`
}

// TokenCount asks the upstream to count the tokens a request would consume.
//
// This is the one genuinely useful upstream call in the documented surface, and
// it is why a Freebuff-backed credential can answer a real CountTokens instead
// of the 501 that a subprocess-backed provider such as Qoder has to return.
//
// A reply without a usable positive count is an error rather than zero: a zero
// would be indistinguishable from a genuinely empty request, and callers use
// this number for quota decisions.
func (c *Client) TokenCount(ctx context.Context, req TokenCountRequest) (int64, error) {
	if len(req.Messages) == 0 {
		return 0, fmt.Errorf("freebuff: token count requires at least one message")
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("freebuff: encoding token count request: %w", err)
	}

	body, err := c.doJSON(ctx, "POST", pathTokenCount, payload)
	if err != nil {
		return 0, err
	}

	var parsed tokenCountResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("freebuff: decoding token count response: %w", err)
	}
	if parsed.InputTokens == nil {
		return 0, fmt.Errorf("freebuff: token count response has no inputTokens field")
	}
	if *parsed.InputTokens <= 0 {
		return 0, fmt.Errorf("freebuff: token count response reported %d input tokens", *parsed.InputTokens)
	}
	return *parsed.InputTokens, nil
}

package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// qoderClaudeRequest is the subset of a Claude Messages request that the Qoder
// CLI needs once the request has been rendered into a prompt.
type qoderClaudeRequest struct {
	System   any               `json:"system"`
	Messages []qoderClaudeTurn `json:"messages"`
	Tools    []qoderClaudeTool `json:"tools"`
}

type qoderClaudeTurn struct {
	Role    string         `json:"role"`
	Content []qoderContent `json:"content"`
}

// UnmarshalJSON accepts both shapes the Claude Messages API allows for a turn's
// content: a bare string shorthand and the full block array.
func (t *qoderClaudeTurn) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Role = raw.Role
	t.Content = nil

	trimmed := bytes.TrimSpace(raw.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		t.Content = []qoderContent{{Type: "text", Text: text}}
		return nil
	}
	return json.Unmarshal(trimmed, &t.Content)
}

type qoderContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type qoderClaudeTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// qoderPromptFromClaudeRequest renders a translated Claude Messages request into
// the natural-language prompt the Qoder CLI consumes on stdin.
func qoderPromptFromClaudeRequest(payload []byte) (string, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return "", errors.New("qoder: empty request payload")
	}
	var req qoderClaudeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return "", fmt.Errorf("qoder: cannot render prompt from %s request: %w", "claude", err)
	}
	return qoderRenderPrompt(&req), nil
}

// qoderRenderPrompt builds the prompt text for a Claude request.
func qoderRenderPrompt(req *qoderClaudeRequest) string {
	var b strings.Builder

	if system := qoderRenderSystem(req.System); system != "" {
		b.WriteString("<system>\n")
		b.WriteString(system)
		b.WriteString("\n</system>\n\n")
	}

	if tools := qoderRenderTools(req.Tools); tools != "" {
		b.WriteString("<available_tools>\n")
		b.WriteString(tools)
		b.WriteString("\n</available_tools>\n\n")
	}

	b.WriteString("<conversation>\n")
	for _, turn := range req.Messages {
		b.WriteString(qoderRenderTurn(turn))
	}
	b.WriteString("</conversation>\n")
	return b.String()
}

// qoderRenderSystem flattens the system field, which may be a string or blocks.
func qoderRenderSystem(system any) string {
	switch v := system.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n\n"))
	default:
		return ""
	}
}

// qoderRenderTools describes the client tools as instructions. The Qoder CLI has
// no native tool-call channel, so tools are advertised in the prompt.
func qoderRenderTools(tools []qoderClaudeTool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	for _, tool := range tools {
		if tool.Name == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s", tool.Name)
		if tool.Description != "" {
			fmt.Fprintf(&b, ": %s", strings.Join(strings.Fields(tool.Description), " "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// qoderRenderTurn renders one conversation turn.
func qoderRenderTurn(turn qoderClaudeTurn) string {
	var b strings.Builder
	// System turns belong in the system block, not the transcript.
	if turn.Role == "system" || turn.Role == "" {
		return ""
	}
	fmt.Fprintf(&b, "[%s]\n", turn.Role)
	for _, block := range turn.Content {
		switch block.Type {
		case "text", "":
			if block.Text != "" {
				b.WriteString(block.Text)
				b.WriteByte('\n')
			}
		case "tool_use":
			fmt.Fprintf(&b, "[tool_use %s]\n", block.Name)
			if len(block.Content) > 0 {
				b.Write(block.Content)
				b.WriteByte('\n')
			}
		case "tool_result":
			fmt.Fprintf(&b, "[tool_result %s]\n", block.ToolUseID)
			if len(block.Content) > 0 {
				b.WriteString(qoderRenderToolResult(block.Content))
				b.WriteByte('\n')
			}
		case "thinking":
			// Extended thinking blocks are not part of the prompt.
		default:
			if block.Text != "" {
				b.WriteString(block.Text)
				b.WriteByte('\n')
			}
		}
	}
	b.WriteByte('\n')
	return b.String()
}

// qoderRenderToolResult flattens a tool_result payload, which may be a string or
// a list of content blocks.
func qoderRenderToolResult(raw json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, block := range blocks {
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

package freebuff

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// This file implements the Freebuff chat transport.
//
// The official CLI chats through the OpenAI-compatible route on the Codebuff
// API host: a POST carrying the chat request in OpenAI shape, authenticated
// with the two headers the catalogue route also requires — the bearer token
// and the x-codebuff-api-key header — both carrying the token the login flow
// obtained. The web app's own route on freebuff.com now answers 409
// "chat_moved" to every client and is not spoken here.
//
// Nothing here is forged: the credential is the one `freebuff login` obtained,
// and the client identifies itself honestly.

// ChatPath is the official CLI's OpenAI-compatible chat route.
const ChatPath = "/api/v1/chat/completions"

// maxChatLine bounds one line of a chat stream so a misbehaving upstream
// cannot balloon memory.
const maxChatLine = 1 << 20

// ChatCompletionDelta carries the useful part of one chat-completions chunk.
//
// Model is the model name the upstream echoes in each chunk and Usage is the
// token accounting the final chunk carries when the caller asked for it; both
// are optional and may be empty.
type ChatCompletionDelta struct {
	Content      string
	Reasoning    string
	FinishReason string
	Model        string
	Usage        *ChatCompletionUsage
}

// ChatCompletionUsage is the token accounting one chunk may carry.
type ChatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatStream posts an OpenAI-format chat completion request to the chat host
// and returns the raw response body.
//
// The body is the already-translated OpenAI request, sent as it arrived; this
// transport does not reshape it. A request with stream:true is answered with
// Server-Sent Events, anything else with one JSON object.
//
// chatBaseURL is the origin that serves the chat route. It is a parameter
// rather than the client's own base URL because the web host the client points
// at for login is a different origin from the one that serves chat, and a test
// must be able to point at an in-process fixture.
//
// The returned body must be closed by the caller. On a non-2xx response the
// body is drained and closed here and the error carries the service's own
// message, because that message is what distinguishes an expired credential
// from a real failure and an operator needs it to know whether to re-run
// `freebuff login`.
func (c *Client) ChatStream(ctx context.Context, chatBaseURL string, payload []byte) (io.ReadCloser, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, errors.New("freebuff: chat requires a token; run `freebuff login` first")
	}
	base := strings.TrimRight(strings.TrimSpace(chatBaseURL), "/")
	if base == "" {
		return nil, errors.New("freebuff: no chat base URL is configured")
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, errors.New("freebuff: chat requires a request body")
	}

	// Both auth headers are required, exactly as for the catalogue route: the
	// live service answers a bearer token alone with 401.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+ChatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("freebuff: building %s request: %w", ChatPath, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set(APIKeyHeader, c.apiKey)
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("freebuff: opening chat stream: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, c.chatStatusError(resp)
	}
	return resp.Body, nil
}

// chatStatusError turns a rejected chat request into an error that says what
// to do about it. The expired-credential case is the common one and is worth
// naming, because the fix is to re-authenticate rather than to retry.
func (c *Client) chatStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
	se := &SessionError{
		Status:    resp.StatusCode,
		Method:    http.MethodPost,
		Path:      ChatPath,
		Body:      string(body),
		Message:   summarizeChatError(resp.StatusCode, string(body)),
		ErrorCode: chatErrorCode(body),
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		se.Message = "freebuff session is no longer valid; run `freebuff login` again"
	}
	return se
}

// chatErrorCode pulls the service's own error slug out of an error body. The
// route answers JSON on failure, but a proxy or gateway in front of it may not,
// so an unparseable body is not itself an error.
func chatErrorCode(body []byte) string {
	var envelope struct {
		Error   string `json:"error"`
		Status  string `json:"status"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	for _, candidate := range []string{envelope.Error, envelope.Code, envelope.Status} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return ""
}

// summarizeChatError prefers the service's message and falls back to the raw
// body so an unrecognised failure still carries what the server said.
func summarizeChatError(status int, body string) string {
	var envelope struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil {
		if msg := strings.TrimSpace(envelope.Message); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(envelope.Error); msg != "" {
			return msg
		}
	}
	return truncate(body, 200)
}

// ScanChatCompletionEvents reads an OpenAI-format chat-completions SSE stream
// and invokes fn for every chunk that carries something to deliver.
//
// The stream is the only place the answer arrives, so a read failure part-way
// through is reported rather than swallowed: returning a partial answer as if
// it were complete would be worse than failing the request.
//
// Lines that are blank, are SSE comments, or fail to decode are skipped,
// because an upstream that grows fields or shapes this client does not model
// must not cost the caller an otherwise healthy answer. "[DONE]" ends the scan
// cleanly, and usage in a chunk is delivered without content so a caller can
// record the real token counts where the service reports them.
func ScanChatCompletionEvents(body io.Reader, fn func(ChatCompletionDelta) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxChatLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return nil
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *ChatCompletionUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A payload this client cannot decode is not fatal; the upstream
			// may use shapes this client does not model yet.
			continue
		}
		if chunk.Error != nil {
			message := strings.TrimSpace(chunk.Error.Message)
			if message == "" {
				message = strings.TrimSpace(chunk.Error.Code)
			}
			if message == "" {
				message = "the stream reported a failure"
			}
			return &SessionError{
				Status:  http.StatusBadGateway,
				Method:  http.MethodPost,
				Path:    ChatPath,
				Message: message,
				Body:    truncate(payload, 200),
			}
		}
		if chunk.Usage != nil {
			if err := fn(ChatCompletionDelta{Model: chunk.Model, Usage: chunk.Usage}); err != nil {
				return err
			}
		}
		for _, choice := range chunk.Choices {
			delta := ChatCompletionDelta{
				Content:      choice.Delta.Content,
				Reasoning:    choice.Delta.ReasoningContent,
				FinishReason: choice.FinishReason,
				Model:        chunk.Model,
			}
			if delta.Content == "" && delta.Reasoning == "" && delta.FinishReason == "" {
				continue
			}
			if err := fn(delta); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("freebuff: reading chat stream: %w", err)
	}
	return nil
}

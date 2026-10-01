package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/freebuff"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// This file drives the Freebuff web chat route.
//
// The public Codebuff chat API refuses every third-party client, so the executor
// speaks the web app's own streaming route instead. Two consequences shape the
// code: the answer arrives as a sequence of deltas rather than one JSON body, so
// a non-streaming request accumulates them; and the route authenticates with a
// session cookie rather than a bearer token.

// freebuffCatalogHost is the Codebuff API origin that serves the model
// catalogue. It is deliberately a named constant rather than inferred from the
// web host: the two are different origins, and deriving one from the other
// would silently point catalogue traffic at the wrong server.
const freebuffCatalogHost = "https://www.codebuff.com"

// freebuffDefaultWebHost is where login and chat live.
const freebuffDefaultWebHost = "https://freebuff.com"

// FreebuffDefaultWebHost returns the web origin that serves login and chat.
//
// It is exported so callers that hold only a credential can name the origin that
// belongs with it, rather than hard-coding a second copy of the address that
// could drift from this one.
func FreebuffDefaultWebHost() string { return freebuffDefaultWebHost }

// freebuffSessionExpiredMessage is what an operator sees when the credential
// has aged out. The token is a session token with a finite life, so the useful
// instruction is to re-authenticate rather than to retry.
const freebuffSessionExpiredMessage = "freebuff session expired; run `freebuff login` again"

// chatCompletion accumulates the deltas of one streamed answer.
//
// It is mutex-guarded because the scan runs on the caller's goroutine today but
// nothing in the type requires that, and a shared accumulator is the easiest
// place for a future concurrent reader to race.
type chatCompletion struct {
	content   strings.Builder
	reasoning strings.Builder
	threadID  string
	model     string
	title     string
}

func (c *chatCompletion) consume(event freebuff.ChatEvent) {
	switch {
	case strings.Contains(event.Type, "error") || strings.Contains(event.Type, "fail"):
		// Reported through the caller's error path rather than accumulated as
		// content: a partial answer that silently omitted the failure would be
		// worse than failing the request.
	case event.Type == "meta":
		c.threadID = event.ThreadID
		if event.Model != "" {
			c.model = event.Model
		}
	case event.Type == "title":
		if event.Title != "" {
			c.title = event.Title
		}
	case event.Type == "reasoning_delta":
		c.reasoning.WriteString(event.Text)
	case event.Type == "delta":
		c.content.WriteString(event.Text)
	}
}

func (c *chatCompletion) text() string { return c.content.String() }

func (c *chatCompletion) reasoningText() string { return c.reasoning.String() }

// nonStreamResponse renders the accumulated answer as an OpenAI-shaped chat
// completion.
//
// Usage is estimated rather than reported: the web route streams no token
// counts, and inventing zeroes would make a caller believe a request was free.
func (c *chatCompletion) nonStreamResponse(id, requestedModel string) []byte {
	model := c.model
	if model == "" {
		model = requestedModel
	}
	message := map[string]any{"role": "assistant", "content": c.text()}
	if reasoning := c.reasoningText(); reasoning != "" {
		// reasoning_content is the conventional OpenAI-shaped field for a
		// reasoning trace; it is additive, so a client that ignores it is
		// unaffected.
		message["reasoning_content"] = reasoning
	}
	promptTokens := freebuffEstimateTokens(c.title) + freebuffEstimateTokens(c.reasoningText())
	completionTokens := freebuffEstimateTokens(c.text())
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return encoded
}

// freebuffEstimateTokens is a deliberately rough character-based estimate.
//
// The web route reports no usage, so a value is needed for callers that expect
// a usage object. Over-estimating slightly is the safer error: a caller that
// budgets from this should not be surprised by a lower bill.
func freebuffEstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Four characters per token is the rule of thumb for English prose; the
	// rounding up keeps any non-empty text non-zero.
	return (len([]rune(text)) + 3) / 4
}

// freebuffChatRequestFor renders a translated OpenAI request into the body the
// web route accepts.
//
// The route takes a single prompt string rather than a message array, so the
// conversation is flattened with role prefixes. Tool definitions are not
// forwarded: the route has no tool channel, and passing OpenAI tool schemas into
// a field that expects a prompt would corrupt the prompt itself.
func freebuffChatRequestFor(translated []byte, threadID, model string, reasoningEffort *string) (freebuff.ChatRequest, error) {
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(translated, &payload); err != nil {
		return freebuff.ChatRequest{}, fmt.Errorf("freebuff: reading the translated request: %w", err)
	}

	var prompt strings.Builder
	for _, message := range payload.Messages {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		if prompt.Len() > 0 {
			prompt.WriteString("\n\n")
		}
		prompt.WriteString(role)
		prompt.WriteString(": ")
		prompt.WriteString(content)
	}
	if prompt.Len() == 0 {
		return freebuff.ChatRequest{}, fmt.Errorf("freebuff: the request carried no message content")
	}

	req := freebuff.ChatRequest{
		Content:         prompt.String(),
		Model:           model,
		ReasoningEffort: reasoningEffort,
	}
	if strings.TrimSpace(threadID) != "" {
		id := threadID
		req.ThreadID = &id
	}
	return req, nil
}

// executeChat runs one chat turn and returns the accumulated answer.
//
// Shared by the streaming and non-streaming paths so both agree on how a prompt
// is rendered, which model is resolved, and how a mid-stream failure is
// classified. Only the delivery differs.
func (e *FreebuffExecutor) executeChat(
	ctx context.Context,
	client *freebuff.Client,
	translated []byte,
	req cliproxyexecutor.Request,
	threadID string,
	onDelta func(content, reasoning string),
) (*chatCompletion, error) {
	catalog, err := client.FetchCatalog(ctx, e.catalogBaseURL)
	if err != nil {
		return nil, err
	}
	row, ok := catalog.Resolve(req.Model)
	if !ok {
		return nil, freebuffStatusError{
			code:        400,
			msg:         fmt.Sprintf("freebuff: no model is available for %q on this account", req.Model),
			credentials: false,
		}
	}

	chatReq, err := freebuffChatRequestFor(translated, threadID, row.Handle, reasoningEffortFor(row))
	if err != nil {
		return nil, err
	}

	body, err := client.ChatStream(ctx, chatReq)
	if err != nil {
		return nil, e.classifyChatError(err)
	}
	defer func() {
		if closeErr := body.Close(); closeErr != nil {
			log.WithError(closeErr).Debug("freebuff: closing the chat stream")
		}
	}()

	accumulated := &chatCompletion{}
	scanErr := freebuff.ScanChatEvents(body, func(event freebuff.ChatEvent) error {
		if event.IsStreamFailure() {
			return freebuffStatusError{
				code: 502,
				msg:  "freebuff: the stream reported a failure: " + strings.TrimSpace(event.Text),
			}
		}
		if event.Type == "delta" && onDelta != nil && event.Text != "" {
			onDelta(event.Text, "")
		}
		if event.Type == "reasoning_delta" && onDelta != nil && event.Text != "" {
			onDelta("", event.Text)
		}
		accumulated.consume(event)
		return nil
	})
	if scanErr != nil {
		if classified := e.classifyChatError(scanErr); !isCredentialLoss(classified) {
			return nil, classified
		}
	}
	if accumulated.text() == "" && accumulated.reasoningText() == "" {
		return nil, freebuffStatusError{code: 502, msg: "freebuff: the stream produced no content"}
	}
	return accumulated, nil
}

// reasoningEffortFor picks the effort to request for a catalogue row.
//
// The route accepts null, and the catalogue names a default per model. Sending
// a default the account did not ask for would silently spend more than intended,
// so the row's own default is only used when the model actually declares one.
func reasoningEffortFor(row freebuff.CatalogRow) *string {
	if len(row.Efforts) == 0 {
		return nil
	}
	effort := strings.TrimSpace(row.DefaultEffort)
	if effort == "" {
		// Fall back to the lowest offered effort rather than the highest: an
		// unset default should not cost the account more than it has to.
		effort = strings.TrimSpace(row.Efforts[0])
	}
	if effort == "" {
		return nil
	}
	return &effort
}

// classifyChatError turns a transport or stream failure into something the
// conductor can act on.
//
// A rejected session is credential-scoped so the conductor stops routing to a
// credential that cannot work, and the message names re-authentication because
// retrying an expired session cannot succeed.
func (e *FreebuffExecutor) classifyChatError(err error) error {
	if err == nil {
		return nil
	}
	var sessionErr *freebuff.SessionError
	if asFreebuffSessionError(err, &sessionErr) {
		switch sessionErr.StatusCode() {
		case 401, 403:
			return freebuffStatusError{
				code:        401,
				msg:         freebuffSessionExpiredMessage,
				credentials: true,
			}
		case 429:
			return freebuffStatusError{code: 429, msg: sessionErr.Error()}
		}
		return freebuffStatusError{code: sessionErr.StatusCode(), msg: sessionErr.Error()}
	}
	if strings.Contains(err.Error(), "session") && strings.Contains(err.Error(), "login") {
		return freebuffStatusError{code: 401, msg: freebuffSessionExpiredMessage, credentials: true}
	}
	return freebuffStatusError{code: 502, msg: err.Error()}
}

// isCredentialLoss reports whether an error means the session can no longer be
// used. It exists so a mid-stream credential loss is not reported as a generic
// upstream failure, which would leave the conductor routing to a dead token.
func isCredentialLoss(err error) bool {
	var status freebuffStatusError
	if !asFreebuffStatusError(err, &status) {
		return false
	}
	return status.code == 401 && status.credentials
}

// asFreebuffStatusError is errors.As specialised for the local status error,
// kept as a helper so the executor does not import errors just for one call.
func asFreebuffStatusError(err error, target *freebuffStatusError) bool {
	for err != nil {
		if candidate, ok := err.(freebuffStatusError); ok {
			*target = candidate
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// asFreebuffSessionError is errors.As specialised for the transport's session
// error, which carries the upstream status code.
func asFreebuffSessionError(err error, target **freebuff.SessionError) bool {
	for err != nil {
		if candidate, ok := err.(*freebuff.SessionError); ok {
			*target = candidate
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// streamChunk builds one streaming response in the shape the executor's
// translation layer expects.
//
// The chunk is an OpenAI chat-completion delta because the executor translates
// responses itself; the conductor does not translate stream chunks.
func (e *FreebuffExecutor) streamChunk(id, model, content, reasoning string, finish bool) cliproxyexecutor.StreamChunk {
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finish {
		choice["finish_reason"] = "stop"
	}
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return cliproxyexecutor.StreamChunk{Err: fmt.Errorf("freebuff: encoding a stream chunk: %w", err)}
	}
	return cliproxyexecutor.StreamChunk{Payload: encoded}
}

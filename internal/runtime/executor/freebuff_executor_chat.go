package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/freebuff"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// This file drives the Freebuff chat route on the Codebuff API host.
//
// The route is OpenAI-compatible, so the translated OpenAI body is sent almost
// verbatim and the answer arrives as OpenAI-style delta chunks. The request
// always asks to stream, which leaves one parsing path for both delivery
// modes: a non-streaming request accumulates the deltas rather than following
// a second code path, and the upstream-specific adjustments are limited to
// naming the model by its catalogue handle.

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
	content      strings.Builder
	reasoning    strings.Builder
	model        string
	finishReason string
	usage        *freebuff.ChatCompletionUsage
	// promptEstimate is filled in by the caller so a non-streaming response
	// can still carry a prompt token count when the service reports none.
	promptEstimate int
}

func (c *chatCompletion) consume(delta freebuff.ChatCompletionDelta) {
	if delta.Model != "" {
		c.model = delta.Model
	}
	c.content.WriteString(delta.Content)
	c.reasoning.WriteString(delta.Reasoning)
	if delta.FinishReason != "" {
		c.finishReason = delta.FinishReason
	}
	if delta.Usage != nil {
		c.usage = delta.Usage
	}
}

func (c *chatCompletion) text() string { return c.content.String() }

func (c *chatCompletion) reasoningText() string { return c.reasoning.String() }

// nonStreamResponse renders the accumulated answer as an OpenAI-shaped chat
// completion.
//
// Usage is the count the service reported when it reports one, and an estimate
// otherwise; inventing zeroes would make a caller believe a request was free.
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
	finish := c.finishReason
	if finish == "" {
		finish = "stop"
	}
	promptTokens, completionTokens := c.usageTokens()
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
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

// usageTokens returns the token counts to report: the service's own numbers
// when the stream carried them, otherwise character-based estimates.
//
// The prompt estimate comes from the request rather than from nothing, and the
// completion estimate keeps the reasoning trace in the total so a caller that
// budgets from these numbers is not surprised by the reasoning cost.
func (c *chatCompletion) usageTokens() (int, int) {
	if c.usage != nil {
		return c.usage.PromptTokens, c.usage.CompletionTokens
	}
	return c.promptEstimate, freebuffEstimateTokens(c.text()) + freebuffEstimateTokens(c.reasoningText())
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

// freebuffChatCompletionsBody renders the translated OpenAI request into the
// body the chat route accepts.
//
// The route is OpenAI-compatible, so the translated body travels almost
// verbatim. The executor names the model by its catalogue handle — a name the
// caller sent verbatim would otherwise be substituted silently — and always
// asks to stream, which leaves one parsing path for both delivery modes.
//
// The catalogue's declared effort is forwarded as reasoning_effort because the
// upstream prices and floors models by effort; an effort chosen without
// consulting the catalogue would silently spend more than the account has to.
func freebuffChatCompletionsBody(translated []byte, model string, reasoningEffort *string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(translated, &body); err != nil {
		return nil, fmt.Errorf("freebuff: reading the translated request: %w", err)
	}
	if body == nil {
		body = map[string]any{}
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, fmt.Errorf("freebuff: the request carried no message content")
	}
	if strings.TrimSpace(model) != "" {
		body["model"] = model
	}
	body["stream"] = true
	if reasoningEffort != nil && strings.TrimSpace(*reasoningEffort) != "" {
		body["reasoning_effort"] = *reasoningEffort
	}
	return json.Marshal(body)
}

// freebuffPromptEstimate estimates the request's prompt size, which stands in
// for the prompt token count when the service reports none. Message content
// that arrives as an array (image inputs and the like) contributes only its
// string part, and a request that adds up to nothing falls back to estimating
// from the whole body rather than reporting an empty prompt.
func freebuffPromptEstimate(translated []byte) int {
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(translated, &body); err != nil {
		return 0
	}
	var text strings.Builder
	for _, message := range body.Messages {
		text.WriteString(message.Content)
	}
	if text.Len() == 0 {
		return freebuffEstimateTokens(string(translated))
	}
	return freebuffEstimateTokens(text.String())
}

// executeChat runs one chat turn and returns the accumulated answer.
//
// Shared by the streaming and non-streaming paths so both agree on how the
// request is rendered, which model is resolved, and how a mid-stream failure
// is classified. Only the delivery differs.
func (e *FreebuffExecutor) executeChat(
	ctx context.Context,
	client *freebuff.Client,
	translated []byte,
	req cliproxyexecutor.Request,
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

	payload, err := freebuffChatCompletionsBody(translated, row.Handle, reasoningEffortFor(row))
	if err != nil {
		return nil, err
	}

	// The chat route lives on the same Codebuff origin as the catalogue, and
	// a test serves both legs from one fixture, which is why the catalogue
	// base URL carries the chat traffic.
	body, err := client.ChatStream(ctx, e.catalogBaseURL, payload)
	if err != nil {
		return nil, e.classifyChatError(err)
	}
	defer func() {
		if closeErr := body.Close(); closeErr != nil {
			log.WithError(closeErr).Debug("freebuff: closing the chat stream")
		}
	}()

	accumulated := &chatCompletion{promptEstimate: freebuffPromptEstimate(payload)}
	scanErr := freebuff.ScanChatCompletionEvents(body, func(delta freebuff.ChatCompletionDelta) error {
		if onDelta != nil && (delta.Content != "" || delta.Reasoning != "") {
			onDelta(delta.Content, delta.Reasoning)
		}
		accumulated.consume(delta)
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
// The catalogue names a default per model. Sending a default the account did
// not ask for would silently spend more than intended, so the row's own
// default is only used when the model actually declares one.
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

// freebuffAccessDeniedMessage keeps the service's own explanation.
//
// The upstream body names the actual reason - "Your account has been
// suspended." - and that sentence is the only thing that tells an operator what
// to do next. Replacing it with an instruction to sign in again is worse than
// useless here: signing in again is precisely the thing that cannot help.
func freebuffAccessDeniedMessage(sessionErr *freebuff.SessionError) string {
	if message := strings.TrimSpace(sessionErr.Error()); message != "" {
		return "freebuff: access denied by the service: " + message
	}
	return "freebuff: access denied by the service (HTTP 403); it did not say why"
}

// classifyChatError turns a transport or stream failure into something the
// conductor can act on.
//
// A rejected session is credential-scoped so the conductor stops routing to a
// credential that cannot work, and the message names re-authentication because
// retrying an expired session cannot succeed.
//
// A 403 is deliberately treated apart from a 401. The service returns 403 for
// account-level access decisions and per-model or per-slot gates, not only for
// a rejected token, so marking it credential-scoped would take a usable account
// out of rotation for a reason re-authentication cannot fix.
func (e *FreebuffExecutor) classifyChatError(err error) error {
	if err == nil {
		return nil
	}
	var sessionErr *freebuff.SessionError
	if asFreebuffSessionError(err, &sessionErr) {
		switch sessionErr.StatusCode() {
		case 401:
			return freebuffStatusError{
				code:        401,
				msg:         freebuffSessionExpiredMessage,
				credentials: true,
			}
		case 403:
			return freebuffStatusError{
				code: 403,
				msg:  freebuffAccessDeniedMessage(sessionErr),
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

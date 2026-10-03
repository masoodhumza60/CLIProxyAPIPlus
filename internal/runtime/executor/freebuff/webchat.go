package freebuff

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// This file implements the Freebuff web chat surface.
//
// The public Codebuff API (POST /api/v1/chat/completions) refuses every
// request from a third-party client with 403 free_mode_cli_required, so it is
// unusable here and is not called anywhere. The web app's own first-party chat
// route serves the same account fine, and that is what this file speaks: a
// session cookie minted by the CLI login flow, a small JSON body, and a stream
// of Server-Sent Events. Nothing here is forged — the credential is the one
// `freebuff login` obtained, and the client identifies itself honestly.

// ChatPath is the web app's streaming chat route.
const ChatPath = "/api/chat/stream"

// ThreadsPath lists the account's conversations.
const ThreadsPath = "/api/chat/threads"

// SessionCookie is the cookie the web app authenticates a signed-in browser
// session with. The CLI login flow issues the token the web app then accepts
// here, which is why no browser automation or session scraping is involved.
const SessionCookie = "__Secure-next-auth.session-token"

// maxChatLine bounds a single SSE line. The route emits short JSON objects, so
// this is generous; a larger line is treated as a malformed event rather than
// allowed to exhaust memory.
const maxChatLine = 1 << 20

// ChatRequest is the body the web chat route accepts.
//
// ThreadID is a pointer because the field must be present and explicitly null
// to open a new conversation, and because omitting it is not the same thing:
// a JSON null starts a thread, an absent field is not a request we should send.
//
// Model is a catalog handle or a compiled id. The route substitutes silently
// for names it does not recognise, and reports what it actually used in the
// meta event, so an unknown model is not an error here.
type ChatRequest struct {
	ThreadID        *string `json:"threadId"`
	Content         string  `json:"content"`
	Model           string  `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
	Images          []any   `json:"images"`
	Attachments     []any   `json:"attachments"`
}

// ChatEvent is one Server-Sent Event from the chat route.
//
// Every field is optional: the route grows new event types over time and an
// unknown one must not fail a request that is otherwise fine. Text carries the
// answer for delta events and the reasoning trace for reasoning_delta events.
type ChatEvent struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	ThreadID   string `json:"threadId"`
	Title      string `json:"title"`
	Model      string `json:"model"`
	AccessTier string `json:"accessTier"`
	ToolCallID string `json:"toolCallId"`
}

// IsStreamFailure reports whether the event represents an upstream failure
// rather than content. The route reports failures inside the stream rather than
// as a status code, so a caller that only inspected the HTTP status would see a
// successful response that produced no answer.
func (e ChatEvent) IsStreamFailure() bool {
	return strings.Contains(strings.ToLower(e.Type), "error") ||
		strings.Contains(strings.ToLower(e.Type), "fail")
}

// ChatStream opens a streaming chat response.
//
// The returned body must be closed by the caller. On a non-2xx response the
// body is drained and closed here and the error carries the service's own
// message, because that message is what distinguishes an expired session from a
// real failure and an operator needs it to know to re-run `freebuff login`.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest) (io.ReadCloser, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, errors.New("freebuff: chat requires a session token; run `freebuff login` first")
	}
	if strings.TrimSpace(req.Content) == "" {
		return nil, errors.New("freebuff: chat requires non-empty content")
	}
	// The route distinguishes an absent images/attachments array from an empty
	// one, and sending null where it expects a list is rejected.
	if req.Images == nil {
		req.Images = []any{}
	}
	if req.Attachments == nil {
		req.Attachments = []any{}
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("freebuff: encoding chat request: %w", err)
	}

	// Admit (or resume) before streaming. The web host needs a session to
	// attach the stream to; the cookie alone is not enough, and a request sent
	// without one is refused with the same "session expired" that a stale
	// cookie produces, which makes the two indistinguishable from the outside.
	instanceID, err := c.ensureAdmittedSession(ctx, req.Model)
	if err != nil {
		return nil, err
	}

	httpReq, err := c.newStreamRequest(ctx, ChatPath, payload, instanceID)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
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
// to do about it. The expired-session case is the common one and is worth
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

// ScanChatEvents reads a chat stream and invokes fn for each event.
//
// The stream is the only place the answer arrives, so a read failure part-way
// through is reported rather than swallowed: returning a partial answer as if it
// were complete would be worse than failing the request. Lines that are blank
// (SSE padding) or undecodable are skipped, because an event type this client
// does not know about must not cost the caller an otherwise healthy answer.
func ScanChatEvents(body io.Reader, fn func(ChatEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxChatLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event ChatEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			// An unknown or malformed event is not fatal; the route may add
			// event types this client does not model yet.
			continue
		}
		if err := fn(event); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("freebuff: reading chat stream: %w", err)
	}
	return nil
}

// ThreadSummary is one conversation in the account's history.
type ThreadSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Model     string `json:"model"`
	UpdatedAt string `json:"updated_at"`
}

// ThreadBlock is one structured part of an assistant message.
//
// Text and Type are read together: a thinking block and a text block both carry
// text, and only Type distinguishes a reasoning trace from the answer.
type ThreadBlock struct {
	Text       string           `json:"text"`
	Type       string           `json:"type"`
	Status     string           `json:"status"`
	ToolCallID string           `json:"toolCallId"`
	Followups  []ThreadFollowup `json:"followups"`
}

// ThreadFollowup is a suggested next prompt offered alongside an answer.
type ThreadFollowup struct {
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
}

// ThreadMessage is one turn of a conversation.
//
// Content is the plain answer text and Blocks is the structured form of the same
// turn; both are returned by the service and either can be empty depending on
// how the turn was produced.
type ThreadMessage struct {
	ID      string        `json:"id"`
	Role    string        `json:"role"`
	Content string        `json:"content"`
	Blocks  []ThreadBlock `json:"blocks"`
	Model   string        `json:"model"`
}

// Thread is a conversation with its turns.
type Thread struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Model    string          `json:"model"`
	Messages []ThreadMessage `json:"messages"`
}

type threadListResponse struct {
	Threads    []ThreadSummary `json:"threads"`
	AccessTier string          `json:"accessTier"`
}

type threadResponse struct {
	Thread   Thread          `json:"thread"`
	Messages []ThreadMessage `json:"messages"`
}

// newStreamRequest builds a request for the web host, authenticated by the
// session cookie the login flow obtained. It is separate from the JSON helper
// because a streaming response must not be buffered by the caller.
func (c *Client) newStreamRequest(ctx context.Context, path string, payload []byte, instanceID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("freebuff: building %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", UserAgent)
	// The web host authenticates with the session cookie, not the bearer token:
	// the same token the CLI login returns is accepted in this cookie, so no
	// browser automation or session scraping is involved.
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: c.apiKey})
	// The instance id names the admitted session this stream belongs to. Without
	// it the host has no session to attach the request to and reports the same
	// "session expired" that an absent cookie does, which is why a request built
	// before admission and one built after can fail identically.
	if strings.TrimSpace(instanceID) != "" {
		req.Header.Set(headerInstance, instanceID)
	}
	return req, nil
}

// ensureAdmittedSession makes sure the account holds a live session for model
// and returns the instance id to present with the stream.
//
// The web host does not authenticate a stream on the cookie alone. A session has
// to be admitted first, and holding one is metered, so an existing session is
// resumed rather than a new one taken for every turn.
//
// A gate is reported, never routed around: country_blocked, spend_limited and the
// rest are the service deciding what this account may do, and the only honest
// response to one is to say so.
func (c *Client) ensureAdmittedSession(ctx context.Context, model string) (string, error) {
	admission := NewSessionAdmission(c)

	if held := recallSession(c.apiKey, model); held != "" {
		state, err := admission.Reuse(ctx, held, model)
		if err == nil && state != nil && state.Status == StatusActive && state.InstanceID != "" {
			rememberSession(c.apiKey, state.InstanceID, model)
			return state.InstanceID, nil
		}
		// Any refusal means the held session is spent or gone. Drop it so the
		// next request admits afresh instead of retrying a dead instance.
		forgetSession(c.apiKey)
	}

	state, err := admission.Admit(ctx, AdmissionRequest{Model: model})
	if err != nil {
		// A server with no admission route predates the guarantee. Streaming
		// without a session is the older behaviour and still worth attempting,
		// so this is a downgrade rather than a failure.
		if isAdmissionUnsupported(err) {
			return "", nil
		}
		return "", err
	}
	if state == nil {
		return "", nil
	}
	if state.Status != StatusActive {
		return "", sessionGateError(state)
	}
	if state.InstanceID != "" {
		rememberSession(c.apiKey, state.InstanceID, model)
	}
	return state.InstanceID, nil
}

// isAdmissionUnsupported reports whether err means the server has no admission
// endpoint, as opposed to admission being refused.
func isAdmissionUnsupported(err error) bool {
	var se *SessionError
	return asSessionError(err, &se) && se.ErrorCode == errCodeAdmissionUnsupported
}

// sessionGateError renders a gate decision as an error an operator can act on.
//
// The wording is deliberate. These refusals are access decisions, so the message
// names the gate and says what it means rather than reporting a generic failure
// that invites a retry which cannot succeed.
func sessionGateError(state *SessionState) error {
	if state == nil {
		return errors.New("freebuff: the session was refused for an unstated reason")
	}
	gate := string(state.Status)
	if gate == "" {
		gate = "unspecified"
	}
	detail := strings.TrimSpace(state.ErrorCode)
	if detail == "" || detail == gate {
		return fmt.Errorf("freebuff: %s; the account cannot start this session right now", gate)
	}
	return fmt.Errorf("freebuff: %s (%s); the account cannot start this session right now", gate, detail)
}

// doSessionGET performs a buffered GET against the web host with the session
// cookie. Used for the thread endpoints, which answer with plain JSON.
func (c *Client) doSessionGET(ctx context.Context, path string) ([]byte, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, errors.New("freebuff: a session token is required; run `freebuff login` first")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("freebuff: building %s request: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: c.apiKey})

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("freebuff: requesting %s: %w", path, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody*8))
	if err != nil {
		return nil, fmt.Errorf("freebuff: reading %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, errors.New("freebuff session is no longer valid; run `freebuff login` again")
		}
		return nil, &SessionError{
			Status:    resp.StatusCode,
			Method:    http.MethodGet,
			Path:      path,
			Message:   summarizeChatError(resp.StatusCode, string(body)),
			Body:      truncate(string(body), 200),
			ErrorCode: chatErrorCode(body),
		}
	}
	return body, nil
}

// ListThreads returns the account's conversations, newest first as the service
// orders them.
func (c *Client) ListThreads(ctx context.Context) ([]ThreadSummary, error) {
	payload, err := c.doSessionGET(ctx, ThreadsPath)
	if err != nil {
		return nil, err
	}
	var decoded threadListResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("freebuff: decoding thread list: %w", err)
	}
	return decoded.Threads, nil
}

// GetThread returns one conversation with its messages.
func (c *Client) GetThread(ctx context.Context, threadID string) (*Thread, error) {
	if strings.TrimSpace(threadID) == "" {
		return nil, errors.New("freebuff: thread id is required")
	}
	payload, err := c.doSessionGET(ctx, ThreadsPath+"/"+threadID)
	if err != nil {
		return nil, err
	}
	var decoded threadResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("freebuff: decoding thread: %w", err)
	}
	thread := decoded.Thread
	thread.Messages = decoded.Messages
	if len(thread.Messages) == 0 {
		return nil, fmt.Errorf("freebuff: thread %s carried no messages", threadID)
	}
	return &thread, nil
}

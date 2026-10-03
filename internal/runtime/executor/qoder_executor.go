package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// qoderMaxStderrLog bounds how much CLI stderr is captured for server-side logs.
const qoderMaxStderrLog = 4096

// qoderStatusError carries an HTTP-like status so the auth conductor can apply
// cooling to the credential that produced the failure.
type qoderStatusError struct {
	code   int
	status string
	err    error
}

func (e *qoderStatusError) Error() string { return e.err.Error() }

func (e *qoderStatusError) Unwrap() error { return e.err }

func (e *qoderStatusError) StatusCode() int { return e.code }

// qoderCredential is the resolved backend and personal access token for one request.
type qoderCredential struct {
	backend string
	token   string
}

// QoderExecutor executes requests by running the Qoder CLI as a one-shot
// subprocess. The CLI speaks the Claude Code streaming protocol, so responses
// are produced in Claude format and converted by the existing translator hub.
type QoderExecutor struct {
	cfg *config.Config
}

// NewQoderExecutor creates a new Qoder executor.
func NewQoderExecutor(cfg *config.Config) *QoderExecutor {
	return &QoderExecutor{cfg: cfg}
}

// Identifier returns the provider identifier.
func (e *QoderExecutor) Identifier() string { return "qoder" }

// RequestToFormat declares Qoder's wire format. The CLI emits Claude Code
// protocol events, so Claude is used as the provider format and the existing
// Claude translators handle every client interface.
func (e *QoderExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatClaude
}

// qoderCredentialFromAuth resolves the backend and token from the credential the
// scheduler selected, so round-robin, cooling and per-key restrictions apply.
func qoderCredentialFromAuth(auth *cliproxyauth.Auth) (qoderCredential, error) {
	cred := qoderCredential{backend: registry.QoderBackendGlobal}
	if auth == nil {
		return cred, &qoderStatusError{
			code:   http.StatusUnauthorized,
			status: "no credential",
			err:    errors.New("qoder: no credential selected"),
		}
	}
	if raw, ok := auth.Attributes["backend"]; ok {
		cred.backend = raw
	}
	if raw, ok := auth.Attributes["api_key"]; ok {
		cred.token = raw
	}
	if !registry.IsValidQoderBackend(cred.backend) {
		return cred, &qoderStatusError{
			code:   http.StatusBadRequest,
			status: "invalid backend",
			err:    fmt.Errorf("qoder: unsupported backend %q", cred.backend),
		}
	}
	cred.backend = registry.NormalizeQoderBackend(cred.backend)
	return cred, nil
}

// qoderResolveBinary locates the CLI for the credential's backend.
func qoderResolveBinary(backend string) (string, error) {
	name := QoderBinaryForBackend(backend)
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", &qoderStatusError{
			code:   http.StatusServiceUnavailable,
			status: "cli missing",
			err:    fmt.Errorf("qoder: %s not found on PATH; install the Qoder CLI or set QODER_PATH", name),
		}
	}
	return resolved, nil
}

// startQoderCLI launches the CLI in non-interactive print mode with the
// streaming protocol, and feeds it the rendered prompt on stdin.
func startQoderCLI(ctx context.Context, resolved, model, prompt string, cred qoderCredential) (*exec.Cmd, io.ReadCloser, *qoderStderr, error) {
	cmd := exec.CommandContext(ctx, resolved, "-p", "-m", model, "-o", "stream-json")
	cmd.Env = os.Environ()
	// The CN backend authenticates with a personal access token. The global
	// backend uses the credentials from `qodercli login` and must not be given
	// a token, because it would be sent to the wrong endpoint.
	if cred.backend == registry.QoderBackendCN && cred.token != "" {
		cmd.Env = append(cmd.Env, "QODERCN_PERSONAL_ACCESS_TOKEN="+cred.token)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("qoder: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("qoder: stdout pipe: %w", err)
	}
	// The token is injected into the child environment only, so it has to be
	// registered for redaction explicitly.
	var secrets []string
	if cred.token != "" {
		secrets = append(secrets, cred.token)
	}
	stderr := &qoderStderr{secrets: secrets}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, nil, nil, &qoderStatusError{
			code:   http.StatusServiceUnavailable,
			status: "spawn failed",
			err:    fmt.Errorf("qoder: failed to start CLI: %w", err),
		}
	}

	// The CLI reads its prompt from stdin and terminates it to start work.
	go func() {
		defer func() { _ = stdin.Close() }()
		_, _ = io.WriteString(stdin, prompt)
	}()

	return cmd, stdout, stderr, nil
}

// qoderStderr captures a bounded amount of CLI stderr for server-side logging.
type qoderStderr struct {
	// secrets holds credential values injected into the child environment; they
	// must be masked on the way out because the parent env does not contain them.
	secrets []string

	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *qoderStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() < qoderMaxStderrLog {
		s.buf.Write(p)
	}
	return len(p), nil
}

func (s *qoderStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return redactQoderSecrets(s.buf.String(), s.secrets...)
}

// redactQoderSecrets strips anything that looks like a credential before stderr
// reaches the logs.
// redactQoderSecrets truncates CLI stderr and masks credential values. The
// secrets argument must carry the token actually injected into the child
// process, because the parent environment does not hold it.
func redactQoderSecrets(s string, secrets ...string) string {
	if len(s) > qoderMaxStderrLog {
		s = s[:qoderMaxStderrLog] + "... (truncated)"
	}
	candidates := make([]string, 0, len(secrets)+4)
	candidates = append(candidates, secrets...)
	candidates = append(candidates, os.Getenv("QODERCN_PERSONAL_ACCESS_TOKEN"),
		os.Getenv("QODER_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	for _, v := range candidates {
		// Very short values would redact unrelated text, so require a length
		// that cannot plausibly be a common substring.
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "***redacted***")
		}
	}
	return s
}

// qoderPromptForRequest translates the incoming request into the Qoder CLI's
// Claude Messages wire format and renders it as the natural-language prompt the
// CLI reads from stdin. The CLI is a prompt-based agent, so the request body
// must be rendered rather than forwarded verbatim.
func qoderPromptForRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (string, error) {
	from := opts.SourceFormat
	if from == "" {
		from = req.Format
	}
	// A Claude-native client is already in the target format, and the registry
	// has no claude->claude request transformer, so the body must pass through
	// untouched rather than be replaced with an empty translation.
	claudeBody := req.Payload
	if from != sdktranslator.FormatClaude && sdktranslator.HasRequestTransformer(from, sdktranslator.FormatClaude) {
		claudeBody = sdktranslator.TranslateRequest(from, sdktranslator.FormatClaude, req.Model, req.Payload, stream)
	}
	return qoderPromptFromClaudeRequest(claudeBody)
}

// Execute performs a non-streaming request through the Qoder CLI.
func (e *QoderExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	cred, err := qoderCredentialFromAuth(auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	prompt, err := qoderPromptForRequest(req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	resolved, err := qoderResolveBinary(cred.backend)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	cmd, stdout, stderr, err := startQoderCLI(ctx, resolved, req.Model, prompt, cred)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	acc := &qoderAccumulator{model: req.Model}
	scanErr := qoderScanStream(stdout, func(ev qoderEvent) {
		acc.add(ev)
	})
	waitErr := cmd.Wait()
	if stderrOut := stderr.String(); stderrOut != "" {
		log.WithField("provider", "qoder").Debugf("qoder CLI stderr: %s", stderrOut)
	}
	if err := ctx.Err(); err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qoder: request cancelled: %w", err)
	}
	if scanErr != nil {
		return cliproxyexecutor.Response{}, &qoderStatusError{
			code:   http.StatusBadGateway,
			status: "read failed",
			err:    fmt.Errorf("qoder: failed to read CLI output: %w", scanErr),
		}
	}
	if resultErr := acc.resultError(); resultErr != nil {
		return cliproxyexecutor.Response{}, resultErr
	}
	if waitErr != nil {
		return cliproxyexecutor.Response{}, &qoderStatusError{
			code:   http.StatusBadGateway,
			status: "cli failed",
			err:    fmt.Errorf("qoder: CLI exited with error: %w", waitErr),
		}
	}
	if acc.text() == "" {
		return cliproxyexecutor.Response{}, &qoderStatusError{
			code:   http.StatusBadGateway,
			status: "empty response",
			err:    errors.New("qoder: CLI produced no assistant output"),
		}
	}

	payload, err := acc.nonStreamPayload(req.Model)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	var param any
	body := req.Payload
	translated := sdktranslator.TranslateNonStream(
		ctx,
		sdktranslator.FormatClaude,
		cliproxyexecutor.ResponseFormatOrSource(opts),
		req.Model,
		opts.OriginalRequest,
		body,
		payload,
		&param,
	)
	return cliproxyexecutor.Response{Payload: translated}, nil
}

// ExecuteStream performs a streaming request through the Qoder CLI.
func (e *QoderExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	cred, err := qoderCredentialFromAuth(auth)
	if err != nil {
		return nil, err
	}
	prompt, err := qoderPromptForRequest(req, opts, true)
	if err != nil {
		return nil, err
	}
	resolved, err := qoderResolveBinary(cred.backend)
	if err != nil {
		return nil, err
	}

	cmd, stdout, stderr, err := startQoderCLI(ctx, resolved, req.Model, prompt, cred)
	if err != nil {
		return nil, err
	}

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	out := make(chan cliproxyexecutor.StreamChunk, 8)
	relay := &qoderClaudeRelay{model: req.Model}
	var param any

	// The channel is always closed, including on every error path, so downstream
	// forwarders terminate instead of blocking forever.
	go func() {
		defer close(out)
		var werr error
		defer func() {
			_ = cmd.Wait()
			if stderrOut := stderr.String(); stderrOut != "" {
				log.WithField("provider", "qoder").Debugf("qoder CLI stderr: %s", stderrOut)
			}
			if werr == nil {
				werr = ctx.Err()
			}
			if werr == nil {
				werr = relay.finish(ctx, out, &param, responseFormat, req, opts)
			}
			if werr != nil {
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: werr}:
				case <-ctx.Done():
				}
			}
		}()

		werr = qoderScanStream(stdout, func(ev qoderEvent) {
			if werr != nil {
				return
			}
			if err := relay.consume(ctx, out, &param, responseFormat, req, opts, ev); err != nil {
				werr = err
			}
		})
	}()

	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

// CountTokens is not supported by the Qoder CLI.
func (e *QoderExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &qoderStatusError{
		code:   http.StatusNotImplemented,
		status: "unsupported",
		err:    errors.New("qoder: CountTokens is not supported"),
	}
}

// Refresh is a no-op: the CLI owns its own credential lifecycle.
func (e *QoderExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

// HttpRequest is not supported by the Qoder CLI.
func (e *QoderExecutor) HttpRequest(_ context.Context, _ *cliproxyauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, &qoderStatusError{
		code:   http.StatusNotImplemented,
		status: "unsupported",
		err:    errors.New("qoder: HttpRequest is not supported"),
	}
}

// qoderEvent is one decoded line of the CLI's stream-json output.
// qoderContentBlock is one block of an assistant message.
type qoderContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// qoderUsageTokens is the usage block the CLI reports on a result event.
type qoderUsageTokens struct {
	InputTokens        int64 `json:"input_tokens"`
	CacheCreationInput int64 `json:"cache_creation_input_tokens"`
	CacheReadInput     int64 `json:"cache_read_input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
}

// qoderMessage is the embedded message object of an assistant event.
type qoderMessage struct {
	Content []qoderContentBlock `json:"content"`
	Usage   qoderUsageTokens    `json:"usage"`
}

// qoderEvent is one newline-delimited event from the CLI's stream-json output.
type qoderEvent struct {
	Type    string       `json:"type"`
	Subtype string       `json:"subtype"`
	Message qoderMessage `json:"message"`
	Model   string       `json:"model"`
	IsError bool         `json:"is_error"`
	Errors  []string     `json:"errors"`
	// ErrorCode is the CLI's own code; 118 means the account is out of credits.
	ErrorCode  int    `json:"error_code"`
	StopReason string `json:"stop_reason"`
}

// text returns the concatenated assistant text carried by the event.
func (ev qoderEvent) text() string {
	var b strings.Builder
	for _, c := range ev.Message.Content {
		if c.Type == "text" || c.Type == "" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// qoderScanStream decodes newline-delimited CLI events. Malformed lines are
// logged and skipped so a single bad line cannot fail the whole response.
func qoderScanStream(r io.Reader, fn func(qoderEvent)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev qoderEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			log.WithField("provider", "qoder").Debugf("qoder: skipping undecodable CLI line: %v", err)
			continue
		}
		fn(ev)
	}
	return scanner.Err()
}

// qoderAccumulator collects CLI events for a non-streaming response.
type qoderAccumulator struct {
	mu      sync.Mutex
	model   string
	parts   []string
	usage   qoderUsage
	stop    string
	failure error
}

type qoderUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (a *qoderAccumulator) add(ev qoderEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ev.Model != "" && !strings.EqualFold(ev.Model, "auto") {
		a.model = ev.Model
	}
	switch ev.Type {
	case "assistant":
		if t := ev.text(); t != "" {
			a.parts = append(a.parts, t)
		}
	case "result":
		if ev.IsError {
			a.failure = qoderErrorFromEvent(ev)
		}
		if ev.Message.Usage.InputTokens > 0 {
			a.usage.Input = ev.Message.Usage.InputTokens
		}
		a.usage.Output = ev.Message.Usage.OutputTokens
		a.usage.CacheRead = ev.Message.Usage.CacheReadInput
		a.usage.CacheWrite = ev.Message.Usage.CacheCreationInput
		if ev.StopReason != "" {
			a.stop = ev.StopReason
		}
	}
}

func (a *qoderAccumulator) text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.parts, "")
}

// resultError returns the classified error from a failed CLI run, if any.
func (a *qoderAccumulator) resultError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failure
}

// nonStreamPayload renders the accumulated text as a Claude Messages response.
func (a *qoderAccumulator) nonStreamPayload(requestedModel string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	model := a.model
	if model == "" {
		model = requestedModel
	}
	stop := a.stop
	if stop == "" {
		stop = "end_turn"
	}
	body := map[string]any{
		"id":            "msg_" + qoderMessageID(a.parts),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]string{{"type": "text", "text": strings.Join(a.parts, "")}},
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                a.usage.Input,
			"output_tokens":               a.usage.Output,
			"cache_read_input_tokens":     a.usage.CacheRead,
			"cache_creation_input_tokens": a.usage.CacheWrite,
		},
	}
	return json.Marshal(body)
}

// qoderErrorFromEvent classifies a failed CLI result into a status-carrying
// error so the conductor can cool the offending credential.
func qoderErrorFromEvent(ev qoderEvent) error {
	if !ev.IsError {
		return nil
	}
	msg := strings.TrimSpace(strings.Join(ev.Errors, "; "))
	if msg == "" {
		msg = fmt.Sprintf("qoder CLI reported error code %d", ev.ErrorCode)
	}
	lower := strings.ToLower(msg)
	code := http.StatusBadGateway
	switch {
	case strings.Contains(lower, "credit"), strings.Contains(lower, "quota"),
		strings.Contains(lower, "upgrade"), strings.Contains(lower, "balance"),
		ev.ErrorCode == 118:
		code = http.StatusPaymentRequired
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"):
		code = http.StatusTooManyRequests
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "not logged in"),
		strings.Contains(lower, "authentication"), strings.Contains(lower, "token"):
		code = http.StatusUnauthorized
	}
	// The CLI echoes user-visible text; keep it, it carries no credentials.
	return &qoderStatusError{code: code, status: "cli error", err: errors.New("qoder: " + msg)}
}

// qoderMessageID derives a stable synthetic message id from the response parts.
func qoderMessageID(parts []string) string {
	sum := uint64(1469598103934665603)
	for _, p := range parts {
		for i := 0; i < len(p); i++ {
			sum ^= uint64(p[i])
			sum *= 1099511628211
		}
	}
	return fmt.Sprintf("%016x", sum)
}

// qoderClaudeRelay converts CLI events into Claude streaming events and pushes
// them through the translator hub so any client format is supported.
type qoderClaudeRelay struct {
	model      string
	started    bool
	blockOpen  bool
	blockIndex int
	stopReason string
	usage      qoderUsage
	emitted    int
}

func (r *qoderClaudeRelay) consume(
	ctx context.Context,
	out chan<- cliproxyexecutor.StreamChunk,
	param *any,
	responseFormat sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	ev qoderEvent,
) error {
	if ev.Model != "" && !strings.EqualFold(ev.Model, "auto") {
		r.model = ev.Model
	}
	switch ev.Type {
	case "assistant":
		text := ev.text()
		if text == "" {
			return nil
		}
		// Each assistant event is a complete turn, so it becomes its own block.
		if err := r.closeBlock(ctx, out, param, responseFormat, req, opts); err != nil {
			return err
		}
		if err := r.open(ctx, out, param, responseFormat, req, opts); err != nil {
			return err
		}
		return r.push(ctx, out, param, responseFormat, req, opts, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": r.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	case "result":
		if ev.IsError {
			return qoderErrorFromEvent(ev)
		}
		if ev.Message.Usage.InputTokens > 0 {
			r.usage.Input = ev.Message.Usage.InputTokens
		}
		r.usage.Output = ev.Message.Usage.OutputTokens
		r.usage.CacheRead = ev.Message.Usage.CacheReadInput
		r.usage.CacheWrite = ev.Message.Usage.CacheCreationInput
		if ev.StopReason != "" {
			r.stopReason = ev.StopReason
		}
	}
	return nil
}

// finish emits the terminal Claude events (content_block_stop, message_delta,
// message_stop) so clients see a well-formed stream with usage accounting.
// It returns an error only when the CLI produced nothing at all.
func (r *qoderClaudeRelay) finish(
	ctx context.Context,
	out chan<- cliproxyexecutor.StreamChunk,
	param *any,
	responseFormat sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) error {
	if r.emitted == 0 {
		return &qoderStatusError{
			code:   http.StatusBadGateway,
			status: "empty response",
			err:    errors.New("qoder: CLI produced no assistant output"),
		}
	}

	// Close any block left open by the last assistant turn.
	if err := r.closeBlock(ctx, out, param, responseFormat, req, opts); err != nil {
		return err
	}

	stopReason := r.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	if err := r.push(ctx, out, param, responseFormat, req, opts, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":                r.usage.Input,
			"output_tokens":               r.usage.Output,
			"cache_read_input_tokens":     r.usage.CacheRead,
			"cache_creation_input_tokens": r.usage.CacheWrite,
		},
	}); err != nil {
		return err
	}

	return r.push(ctx, out, param, responseFormat, req, opts, "message_stop", map[string]any{
		"type": "message_stop",
	})
}

func (r *qoderClaudeRelay) open(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, param *any, responseFormat sdktranslator.Format, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	if !r.started {
		if err := r.push(ctx, out, param, responseFormat, req, opts, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_" + qoderMessageID(nil), "type": "message", "role": "assistant",
				"model": r.model, "content": []any{},
				"stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": r.usage.Input, "output_tokens": 0},
			},
		}); err != nil {
			return err
		}
		r.started = true
	}
	r.blockIndex++
	if err := r.push(ctx, out, param, responseFormat, req, opts, "content_block_start", map[string]any{
		"type": "content_block_start", "index": r.blockIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	r.blockOpen = true
	return nil
}

func (r *qoderClaudeRelay) closeBlock(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, param *any, responseFormat sdktranslator.Format, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	if !r.blockOpen {
		return nil
	}
	err := r.push(ctx, out, param, responseFormat, req, opts, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": r.blockIndex,
	})
	r.blockOpen = false
	return err
}

// push renders one Claude event, translates it into the client format, and
// forwards the resulting chunks.
func (r *qoderClaudeRelay) push(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, param *any, responseFormat sdktranslator.Format, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, eventType string, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	line := append([]byte("data: "), raw...)
	for _, chunk := range sdktranslator.TranslateStream(
		ctx,
		sdktranslator.FormatClaude,
		responseFormat,
		req.Model,
		opts.OriginalRequest,
		req.Payload,
		line,
		param,
	) {
		if len(chunk) == 0 {
			continue
		}
		r.emitted++
		select {
		case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// compile-time interface check
var _ interface {
	Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
	HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error)
	Identifier() string
} = (*QoderExecutor)(nil)

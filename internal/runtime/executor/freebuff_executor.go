package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/freebuff"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// freebuffUnconfiguredMessage is returned when an operator tries to use the
// provider without a base URL. The web host is where login and chat live and is
// configurable so an operator can point this at a self-hosted instance, but
// there is no default: a guessed origin turns a typo into what looks like an
// upstream outage.
const freebuffUnconfiguredMessage = "freebuff: no base URL is configured. Set `freebuff.base-url` in the config, or run `freebuff login` to store a credential."

// FreebuffExecutor speaks the Freebuff/Codebuff wire protocol.
//
// The protocol is OpenAI-shaped on the surface, with one addition that has no
// OpenAI equivalent: a chat completion is only accepted inside a run, so every
// request has to acquire a run, carry its id in a `codebuff_metadata` envelope,
// and release it afterwards. That envelope is why an OpenAI-compatible executor
// cannot simply be pointed at this service.
type FreebuffExecutor struct {
	cfg *config.Config

	// catalogBaseURL is the origin that serves the model catalogue.
	//
	// It is a field rather than a constant because it is a different origin from
	// the web host that serves chat, and a test must be able to point both at one
	// in-process fixture. Production leaves it at the documented host.
	catalogBaseURL string

	// clientFactory builds the protocol client. Production uses the default
	// factory, which reads the endpoint from configuration; tests substitute
	// their own so they can point the client at an in-process fixture.
	clientFactory func(baseURL, apiKey string) (*freebuff.Client, error)
}

// NewFreebuffExecutor creates a Freebuff executor. The endpoint comes from the
// `freebuff` config section and the token from the selected credential.
func NewFreebuffExecutor(cfg *config.Config) *FreebuffExecutor {
	return newFreebuffExecutorWithClientFactory(cfg, newDefaultFreebuffClient)
}

// newDefaultFreebuffClient builds a client for the configured endpoint. The
// default HTTP client is used so the request inherits the process proxy
// settings the rest of the server already honours.
func newDefaultFreebuffClient(baseURL, apiKey string) (*freebuff.Client, error) {
	return freebuff.NewClient(baseURL, apiKey, freebuff.Options{})
}

// newFreebuffExecutorWithClientFactory creates an executor with a caller
// supplied client factory. Tests use this to inject a fixture endpoint.
func newFreebuffExecutorWithClientFactory(cfg *config.Config, factory func(baseURL, apiKey string) (*freebuff.Client, error)) *FreebuffExecutor {
	return &FreebuffExecutor{cfg: cfg, catalogBaseURL: freebuffCatalogHost, clientFactory: factory}
}

// withCatalogBaseURL points the catalogue at a different origin.
//
// It exists so a test can serve chat and the catalogue from one fixture: the two
// are separate origins in production, and a test that could only reach the
// hard-coded catalogue host would exercise nothing.
func (e *FreebuffExecutor) withCatalogBaseURL(baseURL string) *FreebuffExecutor {
	if strings.TrimSpace(baseURL) != "" {
		e.catalogBaseURL = baseURL
	}
	return e
}

// Identifier returns the provider identifier.
func (e *FreebuffExecutor) Identifier() string { return "freebuff" }

// RequestToFormat reports the provider's wire format. The upstream is
// OpenAI-shaped, so the existing OpenAI translator tree is reused verbatim and
// no Freebuff-specific translation code exists.
func (e *FreebuffExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

// freebuffAPIKey extracts the API key from the selected credential.
func freebuffAPIKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["api_key"])
}

// freebuffBaseURL resolves the endpoint for a request.
//
// A credential may carry its own base_url, which wins, so a harness or a
// one-off credential can be pointed somewhere specific. Otherwise the
// `freebuff.base-url` config value is used. There is no hardcoded default:
// assuming an origin would turn a missing setting into what looks like an
// upstream outage rather than a configuration mistake.
func (e *FreebuffExecutor) freebuffBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if fromCredential := strings.TrimSpace(auth.Attributes["base_url"]); fromCredential != "" {
			return fromCredential
		}
	}
	if e.cfg == nil {
		return ""
	}
	return strings.TrimSpace(e.cfg.Freebuff.BaseURL)
}

// resolveClient returns a protocol client for the selected credential, or an
// error explaining why the provider cannot be used.
func (e *FreebuffExecutor) resolveClient(auth *cliproxyauth.Auth) (*freebuff.Client, error) {
	apiKey := freebuffAPIKey(auth)
	if apiKey == "" {
		return nil, freebuffStatusError{code: http.StatusUnauthorized, msg: "freebuff: selected credential has no api_key attribute", credentials: true}
	}
	baseURL := e.freebuffBaseURL(auth)
	if baseURL == "" {
		return nil, freebuffStatusError{
			code: http.StatusNotImplemented,
			msg:  "freebuff: no base URL is configured. Set `freebuff.base-url` in the config, or run `freebuff login` to store a credential.",
		}
	}
	client, err := e.clientFactory(baseURL, apiKey)
	if err != nil {
		return nil, freebuffStatusError{code: http.StatusInternalServerError, msg: err.Error()}
	}
	return client, nil
}

// freebuffStatusError carries an HTTP-like status so the conductor can cool a
// credential on 401 and honour Retry-After on 429.
type freebuffStatusError struct {
	code        int
	msg         string
	retryAfter  *time.Duration
	credentials bool
}

func (e freebuffStatusError) Error() string { return e.msg }

// StatusCode implements cliproxyexecutor.StatusError.
func (e freebuffStatusError) StatusCode() int { return e.code }

// RetryAfter implements the conductor's retry-after convention, which is a
// pointer: nil means "no guidance".
func (e freebuffStatusError) RetryAfter() *time.Duration { return e.retryAfter }

// IsCredentialScoped reports that the failure is tied to the credential rather
// than the request, so the conductor should stop routing to this key.
func (e freebuffStatusError) IsCredentialScoped() bool { return e.credentials }

var _ cliproxyexecutor.StatusError = freebuffStatusError{}

// prepare translates the incoming request into the provider's OpenAI format and
// applies the same payload adjustments the OpenAI-compatible path applies.
func (e *FreebuffExecutor) prepare(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string, stream bool) ([]byte, []byte, error) {
	from := opts.SourceFormat
	to := sdktranslator.FormatOpenAI

	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, stream, isCompat)
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, stream, isCompat)

	translated, err := helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	translated = helps.NormalizeOpenAIToolResultsTextOnly(translated)
	useMCT := helps.ShouldUseMaxCompletionTokensForModel(nil, baseModel, requestedModel)
	translated = helps.NormalizeOpenAIMaxTokens(translated, useMCT)
	if stream {
		// Ask for usage in the final chunk so token statistics survive an
		// OpenAI-shaped upstream that would otherwise omit them.
		translated = helps.SetBoolIfDifferent(translated, "stream_options.include_usage", true)
	}
	return translated, originalPayload, nil
}

// Execute performs a non-streaming completion.
func (e *FreebuffExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	client, err := e.resolveClient(auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	translated, originalPayload, err := e.prepare(ctx, req, opts, baseModel, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	reporter.SetTranslatedReasoningEffort(translated, sdktranslator.FormatOpenAI.String())

	// The web route streams its answer, so a non-streaming request accumulates
	// the deltas rather than receiving one JSON body. There is no run lifecycle
	// to retry here: the route is idempotent per conversation, and a retried
	// turn would appear twice in the thread history.
	completion, chatErr := e.executeChat(ctx, client, translated, req, freebuffThreadID(ctx, opts), nil)
	if chatErr != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, chatErr)
		return cliproxyexecutor.Response{}, chatErr
	}

	body := completion.nonStreamResponse(freebuffCompletionID(completion), req.Model)
	if body == nil {
		err = freebuffStatusError{code: 502, msg: "freebuff: the completion could not be rendered"}
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)

	reporter.ObserveResponseModel(body)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	reporter.EnsurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, responseFormat, req.Model, originalPayload, translated, body, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	return cliproxyexecutor.Response{Payload: out}, nil
}

// freebuffThreadID returns the conversation to continue, if the caller supplied
// one. An empty result starts a new conversation, which is the default: the
// proxy holds no conversation state between requests, so a caller that wants a
// follow-up turn must name the thread.
func freebuffThreadID(_ context.Context, opts cliproxyexecutor.Options) string {
	if opts.Metadata == nil {
		return ""
	}
	for _, key := range []string{"freebuff_thread_id", "thread_id"} {
		if value, ok := opts.Metadata[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// freebuffCompletionID derives a stable identifier for one answer from the
// conversation and answer text, so a retry of the same turn does not produce a
// different id for the same content.
func freebuffCompletionID(completion *chatCompletion) string {
	seed := completion.threadID + "|" + completion.text()
	digest := fnv64(seed)
	return fmt.Sprintf("chatcmpl-freebuff-%016x", digest)
}

// fnv64 is the FNV-1a 64-bit hash, used inline to avoid pulling a dependency in
// for one identifier.
func fnv64(s string) uint64 {
	const (
		offset64 = uint64(14695981039346656037)
		prime64  = uint64(1099511628211)
	)
	hash := offset64
	for i := 0; i < len(s); i++ {
		hash ^= uint64(s[i])
		hash *= prime64
	}
	return hash
}

// ExecuteStream performs a streaming completion.
func (e *FreebuffExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetStream(true)

	client, err := e.resolveClient(auth)
	if err != nil {
		return nil, err
	}

	translated, _, err := e.prepare(ctx, req, opts, baseModel, true)
	if err != nil {
		return nil, err
	}
	reporter.SetTranslatedReasoningEffort(translated, sdktranslator.FormatOpenAI.String())

	// The web route reports no token counts, so usage is estimated from the
	// accumulated text and published once the stream ends.
	var streamUsage helps.StreamUsageBuffer
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		// Closing the channel on every path is what lets the stream forwarder
		// finish; a goroutine that returns without closing it hangs the client
		// until it disconnects.
		defer close(out)
		defer streamUsage.Publish(ctx, reporter)

		// The role chunk goes out before any content so a client that keys off
		// the opening role does not have to infer it from the first delta.
		if err := sendChunk(ctx, out, e.streamChunk(freebuffCompletionID(&chatCompletion{}), req.Model, "", "", false)); err != nil {
			return
		}

		_, chatErr := e.executeChat(ctx, client, translated, req, freebuffThreadID(ctx, opts), func(content, reasoning string) {
			chunk := e.streamChunk("", req.Model, content, reasoning, false)
			if chunk.Err != nil {
				log.WithError(chunk.Err).Debug("freebuff: encoding a stream chunk")
				return
			}
			// A failed send here means the caller hung up; the surrounding
			// executeChat scan returns on the context, so nothing is lost.
			_ = sendChunk(ctx, out, chunk)
		})
		if chatErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, chatErr)
			reporter.PublishFailure(ctx, chatErr)
			_ = sendChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: chatErr})
			return
		}

		// A terminating chunk is owed to the client on the success path too:
		// without a finish_reason the forwarder has no signal to stop on.
		final := e.streamChunk("", req.Model, "", "", true)
		if final.Err != nil {
			_ = sendChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: final.Err})
			return
		}
		_ = sendChunk(ctx, out, final)
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

// sendChunk delivers one chunk unless the caller has gone away.
//
// Every write to a stream channel goes through here so that no path can block
// forever on a reader that has disconnected, which is the failure mode that
// leaks a goroutine and hangs the client.
func sendChunk(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, chunk cliproxyexecutor.StreamChunk) error {
	select {
	case out <- chunk:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CountTokens uses the upstream token-count endpoint.
//
// This is a real upstream count rather than a local tokenizer estimate, which
// is what most providers in this repository are limited to. A missing or
// non-positive count is an error rather than a zero because callers use the
// number for quota decisions, where "unknown" and "none" must not look alike.
func (e *FreebuffExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	client, err := e.resolveClient(auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	translated, _, err := e.prepare(ctx, req, opts, thinking.ParseSuffix(req.Model).ModelName, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	messages, system, tools, err := freebuffSplitChatPayload(translated)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	count, err := client.TokenCount(ctx, freebuff.TokenCountRequest{
		Messages: messages,
		System:   system,
		Model:    thinking.ParseSuffix(req.Model).ModelName,
		Tools:    tools,
	})
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: helps.BuildOpenAIUsageJSON(count)}, nil
}

// freebuffSplitChatPayload pulls the fields the token-count endpoint accepts out
// of a translated OpenAI chat body. Fields the endpoint does not take are
// dropped rather than guessed at.
func freebuffSplitChatPayload(payload []byte) (messages []freebuff.TokenCountMessage, system json.RawMessage, tools []any, err error) {
	var body struct {
		Messages []freebuff.TokenCountMessage `json:"messages"`
		System   json.RawMessage              `json:"system"`
		Tools    []any                        `json:"tools"`
	}
	if err = json.Unmarshal(payload, &body); err != nil {
		return nil, nil, nil, err
	}
	if len(body.System) > 0 {
		system = body.System
	}
	return body.Messages, system, body.Tools, nil
}

// Refresh is a no-op: a Freebuff API key does not expire on a schedule this
// client can predict, so there is nothing to refresh ahead of time.
func (e *FreebuffExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

// HttpRequest is not supported: the provider has no generic passthrough surface.
func (e *FreebuffExecutor) HttpRequest(_ context.Context, _ *cliproxyauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, freebuffStatusError{code: http.StatusNotImplemented, msg: "freebuff: HttpRequest is not supported"}
}

// compile-time interface check for auth.ProviderExecutor.
var _ interface {
	Identifier() string
	RequestToFormat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format
	Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
	HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error)
} = (*FreebuffExecutor)(nil)

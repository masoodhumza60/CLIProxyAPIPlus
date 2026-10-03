package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Protocol paths and headers, pinned in protocol_notes.md against the upstream
// source. They are constants rather than configuration: the shape of the
// protocol is not something an operator chooses.
const (
	pathSessionAdmission = "/api/v1/freebuff/session/admission"
	pathSession          = "/api/v1/freebuff/session"
	pathSessionReuse     = "/api/v1/freebuff/session/reuse"

	headerInstance       = "x-freebuff-instance-id"
	headerCompactSession = "x-freebuff-compact-session"

	// headerCompactSessionValue is the only value the protocol uses; sending the
	// header at all is what opts into a compact poll.
	headerCompactSessionValue = "1"
)

// errCodeAdmissionUnsupported is reported when the server has no admission
// endpoint, which means this client cannot participate in session admission at
// all. It is worth distinguishing from an ordinary failure because retrying
// cannot help.
const errCodeAdmissionUnsupported = "session_admission_unsupported"

// SessionStatus is the protocol's session state, sent as a bare JSON string in
// the "status" field of every session response.
//
// The wire spellings are load-bearing: the server sends the string, so a
// mismatch here means a gate is never recognised and the caller falls through
// to a confusing generic error.
type SessionStatus string

// The documented status members. "superseded" is a distinct member from
// "session_superseded", which appears as a gate *code* rather than a status.
const (
	StatusActive                SessionStatus = "active"
	StatusEnded                 SessionStatus = "ended"
	StatusNone                  SessionStatus = "none"
	StatusCountryBlocked        SessionStatus = "country_blocked"
	StatusBanned                SessionStatus = "banned"
	StatusModelLocked           SessionStatus = "model_locked"
	StatusModelUnavailable      SessionStatus = "model_unavailable"
	StatusPremiumSlotTaken      SessionStatus = "premium_slot_taken"
	StatusPurchaseClaimReleased SessionStatus = "purchase_claim_released"
	StatusPurchaseInUse         SessionStatus = "purchase_in_use"
	StatusPurchaseCapacity      SessionStatus = "purchase_capacity"
	StatusFirstTabDiscount      SessionStatus = "first_tab_discount_changed"
	StatusConsentRequired       SessionStatus = "consent_required"
	StatusRateLimited           SessionStatus = "rate_limited"
	StatusSpendLimited          SessionStatus = "spend_limited"
	StatusIPCapped              SessionStatus = "ip_capped"
	StatusSuperseded            SessionStatus = "superseded"
)

// gateCodes is the vocabulary of decisions the server may return instead of a
// successful response, on a 403, 409, or 429.
//
// It is keyed on the wire string rather than on SessionStatus because the
// vocabulary is the union of two sets: the session status members, and the
// protocol's gate codes. Some gate codes have no status member at all -- a
// superseded session is reported as the gate code "session_superseded", not as
// the status "superseded" -- so a map[SessionStatus]bool would silently drop
// them and turn a decision the caller must act on into a generic failure.
var gateCodes = map[string]bool{
	// Status members.
	string(StatusCountryBlocked):   true,
	string(StatusBanned):           true,
	string(StatusModelLocked):      true,
	string(StatusModelUnavailable): true,
	string(StatusPremiumSlotTaken): true,
	string(StatusSuperseded):       true,
	string(StatusIPCapped):         true,
	string(StatusRateLimited):      true,
	string(StatusSpendLimited):     true,
	string(StatusConsentRequired):  true,

	// Status members describing the state of a paid purchase rather than of the
	// session itself, which is why they only ever arrive on a 409.
	string(StatusPurchaseClaimReleased): true,
	string(StatusPurchaseInUse):         true,
	string(StatusPurchaseCapacity):      true,
	string(StatusFirstTabDiscount):      true,

	// Gate codes that have no status member. These explain why a session could
	// not be admitted, or why an existing one ended.
	"waiting_room_required":  true,
	"waiting_room_queued":    true,
	"session_expired":        true,
	"session_superseded":     true,
	"session_model_mismatch": true,
	"session_limit_reached":  true,
}

// SessionState is the decoded session response.
//
// The loosely-typed fields are kept as json.RawMessage on purpose. Their shapes
// are owned by the server and change independently of this client, so decoding
// them into Go types here would guarantee a decode failure the first time the
// server adds a field. Callers that care about a value decode it themselves;
// this package never interprets them.
type SessionState struct {
	Status            SessionStatus              `json:"status"`
	InstanceID        string                     `json:"instanceId,omitempty"`
	Model             string                     `json:"model,omitempty"`
	RateLimit         json.RawMessage            `json:"rateLimit,omitempty"`
	RateLimitsByModel map[string]json.RawMessage `json:"rateLimitsByModel,omitempty"`
	Subscription      json.RawMessage            `json:"subscription,omitempty"`
	Freebucks         json.RawMessage            `json:"freebucks,omitempty"`
	ErrorCode         string                     `json:"error,omitempty"`
}

// AdmissionRequest asks the server for a free session.
type AdmissionRequest struct {
	// Model is the model the session must be admitted for. The server rejects a
	// mismatch with session_model_mismatch, so this is not advisory.
	Model string
}

// SessionAdmission drives the session lifecycle: admit, poll, release.
type SessionAdmission struct {
	client *Client
}

// NewSessionAdmission returns a lifecycle driver bound to a client.
func NewSessionAdmission(client *Client) *SessionAdmission {
	return &SessionAdmission{client: client}
}

// Admit requests a session and returns the server's decision.
//
// The protocol deliberately mixes "value" responses with error responses: many
// statuses arrive on a 403, 409, or 429 and are still decisions the caller has
// to act on. Those are returned as a SessionState, not as an error. Only a
// genuinely unrecognised response becomes a *SessionError.
func (s *SessionAdmission) Admit(ctx context.Context, req AdmissionRequest) (*SessionState, error) {
	body, err := json.Marshal(map[string]string{"model": req.Model})
	if err != nil {
		return nil, fmt.Errorf("freebuff: encoding admission request: %w", err)
	}

	payload, err := s.client.doJSON(ctx, http.MethodPost, pathSessionAdmission, body)
	if err != nil {
		return s.classifyAdmitError(ctx, err)
	}
	return decodeSessionState(payload)
}

// classifyAdmitError turns a failed admission into either a gate state or a
// genuine error, following the protocol's per-status handling.
func (s *SessionAdmission) classifyAdmitError(_ context.Context, err error) (*SessionState, error) {
	var se *SessionError
	if !asSessionError(err, &se) {
		return nil, err
	}

	switch se.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		// The 404-means-none rule belongs to GET. On the admission POST a 404 or
		// 405 can only mean the server has no admission endpoint, so there is
		// nothing for this client to fall back to.
		return nil, se.withCode(errCodeAdmissionUnsupported)

	case http.StatusForbidden:
		// A country block or ban is a decision, not a malfunction.
		if state := gateStateFromError(se); state != nil {
			return state, nil
		}
		return nil, err

	case http.StatusConflict:
		if state := gateStateFromError(se); state != nil {
			return state, nil
		}
		return nil, err

	case http.StatusTooManyRequests:
		if state := gateStateFromError(se); state != nil {
			return state, nil
		}
		return nil, err
	}

	return nil, err
}

// gateStateFromError recovers a session state from a gate response. A
// 403/409/429 whose code is not in the gate vocabulary is an ordinary provider
// error and returns nil.
func gateStateFromError(se *SessionError) *SessionState {
	if !gateCodes[se.ErrorCode] {
		return nil
	}
	return &SessionState{Status: SessionStatus(se.ErrorCode), ErrorCode: se.ErrorCode}
}

// Reuse resumes a session that was admitted earlier and is still held.
//
// This is the difference between one admission per turn and one admission per
// account. The protocol meters sessions: the gate vocabulary includes
// premium_slot_taken, purchase_in_use and purchase_capacity, so spending a fresh
// session on every request is not merely wasteful, it eventually refuses. A
// client that holds a live session should resume it.
//
// The response is a decision, not a ticket: a refusal is reported through the
// same gate statuses Admit uses, so a caller that cannot reuse simply admits a
// new session instead of treating it as a failure.
func (s *SessionAdmission) Reuse(ctx context.Context, instanceID, model string) (*SessionState, error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, errors.New("freebuff: reusing a session requires an instance id")
	}

	body, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return nil, fmt.Errorf("freebuff: encoding reuse request: %w", err)
	}

	// Reuse is a POST, so it follows the admission rule rather than the GET rule:
	// a 404 here means the endpoint is absent, not that the session is gone.
	reuseCtx, cancel := context.WithTimeout(ctx, sessionFetchTimeout)
	defer cancel()

	payload, err := s.client.doJSONWithHeaders(reuseCtx, http.MethodPost, pathSessionReuse, body, map[string]string{
		headerInstance: instanceID,
	})
	if err != nil {
		var se *SessionError
		if asSessionError(err, &se) {
			switch se.Status {
			case http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests:
				if state := gateStateFromError(se); state != nil {
					return state, nil
				}
			case http.StatusNotFound, http.StatusMethodNotAllowed:
				return nil, se.withCode(errCodeAdmissionUnsupported)
			}
		}
		return nil, err
	}
	return decodeSessionState(payload)
}

// Poll reads the state of an existing session. When compact is true the server
// may return a reduced payload, which MergeCompactSession knows how to fold into
// a previously known state.
func (s *SessionAdmission) Poll(ctx context.Context, instanceID string, compact bool) (*SessionState, error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, errors.New("freebuff: polling a session requires an instance id")
	}

	headers := map[string]string{headerInstance: instanceID}
	if compact {
		headers[headerCompactSession] = headerCompactSessionValue
	}

	pollCtx, cancel := context.WithTimeout(ctx, sessionFetchTimeout)
	defer cancel()

	payload, err := s.client.doJSONWithHeaders(pollCtx, http.MethodGet, pathSession, nil, headers)
	if err != nil {
		var se *SessionError
		// A GET 404 means there is no such session, which is a value.
		if asSessionError(err, &se) && se.Status == http.StatusNotFound {
			return &SessionState{Status: StatusNone, ErrorCode: se.ErrorCode}, nil
		}
		return nil, err
	}
	return decodeSessionState(payload)
}

// Release ends a session. The server answers 204 with no body.
func (s *SessionAdmission) Release(ctx context.Context, instanceID string) error {
	if strings.TrimSpace(instanceID) == "" {
		return errors.New("freebuff: releasing a session requires an instance id")
	}
	releaseCtx, cancel := context.WithTimeout(ctx, sessionFetchTimeout)
	defer cancel()
	_, err := s.client.doJSONWithHeaders(releaseCtx, http.MethodDelete, pathSession, nil, map[string]string{
		headerInstance: instanceID,
	})
	return err
}

// decodeSessionState parses a session response, requiring the status field to be
// present so a malformed body is caught here rather than read as "no session".
func decodeSessionState(payload []byte) (*SessionState, error) {
	if len(payload) == 0 {
		return nil, errors.New("freebuff: session response had no body")
	}
	var state SessionState
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, fmt.Errorf("freebuff: decoding session response: %w", err)
	}
	if state.Status == "" {
		return nil, errors.New("freebuff: session response is missing a status")
	}
	return &state, nil
}

// Disposition is what to do about a failed session request.
type Disposition int

const (
	// DispositionRetry means the request can safely be repeated.
	DispositionRetry Disposition = iota
	// DispositionStop means the failure is definitive; repeating cannot help.
	DispositionStop
	// DispositionUnknown means the outcome cannot be determined, so repeating
	// could duplicate a mutation.
	DispositionUnknown
)

// String makes dispositions readable in logs and test failures.
func (d Disposition) String() string {
	switch d {
	case DispositionRetry:
		return "retry"
	case DispositionStop:
		return "stop"
	default:
		return "unknown"
	}
}

// postRetriableStatuses are the only statuses a failed POST may be repeated on.
//
// This set is deliberately narrow. An admission POST rotates the active
// instance, so a response that never arrived may still have committed: repeating
// it could rotate twice. 408, 429, and 503 are the statuses the server
// produces *before* committing the mutation, so they are the only ones where a
// repeat is provably safe. Everything else is either definitive (stop) or
// genuinely unknown (unknown).
var postRetriableStatuses = map[int]bool{
	http.StatusRequestTimeout:     true,
	http.StatusTooManyRequests:    true,
	http.StatusServiceUnavailable: true,
}

// getRetriableStatuses is broader because a GET does not mutate anything, so any
// transient failure can safely be repeated.
var getRetriableStatuses = map[int]bool{
	http.StatusRequestTimeout:      true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

// ClassifyFailure decides whether a failed session request may be repeated.
//
// The rule is asymmetric by method, and the asymmetry is the whole point: see
// postRetriableStatuses. A transport error is treated as retryable for both
// methods, because a request that never reached the server cannot have
// committed anything.
func ClassifyFailure(method string, err error) Disposition {
	if err == nil {
		return DispositionStop
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return DispositionStop
	}

	var se *SessionError
	if !asSessionError(err, &se) {
		return DispositionRetry
	}

	retriable := getRetriableStatuses[se.Status]
	if strings.EqualFold(method, http.MethodPost) {
		retriable = postRetriableStatuses[se.Status]
	}
	if retriable {
		return DispositionRetry
	}
	if se.Status >= 400 && se.Status < 500 {
		return DispositionStop
	}
	return DispositionUnknown
}

// MergeCompactSession folds a compact poll into a previously known state.
//
// It returns nil unless both sides are active and describe the same instance and
// model, because merging across a change of instance or model would attribute
// one session's numbers to another. The counters are carried forward from the
// previous state: a compact poll omits them, and losing them would blank the
// client's view of the remaining allowance.
func MergeCompactSession(current, next *SessionState) *SessionState {
	if current == nil || next == nil {
		return nil
	}
	if current.Status != StatusActive || next.Status != StatusActive {
		return nil
	}
	if current.InstanceID == "" || current.InstanceID != next.InstanceID {
		return nil
	}
	if current.Model != "" && next.Model != "" && current.Model != next.Model {
		return nil
	}

	merged := *next
	merged.Status = StatusActive
	merged.InstanceID = current.InstanceID
	if merged.Model == "" {
		merged.Model = current.Model
	}
	if len(merged.RateLimit) == 0 {
		merged.RateLimit = current.RateLimit
	}
	if len(merged.RateLimitsByModel) == 0 {
		merged.RateLimitsByModel = copyRawMessageMap(current.RateLimitsByModel)
	}
	if len(merged.Subscription) == 0 {
		merged.Subscription = current.Subscription
	}
	if len(merged.Freebucks) == 0 {
		merged.Freebucks = current.Freebucks
	}
	return &merged
}

// copyRawMessageMap deep-copies a decoded map so a merge result cannot alias the
// state it was built from.
func copyRawMessageMap(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

// HoldsLiveFreebuffSlot reports whether a session is still occupying a slot.
//
// A session that has ended but still carries an instance id is still counted as
// live by the server, so callers that want to avoid competing for a free slot
// must treat it as held until it is released.
func HoldsLiveFreebuffSlot(state *SessionState) bool {
	if state == nil {
		return false
	}
	if state.Status == StatusActive {
		return true
	}
	return state.Status == StatusEnded && state.InstanceID != ""
}

// withCode returns a copy of the error carrying a specific protocol error code.
// It always overrides: the codes this client assigns describe what it concluded
// from the status line, which is more specific than whatever generic string the
// server echoed in the body.
func (e *SessionError) withCode(code string) *SessionError {
	clone := *e
	clone.ErrorCode = code
	return &clone
}

// sessionFetchTimeout bounds a single session poll. A session read is a bounded
// metadata fetch, not a model call, so a deadline is appropriate here.
const sessionFetchTimeout = 20 * time.Second

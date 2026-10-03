package freebuff

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A gate is the service refusing access. Reported as a plain error it arrives
// with no status, gets classified as an upstream fault and surfaces as a 502,
// which hides both the gate's name and the fact that no amount of signing in
// again would change the outcome.
func TestSessionGateErrorIsAForbiddenSessionError(t *testing.T) {
	err := sessionGateError(&SessionState{Status: StatusCountryBlocked})

	var se *SessionError
	if !errors.As(err, &se) {
		t.Fatalf("a gate must arrive as a *SessionError, got %T", err)
	}
	if se.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", se.Status)
	}
	if se.Method != http.MethodPost || se.Path != pathSessionAdmission {
		t.Fatalf("the error does not say which call was refused: %s %s", se.Method, se.Path)
	}
	if !strings.Contains(se.Message, string(StatusCountryBlocked)) {
		t.Fatalf("the gate name was lost: %q", se.Message)
	}
}

func TestSessionGateErrorNamesTheErrorCodeWhenItAddsSomething(t *testing.T) {
	err := sessionGateError(&SessionState{
		Status:    StatusSpendLimited,
		ErrorCode: "spend_limited",
	})
	if !strings.Contains(err.Error(), "spend_limited") {
		t.Fatalf("the gate code should survive into the message: %q", err.Error())
	}
}

// A refusal that names nothing must still arrive as a refusal.
func TestSessionGateErrorWithoutAReasonStillRefuses(t *testing.T) {
	for name, state := range map[string]*SessionState{
		"nil":          nil,
		"empty status": {Status: ""},
	} {
		var se *SessionError
		if !errors.As(sessionGateError(state), &se) {
			t.Fatalf("%s: a refusal must arrive as a *SessionError", name)
		}
		if se.Status != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", name, se.Status)
		}
		if se.Message == "" {
			t.Fatalf("%s: an unexplained refusal still has to say something", name)
		}
	}
}

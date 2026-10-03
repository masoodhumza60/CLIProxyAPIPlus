package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/freebuff"
)

// A 403 from Freebuff is an access decision, not a dead credential. Relabelling it
// as an expiry sent an operator round a sign-in loop that could not help, and
// marking it credential-scoped took a usable account out of rotation.
func TestClassifyChatErrorForbiddenKeepsTheServiceExplanation(t *testing.T) {
	err := (&FreebuffExecutor{}).classifyChatError(&freebuff.SessionError{
		Status:  403,
		Message: "Your account has been suspended.",
	})

	var status freebuffStatusError
	if !errors.As(err, &status) {
		t.Fatalf("expected a freebuffStatusError, got %T", err)
	}
	if got := status.StatusCode(); got != 403 {
		t.Fatalf("status = %d, want 403: a refusal is not a redirect", got)
	}
	if !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("the service's own explanation was discarded: %q", err.Error())
	}
	if strings.Contains(err.Error(), "login") {
		t.Fatalf("the message still tells the operator to sign in again: %q", err.Error())
	}
	if status.IsCredentialScoped() {
		t.Fatal("a refused session must not retire the credential; re-authentication cannot change an access decision")
	}
}

// The 401 path is genuinely an expiry and must keep saying so.
func TestClassifyChatErrorUnauthorizedRemainsAnExpiry(t *testing.T) {
	err := (&FreebuffExecutor{}).classifyChatError(&freebuff.SessionError{
		Status:  401,
		Message: "unauthorized",
	})

	var status freebuffStatusError
	if !errors.As(err, &status) {
		t.Fatalf("expected a freebuffStatusError, got %T", err)
	}
	if got := status.StatusCode(); got != 401 {
		t.Fatalf("status = %d, want 401", got)
	}
	if !status.IsCredentialScoped() {
		t.Fatal("an expired session is credential-scoped; the account should stop receiving traffic")
	}
	if !strings.Contains(err.Error(), "freebuff login") {
		t.Fatalf("the expiry message lost its instruction: %q", err.Error())
	}
}

// A refusal that names no reason still has to reach the operator as a refusal
// rather than as a transport fault.
func TestClassifyChatErrorForbiddenWithoutAnExplanation(t *testing.T) {
	err := (&FreebuffExecutor{}).classifyChatError(&freebuff.SessionError{Status: 403})

	if got := err.(freebuffStatusError).StatusCode(); got != 403 {
		t.Fatalf("status = %d, want 403", got)
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("an unexplained refusal must still read as one: %q", err.Error())
	}
}

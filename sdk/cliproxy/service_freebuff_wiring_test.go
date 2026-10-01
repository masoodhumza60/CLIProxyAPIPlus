package cliproxy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// statusCoder is the slice of the executor error contract these tests assert on.
type statusCoder interface {
	StatusCode() int
}

// TestFreebuffConfigExposesABaseURL pins the configuration surface: the
// provider is reachable over HTTP, so an operator needs somewhere to point it.
//
// The important half is not that the field exists but that it has no default.
// A silently assumed origin turns a typo or an omitted setting into what looks
// like an upstream outage, so the executor must refuse rather than guess. That
// refusal is asserted by the two tests below.
func TestFreebuffConfigExposesABaseURL(t *testing.T) {
	field, ok := reflect.TypeOf(config.Config{}).FieldByName("Freebuff")
	if !ok {
		t.Fatal("config.Config has no Freebuff field; the provider needs a base URL to be reachable")
	}
	if got := field.Tag.Get("yaml"); got != "freebuff" {
		t.Errorf("Freebuff config yaml tag = %q, want %q", got, "freebuff")
	}

	// The zero value must be empty: no default origin, so a request without an
	// explicit base URL fails loudly instead of dialling a guessed host.
	if got := (&config.Config{}).Freebuff.BaseURL; got != "" {
		t.Errorf("Freebuff.BaseURL default = %q, want empty so the executor never guesses a host", got)
	}
}

// TestFreebuffExecutorRefusesToDialWithoutABaseURL proves the executor does not
// guess, default, or fall back to a host. A credential with a token but no
// endpoint anywhere is a configuration mistake, and it is reported as one.
func TestFreebuffExecutorRefusesToDialWithoutABaseURL(t *testing.T) {
	exec := executor.NewFreebuffExecutor(&config.Config{})

	if got := exec.Identifier(); got != "freebuff" {
		t.Fatalf("Identifier() = %q, want %q", got, "freebuff")
	}

	auth := &cliproxyauth.Auth{
		ID:         "freebuff-test",
		Provider:   "freebuff",
		Attributes: map[string]string{"api_key": "some-key"},
	}

	resp, err := exec.Execute(
		context.Background(),
		auth,
		cliproxyexecutor.Request{Model: "deepseek/deepseek-v4-pro", Payload: []byte(`{"model":"deepseek/deepseek-v4-pro","messages":[]}`)},
		cliproxyexecutor.Options{},
	)
	if err == nil {
		t.Fatal("expected an error when no base URL is configured, got a successful response")
	}
	if len(resp.Payload) != 0 {
		t.Errorf("expected no payload on refusal, got %q", resp.Payload)
	}

	var coded statusCoder
	if !errors.As(err, &coded) {
		t.Fatalf("expected a status-carrying error, got %T: %v", err, err)
	}
	if got := coded.StatusCode(); got != 501 {
		t.Errorf("status = %d, want 501", got)
	}
	if !strings.Contains(err.Error(), "base URL") {
		t.Errorf("error %q should explain that no base URL is configured", err.Error())
	}
}

// TestFreebuffCountTokensAlsoRefusesWithoutABaseURL keeps the refusal
// consistent across the whole interface. A provider that served token counts
// without an endpoint would be a confusing half-configuration.
func TestFreebuffCountTokensAlsoRefusesWithoutABaseURL(t *testing.T) {
	exec := executor.NewFreebuffExecutor(&config.Config{})

	_, err := exec.CountTokens(
		context.Background(),
		&cliproxyauth.Auth{Provider: "freebuff", Attributes: map[string]string{"api_key": "some-key"}},
		cliproxyexecutor.Request{Model: "deepseek/deepseek-v4-pro", Payload: []byte(`{"messages":[]}`)},
		cliproxyexecutor.Options{},
	)
	if err == nil {
		t.Fatal("expected CountTokens to refuse without a configured base URL")
	}

	var coded statusCoder
	if !errors.As(err, &coded) {
		t.Fatalf("expected a status-carrying error, got %T: %v", err, err)
	}
	if got := coded.StatusCode(); got != 501 {
		t.Errorf("status = %d, want 501", got)
	}
}

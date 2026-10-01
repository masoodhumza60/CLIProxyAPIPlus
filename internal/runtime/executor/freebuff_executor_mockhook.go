//go:build freebuffmock

package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/freebuff"
)

// NewFreebuffExecutorForTest builds a Freebuff executor wired to a fixed base URL.
//
// It exists only under the freebuffmock build tag, so the symbol does not exist
// in a production binary at all. That is deliberate: the production constructor
// leaves the client factory nil so no operator can point this provider at a host
// of their choosing, and an untagged export would quietly reopen that door. An
// integration test lives in a different package and therefore cannot reach the
// unexported factory, so this is the only way it can supply one.
func NewFreebuffExecutorForTest(cfg *config.Config, baseURL string) *FreebuffExecutor {
	return newFreebuffExecutorWithClientFactory(cfg, func(gotBaseURL, apiKey string) (*freebuff.Client, error) {
		return freebuff.NewClient(gotBaseURL, apiKey, freebuff.Options{})
	})
}

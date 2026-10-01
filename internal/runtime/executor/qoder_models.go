package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

// qoderPathEnv lets operators point at a specific Qoder CLI binary instead of
// relying on PATH lookup.
const qoderPathEnv = "QODER_PATH"

// qoderModelCacheTTL bounds how long a discovered model list is reused before
// the CLI is queried again.
const qoderModelCacheTTL = 30 * time.Minute

// qoderBinaryNames maps a normalized backend to the CLI binary that serves it.
// The CN backend talks to qoder.com.cn; the global backend talks to qoder.com.
var qoderBinaryNames = map[string]string{
	registry.QoderBackendCN:     "qoderclicn",
	registry.QoderBackendGlobal: "qodercli",
}

// QoderBinaryForBackend returns the CLI binary name for a backend, honouring the
// QODER_PATH override.
func QoderBinaryForBackend(backend string) string {
	if override := strings.TrimSpace(os.Getenv(qoderPathEnv)); override != "" {
		return override
	}
	normalized := registry.NormalizeQoderBackend(backend)
	if binary, ok := qoderBinaryNames[normalized]; ok {
		return binary
	}
	// Unknown backends are rejected at config load; fall back to the CN binary
	// so a misconfigured value cannot silently reach the global endpoint.
	return qoderBinaryNames[registry.QoderBackendCN]
}

// qoderModelCacheEntry memoizes a discovered model list per backend.
type qoderModelCacheEntry struct {
	models    []*registry.ModelInfo
	fetchedAt time.Time
}

var (
	qoderModelCacheMu sync.Mutex
	qoderModelCache   = map[string]qoderModelCacheEntry{}

	// qoderDiscoveryMu serializes the CLI invocation itself, so only one
	// discovery shells out at a time. The cache lock cannot cover the subprocess
	// call without also blocking cache readers, and leaving it out lets two
	// callers miss the cache and race. The CLI does not reliably answer two
	// simultaneous invocations: the loser exits non-zero with no output and
	// returns an empty list, which would wipe out the winner's models. Callers
	// re-check the cache after acquiring this, so a waiter gets the winner's
	// result instead of running the CLI a second time.
	qoderDiscoveryMu sync.Mutex
)

// ParseQoderModelList parses the output of `<qoder-cli> --list-models`.
// The CLI prints a "MODEL" header line followed by one model name per line.
func ParseQoderModelList(output string) []string {
	var models []string
	seen := make(map[string]struct{})
	inHeader := true
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if inHeader {
			// Skip the header (and any banner printed before it).
			if strings.EqualFold(line, "MODEL") || strings.EqualFold(line, "MODELS") {
				inHeader = false
			}
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		models = append(models, line)
	}
	return models
}

// qoderModelInfos converts discovered model names into registry model metadata.
// The CLI accepts its own model names verbatim on `--model`, so the discovered
// name is used as the model ID to guarantee it round-trips.
func qoderModelInfos(names []string) []*registry.ModelInfo {
	infos := make([]*registry.ModelInfo, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		infos = append(infos, &registry.ModelInfo{
			ID:          name,
			Object:      "model",
			OwnedBy:     "qoder",
			Type:        "qoder",
			DisplayName: name,
			Name:        name,
		})
	}
	return infos
}

// listQoderModels runs `<binary> --list-models` and parses the result.
// This is credential acquisition, so a timeout is allowed here.
func listQoderModels(ctx context.Context, backend string) ([]*registry.ModelInfo, error) {
	binary := QoderBinaryForBackend(backend)
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("qoder CLI %q not found in PATH (set %s to override): %w", binary, qoderPathEnv, err)
	}

	listCtx, cancel := context.WithTimeout(ctx, qoderModelListTimeout)
	defer cancel()

	// stderr is captured rather than discarded: when the CLI refuses, the
	// reason is only ever on stderr, and a bare "exit status 1" is useless.
	var stderr bytes.Buffer
	cmd := exec.CommandContext(listCtx, resolved, "--list-models")
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("qoder CLI %q --list-models failed: %w (stderr: %s)",
			binary, err, qoderTruncateForError(stderr.String()))
	}

	names := ParseQoderModelList(string(output))
	if len(names) == 0 {
		return nil, fmt.Errorf("qoder CLI %q --list-models returned no models", binary)
	}
	return qoderModelInfos(names), nil
}

// qoderModelListTimeout bounds model discovery, which is credential acquisition.
//
// The CLI needs roughly ten seconds to answer on its own, and the server is
// doing concurrent network work at startup, so a tighter deadline kills the
// child mid-call. A killed child on Windows surfaces as a bare "exit status 1"
// with empty stderr, which reads like a CLI failure rather than a timeout.
const qoderModelListTimeout = 90 * time.Second

// qoderMaxErrorLog bounds how much of a failing CLI's stderr is carried into an
// error message, so a chatty CLI cannot flood the log.
const qoderMaxErrorLog = 512

// qoderTruncateForError bounds a captured stderr string for use in an error.
func qoderTruncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= qoderMaxErrorLog {
		return s
	}
	return s[:qoderMaxErrorLog] + "... (truncated)"
}

// DiscoverQoderModels returns the models available on a backend, querying the
// CLI at most once per cache TTL. Results are cached per backend so model
// registration does not shell out on every config reload.
func DiscoverQoderModels(ctx context.Context, backend string) []*registry.ModelInfo {
	normalized := registry.NormalizeQoderBackend(backend)

	qoderModelCacheMu.Lock()
	entry, ok := qoderModelCache[normalized]
	if ok && time.Since(entry.fetchedAt) < qoderModelCacheTTL {
		qoderModelCacheMu.Unlock()
		return cloneQoderModels(entry.models)
	}
	qoderModelCacheMu.Unlock()

	qoderDiscoveryMu.Lock()
	defer qoderDiscoveryMu.Unlock()

	// Re-read the cache now that the discovery lock is held: a caller that was
	// waiting here may have populated it while this one was reading above.
	qoderModelCacheMu.Lock()
	entry, ok = qoderModelCache[normalized]
	if ok && time.Since(entry.fetchedAt) < qoderModelCacheTTL {
		qoderModelCacheMu.Unlock()
		return cloneQoderModels(entry.models)
	}
	qoderModelCacheMu.Unlock()

	models, err := listQoderModels(ctx, normalized)
	if err != nil {
		log.WithError(err).WithField("backend", normalized).Warn("qoder: model discovery failed")
		// Serve the last known good list when discovery fails so a transient CLI
		// problem does not make every Qoder model disappear.
		if ok && len(entry.models) > 0 {
			return cloneQoderModels(entry.models)
		}
		return nil
	}

	// Region-scoped guard: a global key must never advertise a China-only model.
	filtered := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if !registry.QoderModelSupportsBackend(model.ID, normalized) {
			continue
		}
		filtered = append(filtered, model)
	}

	qoderModelCacheMu.Lock()
	qoderModelCache[normalized] = qoderModelCacheEntry{models: filtered, fetchedAt: time.Now()}
	qoderModelCacheMu.Unlock()

	log.WithField("backend", normalized).WithField("count", len(filtered)).Info("qoder: discovered models")
	return cloneQoderModels(filtered)
}

// cloneQoderModels copies the slice header contents so callers cannot mutate the cache.
func cloneQoderModels(models []*registry.ModelInfo) []*registry.ModelInfo {
	if models == nil {
		return nil
	}
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		copied := *model
		out = append(out, &copied)
	}
	return out
}

// ResetQoderModelCache clears the discovery cache. Used by tests.
func ResetQoderModelCache() {
	qoderModelCacheMu.Lock()
	qoderModelCache = map[string]qoderModelCacheEntry{}
	qoderModelCacheMu.Unlock()
}

// seedQoderModelCache primes the cache for a backend so tests can exercise
// discovery and the region filter without a real CLI on PATH.
func seedQoderModelCache(backend string, models []*registry.ModelInfo) {
	normalized := registry.NormalizeQoderBackend(backend)
	qoderModelCacheMu.Lock()
	qoderModelCache[normalized] = qoderModelCacheEntry{models: cloneQoderModels(models), fetchedAt: time.Now()}
	qoderModelCacheMu.Unlock()
}

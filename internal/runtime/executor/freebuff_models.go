package executor

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/freebuff"
)

// This file discovers the models a Freebuff account may actually use.
//
// The catalogue is per-account and it moves: what one account can run another
// cannot, and rows disappear as the access tier changes. A hardcoded list would
// advertise models that fail on first use, so the list is read from the service
// and cached only briefly.

const (
	// freebuffCatalogCacheTTL bounds how long a catalogue is reused. The service
	// states its own refresh deadline, which is honoured when it is present;
	// this is the ceiling used when it is missing, so a missing field cannot pin
	// a stale list in place indefinitely.
	freebuffCatalogCacheTTL = 30 * time.Minute

	// freebuffCatalogTimeout bounds the discovery fetch. This is a
	// credential-bound metadata read, which is the one case where a deadline is
	// appropriate.
	freebuffCatalogTimeout = 20 * time.Second
)

var (
	freebuffCatalogMu    sync.Mutex
	freebuffCatalogCache = map[string]*freebuff.Catalog{}
)

// ResetFreebuffCatalogCache clears the cached catalogues, so one test's
// catalogue cannot satisfy another's request.
func ResetFreebuffCatalogCache() {
	freebuffCatalogMu.Lock()
	freebuffCatalogCache = map[string]*freebuff.Catalog{}
	freebuffCatalogMu.Unlock()
}

// freebuffCatalogBaseURL returns the catalogue origin.
//
// The catalogue is served by the Codebuff API, a different origin from the web
// host that serves chat. It is never derived from the web host, because the two
// are not the same service and inferring one from the other would send catalogue
// traffic somewhere nobody chose.
func freebuffCatalogBaseURL(cfg *config.Config) string {
	if cfg != nil {
		if configured := cfg.Freebuff.CatalogBaseURL; configured != "" {
			return configured
		}
	}
	return freebuffCatalogHost
}

// DiscoverFreebuffModels returns the models the credential may use.
//
// The cache key includes the credential rather than just the endpoint, because
// two accounts on one process see different catalogues and serving one to the
// other would advertise models the caller cannot run.
func DiscoverFreebuffModels(ctx context.Context, cfg *config.Config, apiKey, webBaseURL string) []*registry.ModelInfo {
	if apiKey == "" {
		return nil
	}
	cacheKey := webBaseURL + "|" + apiKey

	freebuffCatalogMu.Lock()
	cached, held := freebuffCatalogCache[cacheKey]
	if held && time.Now().UnixMilli() < cached.ExpiresAt {
		freebuffCatalogMu.Unlock()
		return freebuffModelInfos(cached)
	}
	freebuffCatalogMu.Unlock()

	// A catalogue is kept past its refresh deadline on purpose: a failed refresh
	// should not empty the model list an operator is looking at mid-request.
	serveHeld := func() []*registry.ModelInfo {
		if held {
			return freebuffModelInfos(cached)
		}
		return nil
	}

	client, err := freebuff.NewClient(webBaseURL, apiKey, freebuff.Options{})
	if err != nil {
		log.WithError(err).Debug("freebuff: building a catalogue client failed")
		return serveHeld()
	}

	fetchCtx, cancel := context.WithTimeout(ctx, freebuffCatalogTimeout)
	defer cancel()
	catalog, err := client.FetchCatalog(fetchCtx, freebuffCatalogBaseURL(cfg))
	if err != nil {
		log.WithError(err).Warn("freebuff: the model catalogue could not be read")
		return serveHeld()
	}

	freebuffCatalogMu.Lock()
	freebuffCatalogCache[cacheKey] = catalog
	freebuffCatalogMu.Unlock()

	usable := len(catalog.UsableRows())
	log.WithField("rows", len(catalog.Rows)).WithField("usable", usable).
		WithField("tier", catalog.AccessTier).Info("freebuff: model catalogue read")
	return freebuffModelInfos(catalog)
}

// freebuffModelInfos converts catalogue rows into the registry's model shape.
//
// The display name becomes the model id because that is the name a caller passes
// back, and Resolve matches on it. The per-account handle is deliberately not
// used as an id: it is a signed value that changes, and advertising it would
// hand every caller a credential-scoped secret as a model name.
func freebuffModelInfos(catalog *freebuff.Catalog) []*registry.ModelInfo {
	if catalog == nil {
		return nil
	}
	rows := catalog.UsableRows()
	infos := make([]*registry.ModelInfo, 0, len(rows))
	for _, row := range rows {
		info := &registry.ModelInfo{
			ID:          row.DisplayName,
			Object:      "model",
			OwnedBy:     "freebuff",
			Type:        "freebuff",
			DisplayName: row.DisplayName,
			Created:     time.Now().Unix(),
		}
		if row.Tagline != "" {
			info.Description = row.Tagline
		}
		if row.ContextWindow > 0 {
			info.InputTokenLimit = int(row.ContextWindow)
		}
		infos = append(infos, info)
	}
	return infos
}

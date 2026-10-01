package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file implements the model catalogue and the shared request helpers the
// web chat surface needs.
//
// The catalogue lives on the Codebuff API host rather than the web host, and it
// is the one part of the protocol reachable with the plain dual auth headers.
// It answers without the `x-freebuff-client` marker and without the device
// signature the official CLI also sends, so this client sends neither: an
// honest identity is enough to be told what this account may use.

// CatalogPath is the model catalogue route on the Codebuff API host.
const CatalogPath = "/api/v1/freebuff/models"

// APIKeyHeader is the second auth header the Codebuff API requires. It is not
// belt-and-braces: sending only `Authorization` is rejected with 401, so both
// must be present on every call to that host.
const APIKeyHeader = "x-codebuff-api-key"

// catalogCacheTTL bounds how long a catalogue is reused. The service states a
// per-row expiry in `refreshAt`; this is the ceiling used when that value is
// missing or implausible, so a bad field cannot turn into a busy loop.
const catalogCacheTTL = 30 * time.Minute

// CatalogRow is one model the account may use.
//
// Handle is a per-account signed reference and is what identifies the model to
// the service. Key is the stable identifier for the row. DisplayName is for
// humans only. The two are distinct namespaces: threads report a compiled model
// id such as "mimo-v2.5" that is neither the key nor the handle, so a name
// chosen from one place must not be assumed valid in another.
//
// Access is "open" for rows the account may use and "locked" for rows that need
// a paid plan; LockedLabel carries the reason.
type CatalogRow struct {
	Key           string   `json:"key"`
	Handle        string   `json:"handle"`
	DisplayName   string   `json:"displayName"`
	Tagline       string   `json:"tagline"`
	Multimodal    bool     `json:"multimodal"`
	Premium       bool     `json:"premium"`
	DataUse       string   `json:"dataUse"`
	Access        string   `json:"access"`
	LockedLabel   string   `json:"lockedLabel"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort"`
	ContextWindow int64    `json:"contextWindow"`
	SortOrder     int      `json:"sortOrder"`
}

// Usable reports whether the account may actually run this row. A locked row is
// advertised by the service but refuses to run, so offering it would produce a
// model list full of entries that fail on first use.
func (r CatalogRow) Usable() bool {
	return r.Key != "" && r.Access == "open"
}

// Catalog is a snapshot of what the account may use at a point in time.
type Catalog struct {
	Rows           []CatalogRow
	AccessTier     string
	RecommendedKey string
	FallbackKey    string
	// ExpiresAt is when the service asked for a refresh, in Unix milliseconds.
	ExpiresAt int64
}

type catalogResponse struct {
	Version        string       `json:"version"`
	IssuedAt       int64        `json:"issuedAt"`
	RefreshAt      int64        `json:"refreshAt"`
	Rows           []CatalogRow `json:"rows"`
	RecommendedKey string       `json:"recommendedKey"`
	FallbackKey    string       `json:"fallbackKey"`
}

// FetchCatalog reads the model catalogue.
//
// apiBaseURL is the Codebuff API host, which is a different origin from the web
// host the chat surface uses: login and chat are on freebuff.com, the
// catalogue is on the Codebuff API. It is a parameter rather than a package
// global so a test can point it at a fixture and so no default can quietly send
// traffic somewhere an operator did not name.
func (c *Client) FetchCatalog(ctx context.Context, apiBaseURL string) (*Catalog, error) {
	if strings.TrimSpace(apiBaseURL) == "" {
		return nil, errors.New("freebuff: a catalogue base URL is required")
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, errors.New("freebuff: the catalogue requires a token; run `freebuff login` first")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiBaseURL, "/")+CatalogPath, nil)
	if err != nil {
		return nil, fmt.Errorf("freebuff: building catalogue request: %w", err)
	}
	// Both headers are required by this host; the catalogue is not reachable
	// with the session cookie the web surface uses.
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set(APIKeyHeader, c.apiKey)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("freebuff: fetching catalogue: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody*8))
	if err != nil {
		return nil, fmt.Errorf("freebuff: reading catalogue: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := summarizeChatError(resp.StatusCode, string(body))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			message = "freebuff credentials rejected; run `freebuff login` again"
		}
		return nil, &SessionError{
			Status:    resp.StatusCode,
			Method:    http.MethodGet,
			Path:      CatalogPath,
			Message:   message,
			Body:      truncate(string(body), 200),
			ErrorCode: chatErrorCode(body),
		}
	}

	var decoded catalogResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("freebuff: decoding catalogue: %w", err)
	}
	catalog := &Catalog{
		Rows:           decoded.Rows,
		AccessTier:     decoded.Version,
		RecommendedKey: decoded.RecommendedKey,
		FallbackKey:    decoded.FallbackKey,
		ExpiresAt:      decoded.RefreshAt,
	}
	if catalog.ExpiresAt <= 0 {
		// A catalogue with no stated expiry must not be treated as valid
		// forever; the ceiling is what stops a missing field pinning a stale
		// model list in place.
		catalog.ExpiresAt = decoded.IssuedAt + int64(catalogCacheTTL/time.Millisecond)
	}
	if len(catalog.Rows) == 0 {
		return nil, errors.New("freebuff: the catalogue returned no models for this account")
	}
	return catalog, nil
}

// UsableRows returns only the rows this account may actually run, preserving the
// service's ordering so a model list is stable between refreshes.
func (c *Catalog) UsableRows() []CatalogRow {
	if c == nil {
		return nil
	}
	usable := make([]CatalogRow, 0, len(c.Rows))
	for _, row := range c.Rows {
		if row.Usable() {
			usable = append(usable, row)
		}
	}
	return usable
}

// Resolve maps a requested model name onto a row.
//
// It accepts, in order, a row key, a row handle, a display name, and the
// recommended key as a fallback so an unrecognised name still resolves to
// something the account may run rather than failing the request outright. The
// chat route substitutes silently for names it does not know, so resolving here
// is what stops a caller's model choice from being quietly ignored.
func (c *Catalog) Resolve(model string) (CatalogRow, bool) {
	if c == nil || len(c.Rows) == 0 {
		return CatalogRow{}, false
	}
	wanted := strings.TrimSpace(model)
	if wanted != "" {
		for _, row := range c.Rows {
			if row.Key == wanted || row.Handle == wanted || row.DisplayName == wanted {
				return row, true
			}
		}
	}
	for _, key := range []string{c.RecommendedKey, c.FallbackKey} {
		if key == "" {
			continue
		}
		for _, row := range c.Rows {
			if row.Key == key && row.Usable() {
				return row, true
			}
		}
	}
	// Last resort: any row the account may run, so a request is never sent
	// with a name the service will substitute away from the caller's intent.
	for _, row := range c.Rows {
		if row.Usable() {
			return row, true
		}
	}
	return CatalogRow{}, false
}

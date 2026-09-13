// OpenRouter free-model catalog: fetched live from openrouter.ai's
// /v1/models endpoint via the family's configured upstream proxy
// (typically the local clash on 127.0.0.1:7897 — direct connections
// from this host get "Access denied by security policy" because
// openrouter rejects the key from our geo / IP). The slice is
// refreshed periodically so model churn is picked up without editing
// source.
//
// HandleModels() reads the latest snapshot under RLock; the refresh
// goroutine swaps the slice under Lock every OPENROUTER_CATALOG_TTL
// (and once at startup).

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openrouterCatalogURL = "https://openrouter.ai/api/v1/models"
	openrouterCatalogTTL = 10 * time.Minute
	openrouterFetchHTTP  = 20 * time.Second
)

var (
	openrouterMu   sync.RWMutex
	openrouterIDs  []string  // last successful snapshot; nil = never fetched
	openrouterLast time.Time // last successful fetch time
)

// StartOpenRouterCatalogRefresher launches a background goroutine that
// fetches the openrouter free-models list once immediately, then every
// OPENROUTER_CATALOG_TTL. Failures are logged but do not crash the
// process — a stale or empty snapshot is acceptable; the catalog will
// simply not list openrouter IDs that round until the next successful
// fetch. Call once at startup after LoadUpstreamProxy().
func StartOpenRouterCatalogRefresher(ctx context.Context) {
	go func() {
		fetchAndStore(ctx)
		t := time.NewTicker(openrouterCatalogTTL)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fetchAndStore(ctx)
			}
		}
	}()
}

// RefreshOpenRouterCatalogForTest triggers an immediate fetch and
// blocks until it completes. Test-only.
func RefreshOpenRouterCatalogForTest(ctx context.Context) error {
	return fetchOnce(ctx)
}

func fetchAndStore(ctx context.Context) {
	if err := fetchOnce(ctx); err != nil {
		log.Printf("openrouter-catalog: fetch failed: %v", err)
	}
}

func fetchOnce(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, openrouterFetchHTTP)
	defer cancel()
	ids, err := fetchOpenRouterFree(cctx)
	if err != nil {
		return err
	}
	openrouterMu.Lock()
	openrouterIDs = ids
	openrouterLast = time.Now()
	openrouterMu.Unlock()
	log.Printf("openrouter-catalog: refreshed %d free models", len(ids))
	return nil
}

func fetchOpenRouterFree(ctx context.Context) ([]string, error) {
	cl, tr := upstreamClient(int(openrouterFetchHTTP.Seconds()), proxyURLForFamily("openrouter"))
	defer tr.CloseIdleConnections()
	if cl == nil {
		return nil, errNoUpstreamClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openrouterCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &catalogHTTPError{status: resp.StatusCode, body: string(body)}
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if strings.HasSuffix(m.ID, ":free") {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// snapshotOpenRouterIDs calls fn with the latest catalog snapshot under
// RLock. fn must not retain ids past the call (the underlying slice can
// be replaced on the next refresh).
func snapshotOpenRouterIDs(fn func(ids []string)) {
	openrouterMu.RLock()
	ids := openrouterIDs
	openrouterMu.RUnlock()
	fn(ids)
}

// catalogAge returns how long ago the snapshot was last refreshed. Test-only.
func catalogAge() time.Duration {
	openrouterMu.RLock()
	defer openrouterMu.RUnlock()
	if openrouterLast.IsZero() {
		return time.Duration(-1)
	}
	return time.Since(openrouterLast)
}

type catalogHTTPError struct {
	status int
	body   string
}

func (e *catalogHTTPError) Error() string {
	return "openrouter-catalog: HTTP " + strconv.Itoa(e.status) + ": " + e.body
}

// errNoUpstreamClient — returned when no HTTP client could be built
// (proxy lookup failed). Distinct from transient fetch errors so the
// caller can decide whether to retry.
var errNoUpstreamClient = &catalogHTTPError{status: 0, body: "no upstream client (proxy lookup failed)"}

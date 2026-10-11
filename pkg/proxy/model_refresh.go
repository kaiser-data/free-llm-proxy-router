package proxy

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/scan"
)

// freeSuffix is the model-id marker the live refresh accepts (task Q):
// the deployment's upstream (UnoRouter) exposes free models ONLY under ids
// ending in ":free". The filter is strict — "exactly this suffix" — on top
// of the scanner's own free_markers/pricing detection.
const freeSuffix = ":free"

// ModelRefresher keeps the canonical :free groups in sync with the
// provider's live model list (task Q): every enabled provider that opted
// in (providers[].discovery.refresh) gets GET /v1/models called at boot
// and then every models.model_refresh_minutes.
//
// Refresh semantics:
//
//   - A failed refresh keeps the provider's existing entries untouched —
//     groups are never cleared by a refresh error (warn logged).
//   - A successful refresh merges: kept ids get refreshed metadata
//     (context window, endpoint/capability data), a new LastVerifiedAt and
//     a cleared NeedsReverification flag (NG8 reverification); ids new to
//     the list join their canonical groups with a fresh DiscoveredAt,
//     empty slot windows and the neutral default score; ids that
//     disappeared leave the routing for NEW requests immediately while
//     in-flight requests carry their own resolved upstream and finish.
//
// The refresh builds a new catalog and swaps it in via Server.UpdateCatalog
// (atomic pointer store) — lock-free for readers and race-free with the
// request path.
type ModelRefresher struct {
	srv  *Server
	disp *scan.Dispatcher
}

// NewModelRefresher creates the refresher for a server.
func NewModelRefresher(srv *Server) *ModelRefresher {
	return &ModelRefresher{srv: srv, disp: scan.NewDispatcher()}
}

// Start runs the boot refresh (once) and then the periodic loop. The
// interval re-reads the config every cycle so a hot reload of
// models.model_refresh_minutes takes effect at the next tick; a value of 0
// disables the live refresh entirely (the boot refresh too).
func (r *ModelRefresher) Start(ctx context.Context) {
	go func() {
		for {
			interval := r.interval()
			if interval <= 0 {
				return
			}
			r.refreshOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// interval returns the current refresh interval from the config; 0 = off.
func (r *ModelRefresher) interval() time.Duration {
	c := r.srv.cfg.Load()
	if c == nil || c.Models.ModelRefreshMinutes <= 0 {
		return 0
	}
	return time.Duration(c.Models.ModelRefreshMinutes) * time.Minute
}

// refreshOnce refreshes every opted-in provider and swaps the merged
// catalog in. Returns whether the model set changed.
func (r *ModelRefresher) refreshOnce(ctx context.Context) bool {
	cfg := r.srv.cfg.Load()
	old := r.srv.catalog.Load()
	if cfg == nil {
		return false
	}
	if old == nil {
		old = &catalog.Catalog{Version: 1}
	}
	out := &catalog.Catalog{
		Version:   old.Version,
		UpdatedAt: time.Now(),
		Blocklist: old.Blocklist,
	}
	refreshed := map[string]bool{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Enabled || !p.Discovery.Refresh {
			continue
		}
		refreshed[p.ID] = true
		fresh, err := r.disp.ScanProvider(ctx, *p)
		if err != nil {
			// Refresh failure NEVER clears the group: keep the existing
			// list and warn (task Q).
			log.Printf("WARN model refresh: provider %s failed (%v) — keeping its existing list", p.ID, err)
			out.Entries = append(out.Entries, old.ByProvider(p.ID)...)
			continue
		}
		merged := mergeProviderEntries(old.ByProvider(p.ID), filterStrictlyFree(fresh))
		log.Printf("model refresh: provider %s → %d free model(s) with the %q suffix", p.ID, len(merged), freeSuffix)
		out.Entries = append(out.Entries, merged...)
	}
	for _, e := range old.Entries {
		if !refreshed[e.ProviderID] {
			out.Entries = append(out.Entries, e)
		}
	}
	changed := !sameModelSet(old.Entries, out.Entries)
	r.srv.UpdateCatalog(out)
	return changed
}

// filterStrictlyFree keeps only free entries whose model id ends in
// ":free" exactly — the defensive second gate on top of the scanner's
// free_markers/pricing detection (task Q).
func filterStrictlyFree(entries []catalog.CatalogEntry) []catalog.CatalogEntry {
	out := make([]catalog.CatalogEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsFree && strings.HasSuffix(e.ModelID, freeSuffix) {
			out = append(out, e)
		}
	}
	return out
}

// mergeProviderEntries merges the fresh :free list into the provider's
// existing entries (see ModelRefresher doc for the rules).
func mergeProviderEntries(old, fresh []catalog.CatalogEntry) []catalog.CatalogEntry {
	freshByID := make(map[string]catalog.CatalogEntry, len(fresh))
	for _, f := range fresh {
		freshByID[f.ModelID] = f
	}
	oldByID := make(map[string]bool, len(old))
	for _, e := range old {
		oldByID[e.ModelID] = true
	}
	now := time.Now()
	out := make([]catalog.CatalogEntry, 0, len(fresh))
	for _, e := range old {
		f, ok := freshByID[e.ModelID]
		if !ok {
			continue // disappeared from the live list — see ModelRefresher doc
		}
		if f.ContextWindow > 0 {
			e.ContextWindow = f.ContextWindow
		}
		if len(f.Metadata) > 0 {
			e.Metadata = f.Metadata
		}
		e.LastVerifiedAt = now
		e.NeedsReverification = false // confirmed by the live list (NG8)
		out = append(out, e)
	}
	for _, f := range fresh {
		if !oldByID[f.ModelID] {
			// New model: joins the group with a fresh DiscoveredAt, empty
			// slot windows (the limiter has no state for it) and the
			// neutral default score.
			out = append(out, f)
		}
	}
	return out
}

// sameModelSet compares catalogs by "provider/model" id set.
func sameModelSet(a, b []catalog.CatalogEntry) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, e := range a {
		seen[e.ProviderID+"/"+e.ModelID] = true
	}
	for _, e := range b {
		if !seen[e.ProviderID+"/"+e.ModelID] {
			return false
		}
	}
	return true
}

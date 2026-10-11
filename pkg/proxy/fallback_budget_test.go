package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// TestHopCeilingStopsTheWalk: fallback.max_fallback_hops is a hard stop —
// once the ceiling is reached the walk aborts immediately (no further
// candidates, no ultimate-fallback step) and the caller gets the budget
// error that turns into the 503 exhaustion response.
func TestHopCeilingStopsTheWalk(t *testing.T) {
	hits := make([]*int32, 3)
	servers := make([]*httptest.Server, 3)
	providers := make([]config.ProviderConfig, 3)
	entries := make([]strategy.RankedEntry, 3)
	for i := 0; i < 3; i++ {
		var n int32
		hits[i] = &n
		id := fmt.Sprintf("provider-%c", 'a'+i)
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&n, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"always failing"}}`)
		}))
		defer servers[i].Close()
		providers[i] = config.ProviderConfig{ID: id, BaseURL: servers[i].URL, Enabled: true}
		entries[i] = strategy.RankedEntry{ProviderID: id, ModelID: "model-" + id}
	}

	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "provider-a", ModelID: "model-provider-a", IsFree: true, TierType: "free"},
		{ProviderID: "provider-b", ModelID: "model-provider-b", IsFree: true, TierType: "free"},
		{ProviderID: "provider-c", ModelID: "model-provider-c", IsFree: true, TierType: "free"},
	}}
	cfg := &config.Config{
		Providers: providers,
		Fallback: config.FallbackConfig{
			MaxAttempts:     10, // would allow all three without the hop ceiling
			MaxFallbackHops: 2,
		},
	}
	fc := &FallbackChain{
		Cfg:                cfg,
		Strategy:           &multiMockStrategy{entries: entries},
		Catalog:            cat,
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
	}

	_, err := fc.Execute(context.Background(), chainRequest())
	if err == nil {
		t.Fatal("expected the hop-ceiling error")
	}
	if !strings.Contains(err.Error(), "hop ceiling") {
		t.Errorf("error = %v, want the hop-ceiling message", err)
	}
	if got := atomic.LoadInt32(hits[0]); got != 1 {
		t.Errorf("provider-a hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(hits[1]); got != 1 {
		t.Errorf("provider-b hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(hits[2]); got != 0 {
		t.Errorf("provider-c hits = %d, want 0 (walk stopped at the ceiling)", got)
	}
}

// TestHopCeilingDoesNotBindAtDefaults: the default ceiling (8) sits above
// the default max_attempts (5) — a walk that fails over across candidates
// still succeeds unchanged.
func TestHopCeilingDoesNotBindAtDefaults(t *testing.T) {
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failSrv.Close()
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer okSrv.Close()

	fc, _ := newTwoProviderChain(failSrv.URL, okSrv.URL)
	fc.Cfg.Fallback.MaxFallbackHops = 0 // zeroed struct → effective default 8

	resp, err := fc.Execute(context.Background(), chainRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want the second provider's 200", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (default hop ceiling must not bind)", resp.StatusCode)
	}
}

// TestTotalDeadlineCutsAttemptMidWay: fallback.total_deadline_seconds rides
// the request context — an attempt that outlives the budget is aborted
// mid-flight, the same-candidate backoff refuses to sleep on a dead budget,
// and the walk stops with the budget-exhaustion error (the handler answers
// the 503 exhaustion response).
func TestTotalDeadlineCutsAttemptMidWay(t *testing.T) {
	var hits int32
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Would succeed after 3s — but the request budget (1s) must cut
		// the attempt mid-flight; the handler observes its own context so
		// the aborted attempt is noticed upstream too.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(3 * time.Second):
		}
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer slowSrv.Close()

	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "testprovider", ModelID: "test-model", IsFree: true, TierType: "free"},
	}}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "testprovider", BaseURL: slowSrv.URL, Enabled: true},
		},
		Fallback: config.FallbackConfig{
			MaxAttempts:          3,
			RetriesPerCandidate:  1,
			RetryOn429:           true,
			RetryOn5xx:           true,
			TotalDeadlineSeconds: 1, // cut the 3s attempt at 1s
		},
	}
	fc := &FallbackChain{
		Cfg:                cfg,
		Strategy:           &multiMockStrategy{entries: []strategy.RankedEntry{{ProviderID: "testprovider", ModelID: "test-model"}}},
		Catalog:            cat,
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{}, // no client timeout — the budget must do the cutting
	}

	start := time.Now()
	_, err := fc.Execute(context.Background(), chainRequest())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the budget-exhaustion error")
	}
	if !strings.Contains(err.Error(), "request budget exhausted") {
		t.Errorf("error = %v, want the request-budget message", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (the deadline aborts the attempt; no retry on a dead budget)", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Execute took %v, want well under the upstream's 3s (budget must cut the attempt)", elapsed)
	}
}

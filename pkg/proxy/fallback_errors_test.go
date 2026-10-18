package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// multiMockStrategy returns a fixed ranking of (provider, model) pairs.
type multiMockStrategy struct {
	entries []strategy.RankedEntry
}

func (m *multiMockStrategy) Name() string { return "mock" }
func (m *multiMockStrategy) Rank(_ strategy.Request, _ []catalog.CatalogEntry, _ *models.EnrichedCatalog) []strategy.RankedEntry {
	return m.entries
}

// newTwoProviderChain builds a FallbackChain ranking provider-one/model-a
// first and provider-two/model-b second, each pointing at its own server.
func newTwoProviderChain(firstURL, secondURL string) (*FallbackChain, *ratelimit.GlobalTracker) {
	rl := ratelimit.NewGlobalTracker()
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "provider-one", ModelID: "model-a", IsFree: true, TierType: "free"},
		{ProviderID: "provider-two", ModelID: "model-b", IsFree: true, TierType: "free"},
	}}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "provider-one", BaseURL: firstURL, Enabled: true},
			{ID: "provider-two", BaseURL: secondURL, Enabled: true},
		},
		Fallback: config.FallbackConfig{MaxAttempts: 3},
	}
	fc := &FallbackChain{
		Cfg: cfg,
		Strategy: &multiMockStrategy{entries: []strategy.RankedEntry{
			{ProviderID: "provider-one", ModelID: "model-a"},
			{ProviderID: "provider-two", ModelID: "model-b"},
		}},
		Catalog:            cat,
		RateLimiter:        rl,
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
	}
	return fc, rl
}

// chainRequest builds a minimal request for the chain-level tests.
func chainRequest() Request {
	return Request{
		Model:    "model-a",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Raw:      map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}
}

// TestFallbackChainClientErrorAbortsChain: a genuine payload defect (400,
// 413) from the first provider is fail-fast — the chain must abort and
// return the upstream response instead of walking the second provider or
// exhausting into the canned 200.
func TestFallbackChainClientErrorAbortsChain(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			var secondHits int32
			failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"error":{"message":"client error %d"}}`, status)
			}))
			defer failSrv.Close()
			okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&secondHits, 1)
				w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
			}))
			defer okSrv.Close()

			fc, _ := newTwoProviderChain(failSrv.URL, okSrv.URL)
			resp, err := fc.Execute(context.Background(), chainRequest())
			if err != nil {
				t.Fatalf("Execute error = %v, want the fail-fast upstream response", err)
			}
			if resp.StatusCode != status {
				t.Errorf("status = %d, want %d", resp.StatusCode, status)
			}
			if got := atomic.LoadInt32(&secondHits); got != 0 {
				t.Errorf("second provider hits = %d, want 0 (payload defect must not walk further providers)", got)
			}
		})
	}
}

// TestFallbackChainAuthErrorFallsThrough: 401/403 are provider-availability
// failures under the three-way classification (second P0 pass, task C) — the
// provider gets a 10-minute cooldown and the walk continues to the next
// candidate, which serves the request. The old contract aborted the whole
// chain on auth errors even though other providers could still serve.
func TestFallbackChainAuthErrorFallsThrough(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			var secondHits int32
			failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"error":{"message":"auth error %d"}}`, status)
			}))
			defer failSrv.Close()
			okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&secondHits, 1)
				w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
			}))
			defer okSrv.Close()

			fc, rl := newTwoProviderChain(failSrv.URL, okSrv.URL)
			resp, err := fc.Execute(context.Background(), chainRequest())
			if err != nil {
				t.Fatalf("Execute error = %v, want the second provider's 200", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (auth error must fall through to the next candidate)", resp.StatusCode)
			}
			if got := atomic.LoadInt32(&secondHits); got != 1 {
				t.Errorf("second provider hits = %d, want 1", got)
			}
			if !rl.IsOnCooldown("provider-one") {
				t.Errorf("provider-one not on cooldown after auth error %d — the walk must mark it", status)
			}
		})
	}
}

// TestFallbackChainServerErrorFallsThrough: 429/500/503 are retryable — the
// chain walks to the next provider and returns its 200.
func TestFallbackChainServerErrorFallsThrough(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			var secondHits int32
			failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer failSrv.Close()
			okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&secondHits, 1)
				w.Write([]byte(`{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"NEXT_OK"},"finish_reason":"stop"}]}`))
			}))
			defer okSrv.Close()

			fc, _ := newTwoProviderChain(failSrv.URL, okSrv.URL)
			resp, err := fc.Execute(context.Background(), chainRequest())
			if err != nil {
				t.Fatalf("Execute error = %v, want success from the second provider", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			if got := atomic.LoadInt32(&secondHits); got != 1 {
				t.Errorf("second provider hits = %d, want 1", got)
			}
			if !strings.Contains(string(resp.Body), "NEXT_OK") {
				t.Errorf("body %q missing second-provider content", resp.Body)
			}
		})
	}
}

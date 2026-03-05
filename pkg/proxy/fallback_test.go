package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// mockStrategy always returns a single ranked entry for the given provider/model.
type mockStrategy struct {
	providerID string
	modelID    string
}

func (m *mockStrategy) Name() string { return "mock" }
func (m *mockStrategy) Rank(_ strategy.Request, _ []catalog.CatalogEntry, _ *models.EnrichedCatalog) []strategy.RankedEntry {
	return []strategy.RankedEntry{{
		ProviderID: m.providerID,
		ModelID:    m.modelID,
	}}
}

// newTestChain builds a minimal FallbackChain pointing at the given server URL.
func newTestChain(serverURL, providerID, modelID string) (*FallbackChain, *ratelimit.GlobalTracker, *catalog.Catalog) {
	cat := &catalog.Catalog{
		Entries: []catalog.CatalogEntry{
			{ProviderID: providerID, ModelID: modelID, IsFree: true, TierType: "free"},
		},
	}
	rl := ratelimit.NewGlobalTracker()
	rel := reliability.New()
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: providerID, BaseURL: serverURL, Enabled: true},
		},
		Fallback: config.FallbackConfig{MaxAttempts: 3},
	}
	fc := &FallbackChain{
		Cfg:                cfg,
		Strategy:           &mockStrategy{providerID: providerID, modelID: modelID},
		Catalog:            cat,
		RateLimiter:        rl,
		ReliabilityTracker: rel,
		HTTPClient:         &http.Client{},
	}
	return fc, rl, cat
}

func TestFallbackChain_404TriggersCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"model not found"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "testprovider", "test-model")
	req := Request{
		Model:    "test-model",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	}

	_, _ = fc.Execute(context.Background(), req)

	if !rl.IsOnCooldown("testprovider") {
		t.Error("expected testprovider to be on cooldown after 404, got not on cooldown")
	}
}

func TestFallbackChain_401TriggersCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "testprovider", "test-model")
	req := Request{
		Model:    "test-model",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	}

	_, _ = fc.Execute(context.Background(), req)

	if !rl.IsOnCooldown("testprovider") {
		t.Error("expected testprovider to be on cooldown after 401, got not on cooldown")
	}
}

func TestFallbackChain_404MarksNeedsReverification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"model not found"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	fc, _, cat := newTestChain(srv.URL, "testprovider", "test-model")
	req := Request{
		Model:    "test-model",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	}

	_, _ = fc.Execute(context.Background(), req)

	entry := cat.Find("testprovider", "test-model")
	if entry == nil || !entry.NeedsReverification {
		t.Error("expected test-model to be flagged NeedsReverification after 404")
	}
}

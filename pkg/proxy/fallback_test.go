package proxy

import (
	"context"
	"encoding/json"
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

func TestCallProvider_NilResolverKeepsRawModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		if m, _ := reqBody["model"].(string); m != "qwen3" {
			t.Errorf("upstream received model %q, want %q (request body must not be rewritten)", m, "qwen3")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","model":"qwen3-32b","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	// No resolver configured: canonical aliasing is off, so UsedModel must
	// keep the raw upstream name even though a canonical name was requested.
	fc, _, _ := newTestChain(srv.URL, "testprovider", "qwen3-32b")
	if fc.Resolver != nil {
		t.Fatal("expected nil resolver on a fresh test chain")
	}
	body := map[string]any{
		"model":    "qwen3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}

	resp, err := fc.callProvider(context.Background(), fc.Cfg.Providers[0], body)
	if err != nil {
		t.Fatalf("callProvider: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if resp.UsedModel != "qwen3-32b" {
		t.Errorf("UsedModel = %q, want raw upstream %q (nil resolver must not rewrite)", resp.UsedModel, "qwen3-32b")
	}
	if got := resp.Header.Get("X-Free-Router-Upstream-Provider"); got != "testprovider" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want %q", got, "testprovider")
	}
	if got := resp.Header.Get("X-Free-Router-Upstream-Model"); got != "qwen3" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want %q", got, "qwen3")
	}
}

func TestCallProvider_CanonicalAliasRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-2","model":"qwen3-32b","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	// Enable canonical aliasing and build the resolver through the same
	// production path used by the server (buildAliasResolver).
	fc, _, cat := newTestChain(srv.URL, "testprovider", "qwen3-32b")
	fc.Cfg.Models = config.ModelsConfig{
		Aliases:          map[string][]string{"qwen3": {"qwen3-32b"}},
		Canonicalization: config.CanonicalizationConfig{Enabled: true},
	}
	fc.Resolver = buildAliasResolver(fc.Cfg, cat)
	if fc.Resolver == nil {
		t.Fatal("expected non-nil resolver when canonicalization is enabled")
	}
	body := map[string]any{
		"model":    "qwen3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}

	resp, err := fc.callProvider(context.Background(), fc.Cfg.Providers[0], body)
	if err != nil {
		t.Fatalf("callProvider: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	// Upstream served the raw ID ("qwen3-32b" echoed in the response body);
	// the alias layer must rewrite UsedModel back to the canonical name.
	if resp.UsedModel != "qwen3" {
		t.Errorf("UsedModel = %q, want canonical %q", resp.UsedModel, "qwen3")
	}
	if got := resp.Header.Get("X-Free-Router-Upstream-Provider"); got != "testprovider" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want %q", got, "testprovider")
	}
	if got := resp.Header.Get("X-Free-Router-Upstream-Model"); got != "qwen3" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want %q", got, "qwen3")
	}
}

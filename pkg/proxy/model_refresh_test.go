package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// unoModelsHandler serves a /v1/models listing from a fixed id list —
// context_length and supported_endpoint_types included the way the API
// exposes them.
func unoModelsHandler(ids []string, ctxLen int, endpoints []string) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type m struct {
			ID                     string   `json:"id"`
			Object                 string   `json:"object"`
			ContextLength          int      `json:"context_length"`
			SupportedEndpointTypes []string `json:"supported_endpoint_types"`
		}
		data := make([]m, 0, len(ids))
		for _, id := range ids {
			data = append(data, m{ID: id, Object: "model", ContextLength: ctxLen, SupportedEndpointTypes: endpoints})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
}

// refreshServer builds a Server + refresher against a fake UnoRouter /v1/models.
func refreshServer(t *testing.T, modelsURL string) (*Server, *ModelRefresher, *catalog.Catalog) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Models.ModelRefreshMinutes = 20
	cfg.Models.Canonicalization.Enabled = true // auto-groups for refreshed ids
	cfg.Providers = []config.ProviderConfig{{
		ID: "unorouter", BaseURL: modelsURL, Enabled: true, RPM: 1,
		Discovery: config.DiscoveryConfig{
			ModelsEndpoint: "/models",
			FreeMarkers:    []string{":free"},
			Refresh:        true,
		},
	}}
	seed := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "a:free", IsFree: true, DiscoveredAt: time.Now().Add(-time.Hour)},
	}}
	s := NewServer(cfg, seed, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())
	return s, NewModelRefresher(s), seed
}

// idsOf returns the catalog's "provider/model" keys.
func idsOf(cat *catalog.Catalog) map[string]bool {
	out := map[string]bool{}
	for _, e := range cat.Entries {
		out[e.ProviderID+"/"+e.ModelID] = true
	}
	return out
}

// TestModelRefreshParsesAndFiltersFreeOnly: the refresh parses the listing,
// keeps ONLY ids ending in ":free" exactly, and captures context length +
// endpoint capabilities into the catalog.
func TestModelRefreshParsesAndFiltersFreeOnly(t *testing.T) {
	var handler http.HandlerFunc = unoModelsHandler(
		[]string{"qwen/qwen3-235b-a22b:free", "paid-model", "vision-model:free", "space-bunny:FREE", "trailing:free:extra"},
		40960,
		[]string{"openai", "tools"},
	)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	s, r, _ := refreshServer(t, srv.URL)

	r.refreshOnce(context.Background())
	cat := s.catalog.Load()
	ids := idsOf(cat)

	if !ids["unorouter/qwen/qwen3-235b-a22b:free"] || !ids["unorouter/vision-model:free"] {
		t.Fatalf(":free ids must survive the refresh, got %v", ids)
	}
	for _, banned := range []string{"unorouter/paid-model", "unorouter/space-bunny:FREE", "unorouter/trailing:free:extra"} {
		if ids[banned] {
			t.Fatalf("%s must be filtered (only exact :free suffix survives), got %v", banned, ids)
		}
	}
	for _, e := range cat.Entries {
		if e.ContextWindow != 40960 {
			t.Fatalf("context length must be captured, got %d for %s", e.ContextWindow, e.ModelID)
		}
		ets := metaStrings(e.Metadata["supported_endpoint_types"])
		if len(ets) != 2 {
			t.Fatalf("endpoint capabilities must be captured, got %v", ets)
		}
	}
	// New ids join canonical groups automatically (auto-grouping).
	if res := s.aliasRes.Load(); res == nil || len(res.Upstreams("qwen3")) == 0 {
		t.Fatal("the refreshed :free model must join its canonical group")
	}
}

// TestModelRefreshFailureKeepsExistingList: a failed refresh keeps the
// provider's existing entries untouched — groups are never cleared (task Q).
func TestModelRefreshFailureKeepsExistingList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"upstream exploded"}`)
	}))
	defer srv.Close()
	s, r, seed := refreshServer(t, srv.URL)

	r.refreshOnce(context.Background())
	cat := s.catalog.Load()
	if got := idsOf(cat); !got["unorouter/a:free"] {
		t.Fatalf("refresh failure must keep the existing list, got %v", got)
	}
	if !cat.Entries[0].DiscoveredAt.Equal(seed.Entries[0].DiscoveredAt) {
		t.Fatal("the kept entry must be the SAME entry, not a rebuild")
	}
}

// TestModelRefreshAddsAndRemovesModels: models new to the live list join
// the group (with empty slot windows); models that disappeared leave the
// routing for new requests (task Q). The ids are variants of ONE family —
// qwen3:free and qwen3-32b:free share the canonical group qwen3.
func TestModelRefreshAddsAndRemovesModels(t *testing.T) {
	current := []string{"qwen3:free", "qwen3-32b:free"}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids := append([]string(nil), current...)
		mu.Unlock()
		unoModelsHandler(ids, 8192, nil).ServeHTTP(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Models.ModelRefreshMinutes = 20
	cfg.Models.Canonicalization.Enabled = true
	cfg.Providers = []config.ProviderConfig{{
		ID: "unorouter", BaseURL: srv.URL, Enabled: true, RPM: 1,
		Discovery: config.DiscoveryConfig{
			ModelsEndpoint: "/models",
			FreeMarkers:    []string{":free"},
			Refresh:        true,
		},
	}}
	seed := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen3:free", IsFree: true},
	}}
	s := NewServer(cfg, seed, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())
	r := NewModelRefresher(s)

	r.refreshOnce(context.Background())
	if got := idsOf(s.catalog.Load()); !got["unorouter/qwen3-32b:free"] {
		t.Fatalf("the new model must join, got %v", got)
	}
	if ups := s.aliasRes.Load().Upstreams("qwen3"); len(ups) != 2 {
		t.Fatalf("qwen3:free and qwen3-32b:free share the canonical group qwen3 — got %d upstreams", len(ups))
	}
	// New models start with empty slot windows.
	if na := s.slotLimit.NextAvailable("unorouter/qwen3-32b:free"); !na.IsZero() {
		t.Fatalf("a new model must start with empty slots, got next_available %s", na.Format("15:04:05"))
	}

	// qwen3-32b:free disappears from the live list.
	mu.Lock()
	current = []string{"qwen3:free"}
	mu.Unlock()
	r.refreshOnce(context.Background())
	if got := idsOf(s.catalog.Load()); got["unorouter/qwen3-32b:free"] {
		t.Fatalf("the disappeared model must leave the group, got %v", got)
	}
	if ups := s.aliasRes.Load().Upstreams("qwen3"); len(ups) != 1 {
		t.Fatalf("the group must shrink to the surviving candidate — got %d", len(ups))
	}
}

// TestModelRefreshClearsNeedsReverification (NG8): a flag installed after a
// 429 is cleared when the live list confirms the model is still free.
func TestModelRefreshClearsNeedsReverification(t *testing.T) {
	srv := httptest.NewServer(unoModelsHandler([]string{"a:free"}, 8192, nil))
	defer srv.Close()
	s, r, _ := refreshServer(t, srv.URL)

	s.catalog.Load().MarkNeedsReverification("unorouter", "a:free")
	if !s.catalog.Load().Entries[0].NeedsReverification {
		t.Fatal("precondition: the flag must be set")
	}
	r.refreshOnce(context.Background())
	if s.catalog.Load().Entries[0].NeedsReverification {
		t.Fatal("a live confirmation must clear the NeedsReverification flag (NG8 reverification)")
	}
	if s.catalog.Load().Entries[0].LastVerifiedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("LastVerifiedAt must be refreshed")
	}
}

// TestGateExcludesFlagged: NG8 — a NeedsReverification-flagged entry is
// never selected while the flag stands; the healthy candidate serves (Q).
func TestGateExcludesFlagged(t *testing.T) {
	rec := &hitRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", BaseURL: "http://127.0.0.1:1", Enabled: true}, // dead — must never be picked
		{ID: "b", BaseURL: srv.URL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a:free", "model-b:free"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "a", ModelID: "model-a:free", IsFree: true, NeedsReverification: true},
		{ProviderID: "b", ModelID: "model-b:free", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req := Request{Model: "qwen3", ClientModel: "qwen3", Messages: []map[string]any{{"role": "user", "content": "hi"}}}
	raw := map[string]any{"model": "qwen3"}
	r := httptest.NewRecorder()
	s.serveAliasModel(r, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), cfg, cat, req, raw, s.aliasRes.Load())
	if r.Code != http.StatusOK {
		t.Fatalf("the healthy candidate must serve, got %d: %s", r.Code, r.Body.String())
	}
	if hits := rec.list(); len(hits) != 1 || hits[0] != "model-b:free" {
		t.Fatalf("the flagged entry must NOT be selected (NG8), hits=%v", hits)
	}
}

// TestGateFailsFastWhenNoCandidateQualifies: a request exceeding every
// candidate's context window fails immediately with no_capable_candidate —
// it never waits in the slot queue (task Q).
func TestGateFailsFastWhenNoCandidateQualifies(t *testing.T) {
	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Fallback.QueueWaitSeconds = 30
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", BaseURL: "http://127.0.0.1:1", Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a:free"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "a", ModelID: "model-a:free", IsFree: true, ContextWindow: 100},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req := Request{
		Model:       "qwen3",
		ClientModel: "qwen3",
		Messages:    []map[string]any{{"role": "user", "content": strings.Repeat("x", 400)}}, // ~100+ tokens
		MaxTokens:   500,
		Raw:         map[string]any{"model": "qwen3"},
	}
	raw := map[string]any{"model": "qwen3"}
	r := httptest.NewRecorder()
	s.serveAliasModel(r, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), cfg, cat, req, raw, s.aliasRes.Load())

	if r.Code != http.StatusBadRequest {
		t.Fatalf("expected the immediate 400 no_capable_candidate, got %d: %s", r.Code, r.Body.String())
	}
	if !strings.Contains(r.Body.String(), "no_capable_candidate") {
		t.Fatalf("expected the no_capable_candidate code, got %s", r.Body.String())
	}
	if got := s.queues.len("qwen3"); got != 0 {
		t.Fatalf("the request must never enter the slot queue, waiters=%d", got)
	}
}

// TestGateToolsAndVisionDemand: the API's capability enumeration is the
// lack-signal — a candidate whose enumeration omits the demanded capability
// is dropped; a candidate the API says nothing about is kept (task Q).
func TestGateToolsAndVisionDemand(t *testing.T) {
	d := demandOf(map[string]any{"tools": []any{map[string]any{"type": "function"}}}, nil, 0)
	if !d.tools {
		t.Fatal("tools in the request must create the demand")
	}
	pass := entryPassesGate(catalog.CatalogEntry{
		ProviderID: "a", ModelID: "model-a:free", IsFree: true,
		Metadata: map[string]any{"supported_endpoint_types": []string{"openai", "tools"}},
	}, d)
	if !pass {
		t.Fatal("a candidate advertising tools must pass")
	}
	fail := entryPassesGate(catalog.CatalogEntry{
		ProviderID: "a", ModelID: "model-b:free", IsFree: true,
		Metadata: map[string]any{"supported_endpoint_types": []string{"openai"}},
	}, d)
	if fail {
		t.Fatal("a candidate whose capability enumeration lacks tools must be dropped")
	}
	kept := entryPassesGate(catalog.CatalogEntry{
		ProviderID: "a", ModelID: "model-c:free", IsFree: true,
	}, d)
	if !kept {
		t.Fatal("a candidate the API says nothing about must be kept")
	}
	// Vision demand from image_url content parts (and the []any JSON shape).
	vd := demandOf(nil, []map[string]any{{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x/y.png"}},
	}}}, 0)
	if !vd.vision {
		t.Fatal("image_url content must create the vision demand")
	}
	vPass := entryPassesGate(catalog.CatalogEntry{
		ProviderID: "a", ModelID: "vl:free", IsFree: true,
		Metadata: map[string]any{"supported_endpoint_types": []any{"openai", "vision"}},
	}, vd)
	if !vPass {
		t.Fatal("a vision-advertising candidate must pass the vision demand")
	}
	vFail := entryPassesGate(catalog.CatalogEntry{
		ProviderID: "a", ModelID: "text-only:free", IsFree: true,
		Metadata: map[string]any{"supported_endpoint_types": []any{"openai"}},
	}, vd)
	if vFail {
		t.Fatal("a text-only candidate must be dropped on the vision demand")
	}
}

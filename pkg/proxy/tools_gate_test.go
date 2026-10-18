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

// Round 9: a tools-unsupported model is never a hard 400. Two layers:
// (1) the capability gate relaxes a tools/vision-only mismatch when it would
// empty the candidate set — the upstream verdict beats stale metadata;
// (2) an upstream 400 whose body says "does not support tools / function
// calling" is bad400Tools → FallbackNext + demotion, so the walk keeps
// hunting for a model that can serve the request.

// toolsDemandRequest builds a request carrying one function tool.
func toolsDemandRequest(model string) (Request, map[string]any) {
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_time"}}}
	raw := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "what time is it?"}},
		"tools":    tools,
	}
	req := Request{
		Model:       model,
		ClientModel: model,
		Messages:    []map[string]any{{"role": "user", "content": "what time is it?"}},
		Raw:         raw,
	}
	return req, raw
}

// textOnlyEntry is the stand-in for stale catalog metadata: the model
// advertises an endpoint type that is NOT tools.
func textOnlyEntry(providerID, modelID string) catalog.CatalogEntry {
	return catalog.CatalogEntry{ProviderID: providerID, ModelID: modelID, IsFree: true,
		ContextWindow: 40960,
		Metadata:      map[string]any{"supported_endpoint_types": []string{"openai"}}}
}

// TestClassify400ToolsUnsupportedWalksOn: real provider error shapes naming
// the missing tools/function capability must fall back, and payload-defect
// near-misses must NOT.
func TestClassify400ToolsUnsupportedWalksOn(t *testing.T) {
	walkOn := []string{
		`{"error":{"message":"Model qwen/qwen3 does not support tools.","code":400}}`,
		`{"error":{"message":"The model ` + "`llama-3.1-8b-instant`" + ` does not support function calling."}}`,
		`{"error":{"message":"Function calling is not enabled for this model."}}`,
		`{"error":{"message":"tool use is not supported by this model"}}`,
		`{"error":{"message":"This provider does not support tools yet."}}`,
	}
	for _, body := range walkOn {
		in := FailureInput{StatusCode: http.StatusBadRequest, Body: []byte(body), RetriesDone: 1}
		if got := ClassifyFailure(in); got != FallbackNext {
			t.Errorf("tools-400 must walk on, got %s for %s", got, body)
		}
	}
	failFast := []string{
		`{"error":{"message":"tools must be an array"}}`,                             // payload defect
		`{"error":{"message":"Invalid type for tool_choice: expected object"}}`,      // payload defect
		`{"error":{"message":"invalid value for tools[0].function.name: too long"}}`, // payload defect
	}
	for _, body := range failFast {
		in := FailureInput{StatusCode: http.StatusBadRequest, Body: []byte(body), RetriesDone: 1}
		if got := ClassifyFailure(in); got != FailFast {
			t.Errorf("payload-defect 400 must fail fast, got %s for %s", got, body)
		}
	}
}

// TestGateEntriesRelaxesOnlyCapabilityMismatches: the two-pass gate —
// tools/vision-only mismatch is relaxed when it empties the set, a context
// mismatch never is, and a non-empty strict pass still prefers tool-capable
// models.
func TestGateEntriesRelaxesOnlyCapabilityMismatches(t *testing.T) {
	entries := []catalog.CatalogEntry{textOnlyEntry("a", "model-a:free")}
	d := capabilityDemand{tools: true}
	if got := gateEntries(entries, d, nil); len(got) != 1 {
		t.Fatalf("tools-only mismatch must be relaxed, got %v", got)
	}
	dCtx := capabilityDemand{neededTokens: 50000}
	if got := gateEntries(entries, dCtx, nil); len(got) != 0 {
		t.Fatal("a context mismatch must stay fatal (never relaxed)")
	}
	toolCapable := textOnlyEntry("a", "tool-model:free")
	toolCapable.Metadata["supported_endpoint_types"] = []string{"openai", "tools"}
	if got := gateEntries([]catalog.CatalogEntry{toolCapable, entries[0]}, d, nil); len(got) != 1 || got[0].ModelID != "tool-model:free" {
		t.Fatalf("strict pass must still drop the tool-less model, got %v", got)
	}
}

// TestTools400FallsBackAndDemotes: the ranked walk end-to-end. Both models
// advertise no tools, so the relaxed gate enters both; model-a answers the
// real-world tools-400, model-b serves 200, and model-a is demoted so the
// NEXT request never touches it again.
func TestTools400FallsBackAndDemotes(t *testing.T) {
	var firstHits, secondHits int32
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Model model-a does not support tools.","code":400}}`)
	}))
	defer failSrv.Close()
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"12:00"}}]}`))
	}))
	defer okSrv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "provider-one", BaseURL: failSrv.URL, Enabled: true},
			{ID: "provider-two", BaseURL: okSrv.URL, Enabled: true},
		},
		Fallback: config.FallbackConfig{MaxAttempts: 3, Unknown400DemotionSeconds: 600},
	}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		textOnlyEntry("provider-one", "model-a"),
		textOnlyEntry("provider-two", "model-b"),
	}}
	fc := &FallbackChain{
		Cfg: cfg,
		Strategy: &multiMockStrategy{entries: []strategy.RankedEntry{
			{ProviderID: "provider-one", ModelID: "model-a"},
			{ProviderID: "provider-two", ModelID: "model-b"},
		}},
		Catalog:            cat,
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
		Demotions:          newDemotionRegistry(),
		Gate:               &capabilityDemand{tools: true},
	}
	req, _ := toolsDemandRequest("model-a")

	resp, err := fc.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the walk must hunt for a serving model), body: %s", resp.StatusCode, resp.Body)
	}
	if got := atomic.LoadInt32(&secondHits); got != 1 {
		t.Fatalf("second provider hits = %d, want 1", got)
	}
	if !fc.Demotions.isDemoted("provider-one", "model-a", time.Now(), 600*time.Second) {
		t.Fatal("the tools-400 offender must be demoted cross-request")
	}
	// The next request's walk drops the demoted offender (real strategies
	// rank only the survivors filterDemotedEntries hands back).
	survivors := fc.filterDemotedEntries(cat.FreeEntries())
	if len(survivors) != 1 || survivors[0].ModelID != "model-b" {
		t.Fatalf("survivors = %v, want only model-b (model-a is demoted)", survivors)
	}
}

// TestAliasRouteTriesToollessModelForToolsDemand: a tools request whose ONLY
// candidate advertises no tools must hit the upstream (relaxed gate), not
// answer the immediate 400.
func TestAliasRouteTriesToollessModelForToolsDemand(t *testing.T) {
	var hits int32
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer okSrv.Close()

	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{{ID: "a", BaseURL: okSrv.URL, Enabled: true}}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a:free"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{textOnlyEntry("a", "model-a:free")}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req, raw := toolsDemandRequest("qwen3")
	rec := httptest.NewRecorder()
	s.serveAliasModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		cfg, cat, req, raw, s.aliasRes.Load())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the tools demand must relax, not 400. body: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// TestAliasRouteContextStillFailsFastOnToolsDemand: the relaxation is
// capability-only — a context-too-small candidate still answers the
// immediate 400 no_capable_candidate (regression anchor for the round 9
// change).
func TestAliasRouteContextStillFailsFastOnToolsDemand(t *testing.T) {
	var hits int32
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer okSrv.Close()

	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{{ID: "a", BaseURL: okSrv.URL, Enabled: true}}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a:free"}}
	small := textOnlyEntry("a", "model-a:free")
	small.ContextWindow = 100
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{small}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req, raw := toolsDemandRequest("qwen3")
	req.MaxTokens = 500
	raw["max_tokens"] = 500
	rec := httptest.NewRecorder()
	s.serveAliasModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		cfg, cat, req, raw, s.aliasRes.Load())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the immediate 400 (context never relaxes). body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_capable_candidate") {
		t.Fatalf("expected the no_capable_candidate code, got %s", rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("upstream hits = %d, want 0", got)
	}
}

// TestDirectRouteTriesToollessModelForToolsDemand: the direct route's
// per-entry gate relaxes a tools-only mismatch and re-enters the entries —
// the old behavior answered 400 no_capable_candidate before ever dialing.
func TestDirectRouteTriesToollessModelForToolsDemand(t *testing.T) {
	var hits int32
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer okSrv.Close()

	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{{ID: "a", BaseURL: okSrv.URL, Enabled: true}}
	cfg.Models.Canonicalization.Enabled = true
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{textOnlyEntry("a", "model-a:free")}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req, raw := toolsDemandRequest("model-a:free")
	rec := httptest.NewRecorder()
	s.serveDirectModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		s.cfg.Load(), cat, req, raw)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the direct route must relax a tools-only mismatch. body: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

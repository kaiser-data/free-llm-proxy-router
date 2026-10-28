package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

	if !rl.IsOnCooldown("testprovider/test-model") {
		t.Error("expected test-model to be on cooldown after 404")
	}
	if rl.IsOnCooldown("testprovider") {
		t.Error("a 404 for one model must not put the whole provider on cooldown")
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

func TestBuildBodyServiceTierOnlyWhenConfigured(t *testing.T) {
	cfg := &config.Config{Providers: []config.ProviderConfig{{ID: "groq", Enabled: true}}}
	fc := &FallbackChain{Cfg: cfg}
	raw := map[string]any{"model": "auto", "stream": true}

	body := fc.buildBody("groq", raw, "llama-3.3-70b-versatile")
	if _, ok := body["service_tier"]; ok {
		t.Fatalf("service_tier sent without config (free Groq orgs reject it): %v", body)
	}
	if _, ok := body["stream"]; ok {
		t.Fatalf("stream must be stripped for buffered fallback: %v", body)
	}

	cfg.Providers[0].NativeFeatures.ServiceTier = "flex"
	body = fc.buildBody("groq", raw, "llama-3.3-70b-versatile")
	if body["service_tier"] != "flex" {
		t.Fatalf("service_tier = %v, want flex", body["service_tier"])
	}
}

func TestIsZeroQuota(t *testing.T) {
	gemini := []byte(`[{"error":{"code":429,"message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 0, model: gemini-nano-banana-2.1"}}]`)
	if !isZeroQuota(gemini) {
		t.Error("Gemini zero-quota 429 not detected")
	}
	if isZeroQuota([]byte(`{"error":{"message":"limit: 10, model: x"}}`)) {
		t.Error("limit: 10 detected as zero quota")
	}
}

// listStrategy returns the given entries in order.
type listStrategy []strategy.RankedEntry

func (l listStrategy) Name() string { return "list" }
func (l listStrategy) Rank(_ strategy.Request, _ []catalog.CatalogEntry, _ *models.EnrichedCatalog) []strategy.RankedEntry {
	return l
}

func TestFallbackChain_429TriesNextModelOfSameProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "paid-model" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"Quota exceeded for metric: free_tier_requests, limit: 0, model: paid-model"}}`))
			return
		}
		w.Write([]byte(`{"model":"free-model","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "gemini", "paid-model")
	fc.Strategy = listStrategy{
		{ProviderID: "gemini", ModelID: "paid-model"},
		{ProviderID: "gemini", ModelID: "free-model"},
	}
	resp, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatalf("expected the second model to answer, got %v", err)
	}
	if resp.UsedModel != "free-model" {
		t.Errorf("UsedModel = %q, want free-model", resp.UsedModel)
	}
	if !rl.IsOnCooldown("gemini/paid-model") {
		t.Error("expected paid-model on cooldown after zero-quota 429")
	}
}

func TestFallbackChain_ModelErrorsCappedPerProvider(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":{"message":"model not found"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "nvidia-nim", "m0")
	fc.Cfg.Fallback.MaxAttempts = 10
	var ranked listStrategy
	for _, id := range []string{"m0", "m1", "m2", "m3", "m4", "m5"} {
		ranked = append(ranked, strategy.RankedEntry{ProviderID: "nvidia-nim", ModelID: id})
	}
	fc.Strategy = ranked
	_, _ = fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if calls != maxModelsPerProvider {
		t.Errorf("provider called %d times, want %d", calls, maxModelsPerProvider)
	}
}

func TestStripGeminiThoughts(t *testing.T) {
	in := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"<thought>*   Question: capital?\n    *   Rome.</thought>Rome"}}],"model":"gemma-4-26b-a4b-it"}`)
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(StripGeminiThoughts(in), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.Choices[0].Message.Content; got != "Rome" {
		t.Errorf("content = %q, want Rome", got)
	}
	if out.Model != "gemma-4-26b-a4b-it" {
		t.Errorf("model field lost: %q", out.Model)
	}
	plain := []byte(`{"choices":[{"message":{"content":"no thoughts here"}}]}`)
	if string(StripGeminiThoughts(plain)) != string(plain) {
		t.Error("body without <thought> must be returned unchanged")
	}
}

func TestFallbackChain_400TriesNextModelOfSameProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "interactions-only" {
			http.Error(w, `{"error":{"code":400,"message":"This model only supports Interactions API."}}`, http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"model":"chat-model","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "gemini", "interactions-only")
	fc.Strategy = listStrategy{
		{ProviderID: "gemini", ModelID: "interactions-only"},
		{ProviderID: "gemini", ModelID: "chat-model"},
	}
	resp, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || resp.UsedModel != "chat-model" {
		t.Fatalf("expected chat-model to answer after a model-specific 400, got resp=%v err=%v", resp, err)
	}
}

func TestFallbackChain_OverloadedModelTriesNext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "busy-model" {
			http.Error(w, `{"error":{"code":503,"message":"This model is currently experiencing high demand."}}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"model":"idle-model","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "gemini", "busy-model")
	fc.Strategy = listStrategy{
		{ProviderID: "gemini", ModelID: "busy-model"},
		{ProviderID: "gemini", ModelID: "idle-model"},
	}
	resp, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || resp.UsedModel != "idle-model" {
		t.Fatalf("expected idle-model after a 503 on busy-model, got resp=%v err=%v", resp, err)
	}
}

func TestFallbackChain_AuthErrorSkipsProvider(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":{"message":"Invalid API Key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "groq", "a")
	fc.Strategy = listStrategy{{ProviderID: "groq", ModelID: "a"}, {ProviderID: "groq", ModelID: "b"}}
	_, _ = fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if calls != 1 {
		t.Errorf("provider called %d times after 401, want 1", calls)
	}
}

func TestCallProviderHonorsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "slow", "m")
	pc := fc.Cfg.Providers[0]
	pc.Timeout = "50ms"
	start := time.Now()
	_, err := fc.callProvider(context.Background(), pc, map[string]any{"model": "m"})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Errorf("timeout not applied: took %v", time.Since(start))
	}
}

func TestFallbackChain_ModelErrorsDoNotCoolDownProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"limit: 0, model: x"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "gemini", "m0")
	fc.Cfg.Fallback.MaxAttempts = 20
	var ranked listStrategy
	for i := 0; i < 12; i++ {
		ranked = append(ranked, strategy.RankedEntry{ProviderID: "gemini", ModelID: fmt.Sprintf("m%d", i)})
	}
	fc.Strategy = ranked
	for i := 0; i < 4; i++ {
		_, _ = fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	}
	if rl.IsOnCooldown("gemini") {
		t.Error("model-level 429s must not put the whole provider on cooldown")
	}
}

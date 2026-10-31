package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

// Requests share one catalog; a failing upstream must not write to it while
// other requests read it (run with -race).
func TestFallbackChain_ConcurrentFailuresDoNotRaceOnCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	fc, _, cat := newTestChain(srv.URL, "testprovider", "test-model")
	req := Request{Model: "test-model", Messages: []map[string]any{{"role": "user", "content": "hi"}}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = fc.Execute(context.Background(), req) }()
		go func() { defer wg.Done(); _ = cat.FreeEntries() }()
	}
	wg.Wait()
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

func TestFallbackChain_EmptyTruncatedTriesNextModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "reasoner" {
			w.Write([]byte(`{"model":"reasoner","choices":[{"message":{"content":""},"finish_reason":"length"}]}`))
			return
		}
		w.Write([]byte(`{"model":"chat-model","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "groq", "reasoner")
	fc.Strategy = listStrategy{
		{ProviderID: "groq", ModelID: "reasoner"},
		{ProviderID: "groq", ModelID: "chat-model"},
	}
	resp, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || resp.UsedModel != "chat-model" {
		t.Fatalf("expected chat-model after empty truncated reply, got resp=%v err=%v", resp, err)
	}
}

func TestFallbackChain_EmptyTruncatedReturnedWhenNothingElse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"reasoner","choices":[{"message":{"content":""},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "groq", "reasoner")
	fc.Strategy = listStrategy{{ProviderID: "groq", ModelID: "reasoner"}}
	resp, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || resp.UsedModel != "reasoner" {
		t.Fatalf("expected the truncated reply rather than exhaustion, got resp=%v err=%v", resp, err)
	}
	if !resp.Truncated {
		t.Error("a last-resort truncated reply must be flagged so it is not cached")
	}
}

func TestIsEmptyTruncated(t *testing.T) {
	cases := map[string]bool{
		`{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`:                     true,
		`{"choices":[{"message":{"content":null},"finish_reason":"length"}]}`:                   true,
		`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`:              false,
		`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`:                       false,
		`{"choices":[{"message":{"content":null,"tool_calls":[{}]},"finish_reason":"length"}]}`: false,
		`not json`: false,
	}
	for body, want := range cases {
		if got := isEmptyTruncated([]byte(body)); got != want {
			t.Errorf("isEmptyTruncated(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestFallbackChain_OpenRouterFollowsStrategyOrder(t *testing.T) {
	orHits := 0
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orHits++
		w.Write([]byte(`{"model":"or-model","choices":[{"message":{"content":"from openrouter"}}]}`))
	}))
	defer or.Close()
	groq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"fast","choices":[{"message":{"content":"from groq"}}]}`))
	}))
	defer groq.Close()

	fc, _, _ := newTestChain(groq.URL, "groq", "fast")
	fc.Cfg.Providers = append(fc.Cfg.Providers, config.ProviderConfig{ID: "openrouter", BaseURL: or.URL, Enabled: true})
	ranked := listStrategy{
		{ProviderID: "groq", ModelID: "fast"},
		{ProviderID: "openrouter", ModelID: "or-model"},
	}
	fc.Strategy = ranked
	resp, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || resp.UsedModel != "fast" {
		t.Fatalf("expected groq (ranked first) to answer, got resp=%v err=%v", resp, err)
	}
	if orHits != 0 {
		t.Errorf("openrouter called %d times although groq was ranked first and answered", orHits)
	}

	// Ranked first, OpenRouter answers before Groq is tried.
	fc.Strategy = listStrategy{ranked[1], ranked[0]}
	resp, err = fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || resp.UsedModel != "or-model" {
		t.Fatalf("expected openrouter (ranked first) to answer, got resp=%v err=%v", resp, err)
	}
}

// Daily usage is persisted from the provider trackers, so a successful
// request has to be counted there or usage.json stays empty.
func TestFallbackChain_SuccessIsCountedInDailyUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "testprovider", "test-model")
	for i := 0; i < 2; i++ {
		if _, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := rl.Snapshot().Providers["testprovider"].ModelDayCounts["test-model"]; got != 2 {
		t.Errorf("daily count = %d, want 2", got)
	}
}

// One Gemini model over its local quota must not hide the other Gemini models.
func TestFallbackChain_GeminiModelOverQuotaTriesNextGeminiModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "gemini", "full")
	fc.Strategy = listStrategy{{ProviderID: "gemini", ModelID: "full"}, {ProviderID: "gemini", ModelID: "spare"}}
	gt := ratelimit.NewGeminiTracker()
	gt.SetLimits("full", ratelimit.GeminiModelLimits{RPM: 1})
	gt.RecordRequest("full", 0)
	fc.GeminiTracker = gt

	if _, err := fc.Execute(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}}); err != nil {
		t.Fatalf("expected the spare Gemini model to answer: %v", err)
	}
	if gotModel != "spare" {
		t.Errorf("upstream got model %q, want spare", gotModel)
	}
}

// A HuggingFace cold-start wait must end when the client goes away.
func TestFallbackChain_HFLoadingWaitHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"Model is currently loading","estimated_time":30}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "huggingface", "m")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	fc.Execute(ctx, Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Execute took %s after the context ended; the wait ignores ctx", took.Round(time.Millisecond))
	}
}

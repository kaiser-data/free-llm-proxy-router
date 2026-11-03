package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

	_, _ = fc.executePass(context.Background(), req, nil)

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

	_, _ = fc.executePass(context.Background(), req, nil)

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

	_, _ = fc.executePass(context.Background(), req, nil)

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
	_, _ = fc.executePass(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}}, nil)
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
	_, _ = fc.executePass(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}}, nil)
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
		_, _ = fc.executePass(context.Background(), Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}}, nil)
	}
	if rl.IsOnCooldown("gemini") {
		t.Error("model-level 429s must not put the whole provider on cooldown")
	}
}

// fastBackoff makes retry tests quick.
func fastBackoff(fc *FallbackChain) {
	fc.Cfg.Fallback.RetryBackoffInitial = time.Millisecond
	fc.Cfg.Fallback.RetryBackoffMax = 5 * time.Millisecond
}

func userReq(raw map[string]any) Request {
	return Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}, Raw: raw}
}

func TestExecute_RetriesPastMaxAttemptsUntilProviderAnswers(t *testing.T) {
	var calls atomic.Int32
	const failures = 7 // more than MaxAttempts (3): a pass ends, the request must not
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= failures {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"model":"real-model","choices":[{"message":{"content":"genuine"}}]}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "gemini", "m")
	fastBackoff(fc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := fc.Execute(ctx, userReq(nil))
	if err != nil {
		t.Fatalf("Execute must wait for a real answer, got error: %v", err)
	}
	if resp.UsedModel != "real-model" || !strings.Contains(string(resp.Body), "genuine") {
		t.Errorf("response is not the provider's: %+v", resp)
	}
	if got := calls.Load(); got != failures+1 {
		t.Errorf("provider called %d times, want %d", got, failures+1)
	}
}

func TestExecute_StaysPendingUntilContextEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "gemini", "m")
	fastBackoff(fc)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	resp, err := fc.Execute(ctx, userReq(nil))
	if resp != nil {
		t.Fatalf("no provider answered, yet got a response: %+v", resp)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline (request pending until client gives up)", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Errorf("Execute returned after %v instead of retrying until the context ended", time.Since(start))
	}
}

func TestExecute_MaxWaitReturnsErrorNotCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "gemini", "m")
	fastBackoff(fc)
	fc.Cfg.Fallback.MaxWait = 60 * time.Millisecond

	resp, err := fc.Execute(context.Background(), userReq(nil))
	if resp != nil || !errors.Is(err, ErrNoProviderAvailable) {
		t.Fatalf("want (nil, ErrNoProviderAvailable), got (%v, %v)", resp, err)
	}
}

func TestExecute_EveryPassSendsTheOriginalRequest(t *testing.T) {
	var calls atomic.Int32
	bodies := make(chan map[string]any, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		if calls.Add(1) < 3 {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "gemini", "m")
	fastBackoff(fc)
	raw := map[string]any{
		"model":       "auto",
		"temperature": 0.2,
		"messages":    []any{map[string]any{"role": "user", "content": "keep me"}},
	}
	if _, err := fc.Execute(context.Background(), userReq(raw)); err != nil {
		t.Fatal(err)
	}
	close(bodies)
	n := 0
	for b := range bodies {
		n++
		msgs, _ := json.Marshal(b["messages"])
		if string(msgs) != `[{"content":"keep me","role":"user"}]` || b["temperature"] != 0.2 {
			t.Errorf("pass %d sent a modified request: %v", n, b)
		}
	}
	if n != 3 {
		t.Errorf("got %d upstream calls, want 3", n)
	}
	if raw["model"] != "auto" {
		t.Errorf("caller's request was mutated: %v", raw)
	}
}

func TestRetryDelay_BoundedAndCooldownAware(t *testing.T) {
	fc := &FallbackChain{RateLimiter: ratelimit.NewGlobalTracker()}
	for i := 0; i < 50; i++ {
		d := fc.retryDelay(time.Second)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("delay %v outside ±20%% of the 1s backoff", d)
		}
	}
	fc.RateLimiter.SetCooldown("p/m", time.Now().Add(300*time.Millisecond))
	if d := fc.retryDelay(30 * time.Second); d > 500*time.Millisecond {
		t.Errorf("delay %v ignores a cooldown that ends in 300ms", d)
	}
}

func TestProxyErrorIsNotACompletion(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProxyError(rec, http.StatusServiceUnavailable, ErrNoProviderAvailable)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["choices"]; ok {
		t.Error("error body must not look like a completion")
	}
	if _, ok := body["error"]; !ok {
		t.Error("error body missing \"error\" object")
	}
}

func TestWriteSSECompletion_AfterKeepaliveDoesNotResendHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	beginSSE(rec)
	rec.Body.WriteString(": keep-alive\n\n")
	writeSSECompletion(rec, []byte(`{"model":"m","choices":[{"message":{"content":"hello"}}]}`), true)
	chunks, done := parseSSE(t, rec.Body.String())
	if !done || len(chunks) == 0 {
		t.Fatalf("stream incomplete: %q", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestIsEmptyCompletion(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		empty bool
	}{
		{"normal", `{"choices":[{"message":{"content":"hi"}}]}`, false},
		{"empty string", `{"choices":[{"message":{"content":""}}]}`, true},
		{"whitespace", `{"choices":[{"message":{"content":"  \n"}}]}`, true},
		{"null content", `{"choices":[{"message":{"content":null}}]}`, true},
		{"no choices", `{"choices":[]}`, true},
		{"missing choices", `{"id":"x"}`, true},
		{"not json", `<html>oops</html>`, true},
		{"reasoning only", `{"choices":[{"message":{"content":"","reasoning_content":"thinking"}}]}`, true},
		{"tool call", `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"1"}]}}]}`, false},
		{"function call", `{"choices":[{"message":{"content":null,"function_call":{"name":"f"}}}]}`, false},
		{"text parts", `{"choices":[{"message":{"content":[{"type":"text","text":"yo"}]}}]}`, false},
		{"empty text parts", `{"choices":[{"message":{"content":[{"type":"text","text":""}]}}]}`, true},
		{"second choice has content", `{"choices":[{"message":{"content":""}},{"message":{"content":"x"}}]}`, false},
	}
	for _, c := range cases {
		if got := isEmptyCompletion([]byte(c.body)); got != c.empty {
			t.Errorf("%s: isEmptyCompletion = %v, want %v", c.name, got, c.empty)
		}
	}
}

// twoProviderChain wires providers "aaa" and "bbb" to separate test servers.
func twoProviderChain(urlA, urlB string) *FallbackChain {
	fc, _, _ := newTestChain(urlA, "aaa", "m")
	fc.Cfg.Providers = append(fc.Cfg.Providers, config.ProviderConfig{ID: "bbb", BaseURL: urlB, Enabled: true})
	fc.Strategy = listStrategy{{ProviderID: "aaa", ModelID: "m"}, {ProviderID: "bbb", ModelID: "m"}}
	fastBackoff(fc)
	return fc
}

const emptyBody = `{"model":"m","choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
const goodBody = `{"model":"m","choices":[{"message":{"role":"assistant","content":"real answer"}}]}`

func TestExecute_EmptyResponseTriesOtherProviderFirst(t *testing.T) {
	var callsA, callsB atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		w.Write([]byte(emptyBody))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB.Add(1)
		w.Write([]byte(goodBody))
	}))
	defer srvB.Close()

	resp, err := twoProviderChain(srvA.URL, srvB.URL).Execute(context.Background(), userReq(nil))
	if err != nil || !strings.Contains(string(resp.Body), "real answer") {
		t.Fatalf("want provider B's real answer, got resp=%v err=%v", resp, err)
	}
	if callsA.Load() != 1 || callsB.Load() != 1 {
		t.Errorf("calls A=%d B=%d, want 1 and 1", callsA.Load(), callsB.Load())
	}
}

func TestExecute_EmptyProviderRetriedOnlyAfterOthers(t *testing.T) {
	var callsA, callsB atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		w.Write([]byte(emptyBody))
	}))
	defer srvA.Close()
	// B fails on its first call, then recovers.
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callsB.Add(1) == 1 {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(goodBody))
	}))
	defer srvB.Close()

	resp, err := twoProviderChain(srvA.URL, srvB.URL).Execute(context.Background(), userReq(nil))
	if err != nil || !strings.Contains(string(resp.Body), "real answer") {
		t.Fatalf("want a real answer, got resp=%v err=%v", resp, err)
	}
	// Pass 1: A empty, B 503. Pass 2: B is tried first and answers; A is not asked again.
	if callsA.Load() != 1 || callsB.Load() != 2 {
		t.Errorf("calls A=%d B=%d, want A=1 (demoted, not retried before B) and B=2", callsA.Load(), callsB.Load())
	}
}

func TestExecute_OnlyEmptyResponsesNeverReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(emptyBody))
	}))
	defer srv.Close()
	fc, _, _ := newTestChain(srv.URL, "gemini", "m")
	fastBackoff(fc)
	fc.Cfg.Fallback.MaxWait = 60 * time.Millisecond

	resp, err := fc.Execute(context.Background(), userReq(nil))
	if resp != nil || !errors.Is(err, ErrNoProviderAvailable) {
		t.Fatalf("an empty completion must never be returned as success; got (%v, %v)", resp, err)
	}
}

func TestDemoteRanked(t *testing.T) {
	in := []strategy.RankedEntry{{ProviderID: "a", ModelID: "1"}, {ProviderID: "b", ModelID: "1"}, {ProviderID: "a", ModelID: "2"}}
	out := demoteRanked(in, map[string]bool{"a/1": true})
	got := fmt.Sprintf("%s/%s %s/%s %s/%s", out[0].ProviderID, out[0].ModelID, out[1].ProviderID, out[1].ModelID, out[2].ProviderID, out[2].ModelID)
	if got != "b/1 a/2 a/1" {
		t.Errorf("order = %s, want b/1 a/2 a/1", got)
	}
}

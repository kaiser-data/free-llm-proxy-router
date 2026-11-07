package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

func hiReq() Request {
	return Request{Messages: []map[string]any{{"role": "user", "content": "hi"}}}
}

const okBody = `{"model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`

// A 429 on key slot 1 must not put the model on cooldown for key slot 2:
// the same model is retried at once with the next key.
func TestFallbackChain_429RetriesSameModelWithNextKey(t *testing.T) {
	t.Setenv("FLPR_TEST_ROT_KEY", "k1")
	t.Setenv("FLPR_TEST_ROT_KEY_2", "k2")
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		keys = append(keys, auth)
		if auth == "Bearer k1" {
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, okBody)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "testprovider", "only-model")
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_TEST_ROT_KEY"

	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatalf("key slot 2 should have answered: %v (keys tried: %v)", err, keys)
	}
	if len(keys) != 2 || keys[1] != "Bearer k2" {
		t.Errorf("keys tried = %v, want [Bearer k1, Bearer k2]", keys)
	}
	// Slot 1 stays cooling down; the next request goes straight to slot 2.
	keys = nil
	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "Bearer k2" {
		t.Errorf("second request tried keys %v, want only Bearer k2", keys)
	}
	if rl.IsOnCooldown("testprovider/only-model") {
		t.Error("model-wide cooldown set although only one key slot is limited")
	}
}

// GROQ_API_KEY_2 alone (no base key) must still be sent.
func TestFallbackChain_OnlyNumberedKeyIsUsed(t *testing.T) {
	t.Setenv("FLPR_TEST_K2ONLY", "")
	t.Setenv("FLPR_TEST_K2ONLY_2", "second")
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		io.WriteString(w, okBody)
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "testprovider", "m")
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_TEST_K2ONLY"
	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer second" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer second")
	}
}

// A 400 means the provider rejects this request shape. Its other models will
// reject it too, so skip the provider — but it is not unhealthy: no
// reliability penalty, no provider cooldown.
func TestFallbackChain_400SkipsProviderWithoutPenalty(t *testing.T) {
	var badCalls, goodCalls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		http.Error(w, `{"error":{"message":"unsupported parameter"}}`, http.StatusBadRequest)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		io.WriteString(w, okBody)
	}))
	defer good.Close()

	fc, rl, _ := newTestChain(bad.URL, "picky", "a")
	fc.Cfg.Providers = append(fc.Cfg.Providers, fc.Cfg.Providers[0])
	fc.Cfg.Providers[1].ID, fc.Cfg.Providers[1].BaseURL = "good", good.URL
	fc.Cfg.Fallback.MaxAttempts = 5
	fc.Strategy = listStrategy{
		{ProviderID: "picky", ModelID: "a"},
		{ProviderID: "picky", ModelID: "b"},
		{ProviderID: "picky", ModelID: "c"},
		{ProviderID: "good", ModelID: "g"},
	}

	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatal(err)
	}
	if n := badCalls.Load(); n != 1 {
		t.Errorf("400 provider called %d times, want 1 (skip its other models)", n)
	}
	if goodCalls.Load() != 1 {
		t.Error("next provider was not tried")
	}
	for _, s := range fc.ReliabilityTracker.All() {
		if s.ProviderID == "picky" {
			t.Errorf("400 recorded against provider reliability: %+v", s)
		}
	}
	if rl.IsOnCooldown("picky") {
		t.Error("400 put the whole provider on cooldown")
	}
}

// Absorbed from PR #16 (BenediktChlopik): a 200 with nothing in it is not an
// answer. Try the next model instead of handing the client an empty reply.
func TestFallbackChain_EmptyCompletionTriesNextModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "hollow" {
			io.WriteString(w, `{"model":"hollow","choices":[{"message":{"content":"  "},"finish_reason":"stop"}]}`)
			return
		}
		io.WriteString(w, `{"model":"solid","choices":[{"message":{"content":"real"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "testprovider", "hollow")
	fc.Strategy = listStrategy{{ProviderID: "testprovider", ModelID: "hollow"}, {ProviderID: "testprovider", ModelID: "solid"}}
	resp, err := fc.Execute(context.Background(), hiReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.UsedModel != "solid" {
		t.Errorf("served by %q, want solid (hollow returned no content)", resp.UsedModel)
	}
}

func TestIsEmptyCompletion(t *testing.T) {
	cases := map[string]bool{
		`not json`:       true,
		`{"choices":[]}`: true,
		`{"choices":[{"message":{"content":""}}]}`:                             true,
		`{"choices":[{"message":{"content":null,"reasoning":"thinking"}}]}`:    true,
		`{"choices":[{"message":{"content":"hi"}}]}`:                           false,
		`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"1"}]}}]}`: false,
		`{"choices":[{"message":{"content":[{"type":"text","text":"x"}]}}]}`:   false,
		`{"choices":[{"text":"legacy"}]}`:                                      false,
	}
	for body, want := range cases {
		if got := isEmptyCompletion([]byte(body)); got != want {
			t.Errorf("isEmptyCompletion(%s) = %v, want %v", body, got, want)
		}
	}
}

// fallback.request_timeout bounds the whole chain, not just one call.
func TestFallbackChain_RequestTimeoutBoundsTheChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "testprovider", "slow")
	fc.Cfg.Fallback.RequestTimeout = 100 * time.Millisecond
	fc.Strategy = listStrategy{{ProviderID: "testprovider", ModelID: "slow"}, {ProviderID: "testprovider", ModelID: "slow2"}}

	start := time.Now()
	_, err := fc.Execute(context.Background(), hiReq())
	if err == nil {
		t.Fatal("expected an error once the request deadline passed")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("chain ran %v, want it cut off near the 100ms request timeout", d)
	}
}

// The direct-model route goes through the same attempt path: a model on
// cooldown is not called.
func TestDirectModel_RespectsCooldown(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, okBody)
	}))
	defer upstream.Close()

	s := newTestServer(upstream.URL, 0)
	s.rateLimiter.SetCooldown("gemini/gemma", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini/gemma","messages":[{"role":"user","content":"hi"}]}`)))
	if n := calls.Load(); n != 0 {
		t.Errorf("model on cooldown was called %d times", n)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503 when the only model is cooling down", rec.Code)
	}
}

// The direct route builds its body like the chain does (service_tier etc.).
func TestDirectModel_UsesBuildBody(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, okBody)
	}))
	defer upstream.Close()

	s := newTestServer(upstream.URL, 0)
	cfg := s.cfg.Load()
	cfg.Providers[0].NativeFeatures.ServiceTier = "flex"
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini/gemma","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got["service_tier"] != "flex" {
		t.Errorf("service_tier = %v, want flex", got["service_tier"])
	}
}

// OpenRouter successes count toward daily usage like every other provider.
func TestFallbackChain_OpenRouterSuccessIsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"meta/llama:free","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "openrouter", "meta/llama:free")
	fc.Strategy = listStrategy{strategy.RankedEntry{ProviderID: "openrouter", ModelID: "meta/llama:free"}}
	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatal(err)
	}
	if got := rl.Snapshot().Providers["openrouter"].ModelDayCounts["meta/llama:free"]; got != 1 {
		t.Errorf("openrouter daily count = %d, want 1", got)
	}
}

// A model on cooldown must also keep OpenRouter's models[] call from using it.
func TestFallbackChain_OpenRouterSkipsModelsOnCooldown(t *testing.T) {
	var sent []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		sent, _ = body["models"].([]any)
		io.WriteString(w, okBody)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "openrouter", "a:free")
	fc.Strategy = listStrategy{{ProviderID: "openrouter", ModelID: "a:free"}, {ProviderID: "openrouter", ModelID: "b:free"}}
	rl.SetCooldown("openrouter/a:free", time.Now().Add(time.Hour))
	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "b:free" {
		t.Errorf("models[] = %v, want [b:free]", sent)
	}
}

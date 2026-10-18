package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// alwaysFailHandler answers every upstream call with a retryable 503.
func alwaysFailHandler() (http.HandlerFunc, *int32) {
	var hits int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"upstream overloaded"}}`)
	}), &hits
}

// TestExhaustionReturns503WithRetryAfter locks in the NG1 spec: when every
// candidate fails with a retryable failure, the client gets 503 Service
// Unavailable with a Retry-After hint and an OpenAI-style JSON error body
// (type "server_error") — never a 200 with the canned
// "temporarily unavailable" completion that agents cannot distinguish from a
// real answer. The alias walk exhausts, falls back to the strategy chain,
// which exhausts too — the single writeExhaustionError point then answers.
func TestExhaustionReturns503WithRetryAfter(t *testing.T) {
	handler, hits := alwaysFailHandler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{{ID: "testprovider", BaseURL: srv.URL, Enabled: true}}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "testprovider", ModelID: "model-a", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req := Request{
		Model:       "qwen3",
		ClientModel: "qwen3",
		Messages:    []map[string]any{{"role": "user", "content": "hi"}},
	}
	raw := map[string]any{"model": "qwen3", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	s.serveAliasModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), cfg, cat, req, raw, s.aliasRes.Load())

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (exhaustion must not answer 200) — body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want the config default 5", got)
	}
	if strings.Contains(rec.Body.String(), "temporarily unavailable") {
		t.Errorf("canned completion leaked: %q", rec.Body.String())
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body is not the JSON error shape: %v — %q", err, rec.Body.String())
	}
	if parsed.Error.Type != "server_error" || parsed.Error.Code != "all_candidates_exhausted" {
		t.Errorf("error body = %+v, want type=server_error code=all_candidates_exhausted", parsed.Error)
	}
	if !strings.Contains(parsed.Error.Message, "exhausted") {
		t.Errorf("error message %q should mention the exhaustion", parsed.Error.Message)
	}
	if got := atomic.LoadInt32(hits); got < 2 {
		t.Errorf("upstream hits = %d, want >= 2 (alias candidate + strategy-chain attempt)", got)
	}
	if got := rec.Header().Get("X-Used-Model"); got != "qwen3" {
		t.Errorf("X-Used-Model = %q, want qwen3", got)
	}
}

// TestExhaustionRetryAfterFromConfig: the Retry-After value comes from
// fallback.retry_after_seconds (here 30), not a hardcoded constant.
func TestExhaustionRetryAfterFromConfig(t *testing.T) {
	s := NewServer(&config.Config{}, &catalog.Catalog{}, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())
	s.cfg.Load().Fallback.RetryAfterSeconds = 30

	rec := httptest.NewRecorder()
	s.writeExhaustionError(rec, "my-model", fmt.Errorf("all providers exhausted after 3 attempts"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30 from fallback.retry_after_seconds", got)
	}
	if got := rec.Header().Get("X-Used-Model"); got != "my-model" {
		t.Errorf("X-Used-Model = %q, want my-model", got)
	}
}

// TestExhaustionStreamRouteGets503Too: streaming requests that exhaust the
// strategy chain get the same 503 JSON error with a Retry-After hint (the old
// path returned a 200 JSON fake completion to an SSE-expecting client).
func TestExhaustionStreamRouteGets503Too(t *testing.T) {
	handler, _ := alwaysFailHandler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{{ID: "testprovider", BaseURL: srv.URL, Enabled: true}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "testprovider", ModelID: "model-a", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req := Request{
		Model:    "auto",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Stream:   true,
	}
	raw := map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true}
	rec := httptest.NewRecorder()
	s.executeStrategyChain(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), cfg, cat, req, raw)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — body: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After header missing on the stream-route exhaustion response")
	}
	if strings.Contains(rec.Body.String(), "temporarily unavailable") {
		t.Errorf("canned completion leaked: %q", rec.Body.String())
	}
}

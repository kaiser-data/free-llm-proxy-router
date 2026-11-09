package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// newTestServer builds a Server with one free Gemini model served by upstreamURL.
func newTestServer(upstreamURL string, cacheTTL int) *Server {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "gemini", BaseURL: upstreamURL, Enabled: true}},
		Fallback:  config.FallbackConfig{MaxAttempts: 3},
	}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Proxy.CacheTTL = cacheTTL
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{{ProviderID: "gemini", ModelID: "gemma", IsFree: true}}}
	rl, rel := ratelimit.NewGlobalTracker(), reliability.New()
	reg := strategy.NewRegistry(rel, rl, "", "", 3, 5)
	return NewServer(cfg, cat, reg, rl, nil, rel)
}

// StripGeminiThoughts shortens the body; the upstream Content-Length must not
// reach the client or the response is cut off.
func TestChatCompletions_RewrittenBodyIsNotTruncated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"model":"gemma","choices":[{"message":{"content":"<thought>long hidden reasoning</thought>Rome"},"finish_reason":"stop"}]}`
		w.Header().Set("Content-Length", fmtLen(body))
		w.Header().Set("X-Ratelimit-Remaining-Requests", "7")
		w.Header().Set("Set-Cookie", "upstream=1")
		io.WriteString(w, body)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(http.HandlerFunc(newTestServer(upstream.URL, 0).handleChatCompletions))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL, "application/json", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"capital of Italy?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading proxied body: %v", err)
	}
	if !strings.Contains(string(got), `"Rome"`) || strings.Contains(string(got), "thought") {
		t.Errorf("body = %s", got)
	}
	if resp.Header.Get("X-Ratelimit-Remaining-Requests") != "7" {
		t.Error("rate-limit headers should still be forwarded")
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("upstream cookies must not be forwarded")
	}
}

func TestChatCompletions_TruncatedReplyIsNotCached(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"model":"gemma","choices":[{"message":{"content":""},"finish_reason":"length"}]}`)
	}))
	defer upstream.Close()

	s := newTestServer(upstream.URL, 300)
	body := `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Header().Get("X-Cache") == "HIT" {
			t.Fatal("empty truncated reply was served from cache")
		}
	}
	if calls < 2 {
		t.Errorf("upstream calls = %d, want a fresh call per request", calls)
	}
}

// YAML numbers from agent profiles are ints; a strategy's max_tokens_cap must
// not mistake a profile's max_tokens for "unset" and raise it.
func TestBuildBody_ProfileIntMaxTokensKeptUnderCap(t *testing.T) {
	cfg := &config.Config{}
	cfg.Proxy.StrategyOverrides = map[string]map[string]any{"mock": {"max_tokens_cap": 4096}}
	fc := &FallbackChain{Cfg: cfg, Strategy: &mockStrategy{}}
	body := fc.buildBody("groq", map[string]any{"max_tokens": 500}, "m")
	if got, _ := toInt(body["max_tokens"]); got != 500 {
		t.Errorf("max_tokens = %v, want 500", body["max_tokens"])
	}
	body = fc.buildBody("groq", map[string]any{"max_tokens": 9000}, "m")
	if got, _ := toInt(body["max_tokens"]); got != 4096 {
		t.Errorf("max_tokens = %v, want capped 4096", body["max_tokens"])
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true,
		"0.0.0.0": false, "": false, "192.168.1.5": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func fmtLen(s string) string { return strconv.Itoa(len(s)) }

// B7: when every provider fails, clients must see a retryable error, not a
// fake assistant reply they would treat as a real answer.
func TestChatCompletions_ExhaustedChainReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	for _, stream := range []bool{false, true} {
		s := newTestServer(upstream.URL, 0)
		body := `{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":` + strconv.FormatBool(stream) + `}`
		rec := httptest.NewRecorder()
		s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("stream=%v: status = %d, want 503; body %s", stream, rec.Code, rec.Body)
		}
		if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
			t.Errorf("stream=%v: Retry-After = %q, want an integer >= 1", stream, rec.Header().Get("Retry-After"))
		}
		if !strings.Contains(rec.Body.String(), `"all_providers_exhausted"`) || strings.Contains(rec.Body.String(), "choices") {
			t.Errorf("stream=%v: body = %s", stream, rec.Body)
		}
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		in   time.Time
		want int
	}{
		{time.Time{}, 5},
		{time.Now().Add(-time.Minute), 1},
		{time.Now().Add(90*time.Second + 500*time.Millisecond), 91},
	}
	for _, c := range cases {
		if got := retryAfterSeconds(c.in); got != c.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestDirectModel_SuccessIsCountedInDailyUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"gemma","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	s := newTestServer(upstream.URL, 0)
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini/gemma","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := s.rateLimiter.Snapshot().Providers["gemini"].ModelDayCounts["gemma"]; got != 1 {
		t.Errorf("daily count = %d, want 1", got)
	}
}

// A client's max_completion_tokens is its explicit budget: an agent profile's
// max_tokens default must not replace it, and the upstream must see only one
// of the two spellings (Gemini 400s on both).
func TestChatCompletions_MaxCompletionTokensBeatsProfileDefault(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"model":"gemma","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	s := newTestServer(upstream.URL, 0)
	s.cfg.Load().Proxy.Agents = map[string]config.AgentProfile{
		"my-agent": {Strategy: "adaptive", Defaults: map[string]any{"max_tokens": 4096}},
	}
	body := `{"model":"my-agent","max_completion_tokens":800,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Errorf("upstream got max_completion_tokens: %v", got)
	}
	if mt, _ := toInt(got["max_tokens"]); mt != 800 {
		t.Errorf("upstream max_tokens = %v, want client's 800", got["max_tokens"])
	}
}

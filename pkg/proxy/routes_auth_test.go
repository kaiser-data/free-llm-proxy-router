package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// authSentinelKey is a fake API key configured on the failing provider — the
// tests assert it NEVER appears in the log output.
const authSentinelKey = "sk-SUPERSENTINEL-401-KEY"

// authPairUpstreams builds the two-candidate scenario for the task-H route
// tests: "badprovider" always answers with the given auth status (401/403),
// "goodprovider" answers 200 — an SSE stream when the request body says
// stream:true, a buffered JSON completion otherwise. Both count their hits.
func authPairUpstreams(t *testing.T, status int, badHits, goodHits *int32) (badURL, goodURL string) {
	t.Helper()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(badHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":{"message":"Invalid API key provided","type":"invalid_request_error"}}`)
	}))
	t.Cleanup(badSrv.Close)
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(goodHits, 1)
		var probe struct {
			Stream bool `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"model-x\",\"choices\":[{\"delta\":{\"content\":\"hi from good\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok","object":"chat.completion","model":"model-x","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(goodSrv.Close)
	return badSrv.URL, goodSrv.URL
}

// authPairConfig builds the config/catalog for the auth route tests.
// onlyBad drops the good provider — the single-candidate exhaustion case.
func authPairConfig(badURL, goodURL string, onlyBad bool) (*config.Config, *catalog.Catalog) {
	providers := []config.ProviderConfig{
		{ID: "badprovider", BaseURL: badURL, Enabled: true, APIKey: authSentinelKey},
	}
	catEntries := []catalog.CatalogEntry{
		{ProviderID: "badprovider", ModelID: "model-x", IsFree: true, TierType: "free"},
	}
	if !onlyBad {
		providers = append(providers, config.ProviderConfig{ID: "goodprovider", BaseURL: goodURL, Enabled: true})
		catEntries = append(catEntries, catalog.CatalogEntry{ProviderID: "goodprovider", ModelID: "model-x", IsFree: true, TierType: "free"})
	}
	cfg := &config.Config{
		Proxy:     config.ProxyConfig{Strategy: "adaptive"},
		Providers: providers,
		Models: config.ModelsConfig{
			Canonicalization: config.CanonicalizationConfig{Enabled: true},
			Aliases:          map[string][]string{"qwen3": {"model-x"}},
		},
		Fallback: config.FallbackConfig{MaxAttempts: 6, RetriesPerCandidate: 0},
	}
	return cfg, &catalog.Catalog{Entries: catEntries}
}

func authPairServer(cfg *config.Config, cat *catalog.Catalog) *Server {
	return NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())
}

// captureLogs redirects the stdlib log writer into a buffer for the test's
// lifetime and returns it.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// authPairRequest builds the request/raw pair for the model-x routes.
func authPairRequest(model string, stream bool) (Request, map[string]any) {
	req := Request{
		Model:       model,
		ClientModel: model,
		Messages:    []map[string]any{{"role": "user", "content": "hi"}},
		Stream:      stream,
	}
	raw := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if stream {
		raw["stream"] = true
	}
	return req, raw
}

// TestAuth401And403BehaveIdenticallyOnEveryRoute locks in the task-H
// contract: EVERY route runs the same ClassifyFailure — a 401/403 from a
// candidate is never surfaced to the client; the provider goes on cooldown;
// a warn-level log names the candidate and the cause; the next candidate
// (or the 503 exhaustion response) answers instead. The log never contains
// the API key or the upstream body.
func TestAuth401And403BehaveIdenticallyOnEveryRoute(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			t.Run("strategy chain walk", func(t *testing.T) {
				var badHits, goodHits int32
				badURL, goodURL := authPairUpstreams(t, status, &badHits, &goodHits)
				cfg, cat := authPairConfig(badURL, goodURL, false)
				rl := ratelimit.NewGlobalTracker()
				fc := &FallbackChain{
					Cfg: cfg,
					Strategy: &multiMockStrategy{entries: []strategy.RankedEntry{
						{ProviderID: "badprovider", ModelID: "model-x"},
						{ProviderID: "goodprovider", ModelID: "model-x"},
					}},
					Catalog:            cat,
					RateLimiter:        rl,
					ReliabilityTracker: reliability.New(),
					HTTPClient:         &http.Client{},
					StreamHTTPClient:   &http.Client{},
				}
				buf := captureLogs(t)
				walkReq, _ := authPairRequest("model-x", false)
				resp, err := fc.Execute(context.Background(), walkReq)
				if err != nil {
					t.Fatalf("Execute error = %v, want the good provider's 200", err)
				}
				if resp.StatusCode != http.StatusOK {
					t.Errorf("status = %d, want 200 served by goodprovider", resp.StatusCode)
				}
				if got := atomic.LoadInt32(&badHits); got != 1 {
					t.Errorf("badprovider hits = %d, want 1", got)
				}
				if got := atomic.LoadInt32(&goodHits); got != 1 {
					t.Errorf("goodprovider hits = %d, want 1", got)
				}
				if !rl.IsOnCooldown("badprovider") {
					t.Error("401/403 must put the provider on cooldown")
				}
				assertAuthLog(t, buf.String(), "badprovider/model-x")
			})

			t.Run("direct route buffered", func(t *testing.T) {
				var badHits, goodHits int32
				badURL, goodURL := authPairUpstreams(t, status, &badHits, &goodHits)
				cfg, cat := authPairConfig(badURL, goodURL, false)
				s := authPairServer(cfg, cat)
				buf := captureLogs(t)
				req, raw := authPairRequest("model-x", false)
				rec := httptest.NewRecorder()
				s.serveDirectModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), s.cfg.Load(), cat, req, raw)
				assertAuthRouteOutcome(t, rec, s.rateLimiter, buf.String(), "", false)
			})

			t.Run("alias route buffered", func(t *testing.T) {
				var badHits, goodHits int32
				badURL, goodURL := authPairUpstreams(t, status, &badHits, &goodHits)
				cfg, cat := authPairConfig(badURL, goodURL, false)
				s := authPairServer(cfg, cat)
				buf := captureLogs(t)
				req, raw := authPairRequest("qwen3", false)
				rec := httptest.NewRecorder()
				s.serveAliasModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), s.cfg.Load(), cat, req, raw, s.aliasRes.Load())
				assertAuthRouteOutcome(t, rec, s.rateLimiter, buf.String(), "qwen3", false)
			})

			t.Run("direct route stream", func(t *testing.T) {
				var badHits, goodHits int32
				badURL, goodURL := authPairUpstreams(t, status, &badHits, &goodHits)
				cfg, cat := authPairConfig(badURL, goodURL, false)
				s := authPairServer(cfg, cat)
				buf := captureLogs(t)
				req, raw := authPairRequest("model-x", true)
				rec := httptest.NewRecorder()
				s.serveDirectModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), s.cfg.Load(), cat, req, raw)
				assertAuthRouteOutcome(t, rec, s.rateLimiter, buf.String(), "", true)
			})

			t.Run("alias route stream", func(t *testing.T) {
				var badHits, goodHits int32
				badURL, goodURL := authPairUpstreams(t, status, &badHits, &goodHits)
				cfg, cat := authPairConfig(badURL, goodURL, false)
				s := authPairServer(cfg, cat)
				buf := captureLogs(t)
				req, raw := authPairRequest("qwen3", true)
				rec := httptest.NewRecorder()
				s.serveAliasModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), s.cfg.Load(), cat, req, raw, s.aliasRes.Load())
				assertAuthRouteOutcome(t, rec, s.rateLimiter, buf.String(), "qwen3", true)
			})

			t.Run("single candidate exhausts to 503", func(t *testing.T) {
				var badHits, goodHits int32
				badURL, goodURL := authPairUpstreams(t, status, &badHits, &goodHits)
				cfg, cat := authPairConfig(badURL, goodURL, true) // no good provider
				s := authPairServer(cfg, cat)
				buf := captureLogs(t)
				req, raw := authPairRequest("qwen3", false)
				rec := httptest.NewRecorder()
				s.serveAliasModel(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), s.cfg.Load(), cat, req, raw, s.aliasRes.Load())

				if rec.Code != http.StatusServiceUnavailable {
					t.Errorf("status = %d, want 503 exhaustion — body: %s", rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), "all_candidates_exhausted") {
					t.Errorf("body = %s, want the exhaustion error body", rec.Body.String())
				}
				// The auth failure itself must never reach the client on any
				// route — not even when every candidate fails.
				if strings.Contains(rec.Body.String(), "Invalid API key") {
					t.Errorf("upstream 401/403 body leaked to the client: %s", rec.Body.String())
				}
				if !s.rateLimiter.IsOnCooldown("badprovider") {
					t.Error("401/403 must put the provider on cooldown")
				}
				assertAuthLog(t, buf.String(), "badprovider/model-x")
			})
		})
	}
}

// assertAuthRouteOutcome checks the shared post-conditions of the direct and
// alias route subtests (buffered and stream): 200 from the good candidate,
// the auth failure nowhere in the client body, cooldown set, warn log clean.
func assertAuthRouteOutcome(t *testing.T, rec *httptest.ResponseRecorder, rl *ratelimit.GlobalTracker, logs, xUsedModel string, stream bool) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the good candidate — body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Invalid API key") {
		t.Errorf("upstream 401/403 body leaked to the client: %s", rec.Body.String())
	}
	if stream {
		if !strings.Contains(rec.Body.String(), "hi from good") || !strings.Contains(rec.Body.String(), "data: [DONE]") {
			t.Errorf("client missed the committed stream: %q", rec.Body.String())
		}
	}
	if xUsedModel != "" && rec.Header().Get("X-Used-Model") != xUsedModel {
		t.Errorf("X-Used-Model = %q, want %q", rec.Header().Get("X-Used-Model"), xUsedModel)
	}
	if !rl.IsOnCooldown("badprovider") {
		t.Error("401/403 must put the provider on cooldown")
	}
	assertAuthLog(t, logs, "badprovider/model-x")
}

// assertAuthLog checks the warn-level auth log: it names the candidate and
// the cause, and never leaks the API key or the upstream error body.
func assertAuthLog(t *testing.T, logs, candidate string) {
	t.Helper()
	if !strings.Contains(logs, "WARN auth failure") {
		t.Errorf("missing the warn-level auth log in:\n%s", logs)
	}
	if !strings.Contains(logs, "candidate="+candidate) {
		t.Errorf("auth log must name the candidate %s:\n%s", candidate, logs)
	}
	if !strings.Contains(logs, "credential rejected") {
		t.Errorf("auth log must state the cause:\n%s", logs)
	}
	if strings.Contains(logs, authSentinelKey) {
		t.Errorf("API key leaked into the logs:\n%s", logs)
	}
	if strings.Contains(logs, "Invalid API key provided") {
		t.Errorf("upstream error body echoed into the logs:\n%s", logs)
	}
}

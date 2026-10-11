package proxy

import (
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
)

// newAliasRouteServer builds a Server whose canonical alias "qwen3" maps to
// two upstream candidates ("model-a", "model-b") served by the given URL
// through a single test provider. clientTimeout > 0 injects a short-timeout
// upstream client for the timeout-classification test.
func newAliasRouteServer(t *testing.T, upstreamURL string, clientTimeout time.Duration) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{
		{ID: "testprovider", BaseURL: upstreamURL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a", "model-b"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "testprovider", ModelID: "model-a", IsFree: true},
		{ProviderID: "testprovider", ModelID: "model-b", IsFree: true},
	}}
	s := NewServer(cfg, cat, nil, nil, nil, nil)
	if clientTimeout > 0 {
		s.chainHTTPClient = &http.Client{Timeout: clientTimeout}
	}
	return s
}

// serveAliasRequest drives serveAliasModel with a canonical "qwen3" request,
// mirroring the production routing path.
func serveAliasRequest(s *Server) *httptest.ResponseRecorder {
	req := Request{
		Model:    "qwen3",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	}
	raw := map[string]any{
		"model":    "qwen3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.serveAliasModel(rec, httpReq, s.cfg.Load(), s.catalog.Load(), req, raw, s.aliasRes.Load())
	return rec
}

// failThenSucceedHandler: the first upstream call replies failStatus (hangs
// for hangFirst when > 0); every later call replies with a valid 200
// completion. hits counts upstream attempts.
func failThenSucceedHandler(failStatus int, hangFirst time.Duration) (http.HandlerFunc, *int32) {
	var hits int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			if hangFirst > 0 {
				// Hang past the proxy client's timeout. The proxy
				// client aborting its request is not reliably visible
				// to this handler (it never reads/writes), so the
				// hang must be bounded for Server.Close to finish.
				select {
				case <-r.Context().Done():
				case <-time.After(hangFirst):
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failStatus)
			fmt.Fprintf(w, `{"error":{"message":"upstream rejected with %d"}}`, failStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"ok","object":"chat.completion","model":"model-b","choices":[{"index":0,"message":{"role":"assistant","content":"FALLBACK_OK"},"finish_reason":"stop"}]}`))
	}), &hits
}

// TestAliasRouteClientErrorsSurfaceImmediately locks in the error
// classification contract: a genuine payload defect (400 ClassifyFailure
// cannot map to a candidate-shaped fix) surfaces as-is, attempts only one
// candidate, and never walks further or degrades into the exhaustion
// response. 401/403 are NOT in this table anymore — task H moved them to the
// cooldown+fallback contract shared by every route
// (TestAliasRouteAuthErrorsCooldownAndFallback here and
// TestAuth401And403BehaveIdenticallyOnEveryRoute for the full matrix).
func TestAliasRouteClientErrorsSurfaceImmediately(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			handler, hits := failThenSucceedHandler(status, 0)
			srv := httptest.NewServer(handler)
			defer srv.Close()
			s := newAliasRouteServer(t, srv.URL, 0)

			rec := serveAliasRequest(s)
			if rec.Code != status {
				t.Errorf("status = %d, want %d (upstream error must surface as-is)", rec.Code, status)
			}
			if got := atomic.LoadInt32(hits); got != 1 {
				t.Errorf("upstream hits = %d, want 1 (non-retryable must not walk further candidates)", got)
			}
			if !strings.Contains(rec.Body.String(), fmt.Sprintf("upstream rejected with %d", status)) {
				t.Errorf("body %q does not contain the upstream error", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "temporarily unavailable") {
				t.Errorf("canned exhaustion response leaked for a client error: %q", rec.Body.String())
			}
		})
	}
}

// TestAliasRouteAuthErrorsCooldownAndFallback: 401/403 on the alias route
// now follow the same contract as every route (task H) — the failure is
// never surfaced, the provider goes on cooldown, and the walk continues to
// the next candidate, which serves the request.
func TestAliasRouteAuthErrorsCooldownAndFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			handler, hits := failThenSucceedHandler(status, 0)
			srv := httptest.NewServer(handler)
			defer srv.Close()
			s := newAliasRouteServer(t, srv.URL, 0)
			s.rateLimiter = ratelimit.NewGlobalTracker() // the harness passes nil — the cooldown assertions need the real tracker

			rec := serveAliasRequest(s)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (401/403 must walk to the next candidate, task H)", rec.Code)
			}
			if got := atomic.LoadInt32(hits); got != 2 {
				t.Errorf("upstream hits = %d, want 2 (auth failure walks, next candidate serves)", got)
			}
			if !strings.Contains(rec.Body.String(), "FALLBACK_OK") {
				t.Errorf("body %q missing second-candidate content", rec.Body.String())
			}
			if !s.rateLimiter.IsOnCooldown("testprovider") {
				t.Error("401/403 must put the provider on cooldown")
			}
		})
	}
}

// TestAliasRouteRetryableErrorsFallback: 408/429/5xx walk to the next
// candidate, which serves the request.
func TestAliasRouteRetryableErrorsFallback(t *testing.T) {
	for _, status := range []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			handler, hits := failThenSucceedHandler(status, 0)
			srv := httptest.NewServer(handler)
			defer srv.Close()
			s := newAliasRouteServer(t, srv.URL, 0)

			rec := serveAliasRequest(s)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (retryable failure must fall through to next candidate)", rec.Code)
			}
			if got := atomic.LoadInt32(hits); got != 2 {
				t.Errorf("upstream hits = %d, want 2 (first failed, second served)", got)
			}
			if !strings.Contains(rec.Body.String(), "FALLBACK_OK") {
				t.Errorf("body %q missing second-candidate content", rec.Body.String())
			}
		})
	}
}

// TestAliasRouteTimeoutFallback verifies that an upstream timeout is
// classified as retryable: the proxy aborts the hanging first candidate and
// is served by the second one instead of exhausting.
func TestAliasRouteTimeoutFallback(t *testing.T) {
	handler, hits := failThenSucceedHandler(0, 2*time.Second)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	s := newAliasRouteServer(t, srv.URL, 100*time.Millisecond)

	rec := serveAliasRequest(s)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (timeout is retryable → next candidate)", rec.Code)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2", got)
	}
	if !strings.Contains(rec.Body.String(), "FALLBACK_OK") {
		t.Errorf("body %q missing second-candidate content", rec.Body.String())
	}
}

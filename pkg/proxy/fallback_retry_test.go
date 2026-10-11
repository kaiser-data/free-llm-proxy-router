package proxy

import (
	"context"
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

// statusThenOKHandler: the first upstream hit replies failStatus, every later
// hit replies with a valid completion. hits counts upstream attempts.
func statusThenOKHandler(failStatus int) (http.HandlerFunc, *int32) {
	var hits int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failStatus)
			fmt.Fprintf(w, `{"error":{"message":"transient failure %d"}}`, failStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"RETRY_OK"},"finish_reason":"stop"}]}`))
	}), &hits
}

// newRetryChain builds a single-candidate chain with explicit retry config,
// mirroring the production default flags (retry_on_429/5xx default true via
// viper; here they are spelled out per test).
func newRetryChain(serverURL string, retries, on429, on5xx int) (*FallbackChain, *ratelimit.GlobalTracker) {
	rl := ratelimit.NewGlobalTracker()
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "testprovider", ModelID: "test-model", IsFree: true, TierType: "free"},
	}}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "testprovider", BaseURL: serverURL, Enabled: true},
		},
		Fallback: config.FallbackConfig{
			MaxAttempts:         3,
			RetriesPerCandidate: retries,
			RetryOn429:          on429 == 1,
			RetryOn5xx:          on5xx == 1,
		},
	}
	fc := &FallbackChain{
		Cfg:                cfg,
		Strategy:           &mockStrategy{providerID: "testprovider", modelID: "test-model"},
		Catalog:            cat,
		RateLimiter:        rl,
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
	}
	return fc, rl
}

// retryRequest builds the minimal chain request for these tests.
func retryRequest() Request {
	return Request{
		Model:    "test-model",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Raw:      map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}
}

// TestSameCandidateRetryRecovers locks in the G1 fix: a transient 500 on the
// only candidate no longer drops the provider — one same-candidate retry
// with backoff succeeds and the request is served.
func TestSameCandidateRetryRecovers(t *testing.T) {
	handler, hits := statusThenOKHandler(http.StatusInternalServerError)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	fc, _ := newRetryChain(srv.URL, 1, 1, 1)

	resp, err := fc.Execute(context.Background(), retryRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want success after same-candidate retry", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (initial call + one retry)", got)
	}
	if !strings.Contains(string(resp.Body), "RETRY_OK") {
		t.Errorf("body %q missing retried-candidate content", resp.Body)
	}
}

// TestSameCandidateRetryDisabledWhenZero: retries_per_candidate = 0 restores
// the old fail-once-then-skip behavior (single candidate → exhaustion).
func TestSameCandidateRetryDisabledWhenZero(t *testing.T) {
	handler, hits := statusThenOKHandler(http.StatusInternalServerError)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	fc, _ := newRetryChain(srv.URL, 0, 1, 1)

	_, err := fc.Execute(context.Background(), retryRequest())
	if err == nil {
		t.Fatal("expected exhaustion error with retries disabled")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (old fail-once behavior)", got)
	}
}

// TestRetryOn5xxFlagGatesRetry: the previously dead retry_on_5xx config now
// actually gates same-candidate retries for 5xx failures.
func TestRetryOn5xxFlagGatesRetry(t *testing.T) {
	handler, hits := statusThenOKHandler(http.StatusInternalServerError)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	fc, _ := newRetryChain(srv.URL, 1, 1, 0)

	_, err := fc.Execute(context.Background(), retryRequest())
	if err == nil {
		t.Fatal("expected exhaustion when retry_on_5xx = false")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (flag must gate the retry)", got)
	}
}

// TestRetryOn429FlagGatesRetry: 429 same-candidate retry follows
// retry_on_429 — off means one call and the model cooldown as before.
func TestRetryOn429FlagGatesRetry(t *testing.T) {
	handler, hits := statusThenOKHandler(http.StatusTooManyRequests)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	fc, rl := newRetryChain(srv.URL, 1, 0, 1)

	_, err := fc.Execute(context.Background(), retryRequest())
	if err == nil {
		t.Fatal("expected exhaustion when retry_on_429 = false")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (flag must gate the retry)", got)
	}
	if !rl.IsOnCooldown("testprovider/test-model") {
		t.Error("expected the per-model cooldown after 429 (existing behavior preserved)")
	}
}

// Test429RetriesWhenFlagOn: retry_on_429 = true (the production default)
// gives a 429 one same-candidate retry; on success no model cooldown is set.
func Test429RetriesWhenFlagOn(t *testing.T) {
	handler, hits := statusThenOKHandler(http.StatusTooManyRequests)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	fc, rl := newRetryChain(srv.URL, 1, 1, 0)

	resp, err := fc.Execute(context.Background(), retryRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want success after same-candidate retry", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2", got)
	}
	if rl.IsOnCooldown("testprovider/test-model") {
		t.Error("model cooldown must not be set when the same-candidate retry succeeds")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// Test400NeverSameCandidateRetried: client errors are non-retryable —
// surfaced as-is with exactly one upstream call.
func Test400NeverSameCandidateRetried(t *testing.T) {
	handler, hits := statusThenOKHandler(http.StatusBadRequest)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	fc, _ := newRetryChain(srv.URL, 1, 1, 1)

	resp, err := fc.Execute(context.Background(), retryRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want the upstream 400 surfaced", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 surfaced as-is", resp.StatusCode)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (non-retryable must not be retried)", got)
	}
}

// TestRetriesConsumeMaxAttemptsBudget: each retry consumes the shared
// max_attempts budget, keeping the total upstream call bound intact.
func TestRetriesConsumeMaxAttemptsBudget(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	fc, _ := newRetryChain(srv.URL, 1, 1, 1)
	fc.Cfg.Fallback.MaxAttempts = 2

	_, err := fc.Execute(context.Background(), retryRequest())
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if !strings.Contains(err.Error(), "2 attempts") {
		t.Errorf("error = %v, want exhaustion after 2 attempts (call + retry share the budget)", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (max_attempts bounds total calls)", got)
	}
}

// TestRetriesStopAtMaxAttemptsCeiling: retries share the max_attempts budget
// AND the ceiling holds inside one candidate's retry chain — the total number
// of upstream calls never exceeds max_attempts. Before the ceiling check was
// added inside the retry loop, the walk only checked the ceiling between
// candidates, so candidate two could push the total to
// max_attempts + retries_per_candidate (here 6 calls for a budget of 4).
func TestRetriesStopAtMaxAttemptsCeiling(t *testing.T) {
	var hitsA, hitsB int32
	fail500 := func(counter *int32) http.HandlerFunc {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(counter, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"always failing"}}`)
		})
	}
	srvA := httptest.NewServer(fail500(&hitsA))
	defer srvA.Close()
	srvB := httptest.NewServer(fail500(&hitsB))
	defer srvB.Close()

	rl := ratelimit.NewGlobalTracker()
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "provider-a", ModelID: "model-a", IsFree: true, TierType: "free"},
		{ProviderID: "provider-b", ModelID: "model-b", IsFree: true, TierType: "free"},
	}}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "provider-a", BaseURL: srvA.URL, Enabled: true},
			{ID: "provider-b", BaseURL: srvB.URL, Enabled: true},
		},
		Fallback: config.FallbackConfig{
			MaxAttempts:         4,
			RetriesPerCandidate: 2,
			RetryOn429:          true,
			RetryOn5xx:          true,
		},
	}
	fc := &FallbackChain{
		Cfg: cfg,
		Strategy: &multiMockStrategy{entries: []strategy.RankedEntry{
			{ProviderID: "provider-a", ModelID: "model-a"},
			{ProviderID: "provider-b", ModelID: "model-b"},
		}},
		Catalog:            cat,
		RateLimiter:        rl,
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
	}

	_, err := fc.Execute(context.Background(), retryRequest())
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	a, b := atomic.LoadInt32(&hitsA), atomic.LoadInt32(&hitsB)
	if a != 3 {
		t.Errorf("provider-a hits = %d, want 3 (initial call + 2 retries fit the budget)", a)
	}
	if b != 1 {
		t.Errorf("provider-b hits = %d, want 1 (initial call only — budget spent, ceiling blocks the retry)", b)
	}
	if a+b != 4 {
		t.Errorf("total upstream calls = %d, want exactly max_attempts (4)", a+b)
	}
}

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// multiStrategy ranks the given (provider, model) pairs in order.
type multiStrategy struct{ entries []strategy.RankedEntry }

func (m *multiStrategy) Name() string { return "mock-multi" }
func (m *multiStrategy) Rank(_ strategy.Request, _ []catalog.CatalogEntry, _ *models.EnrichedCatalog) []strategy.RankedEntry {
	return m.entries
}

// TestSlotKeyIsPerModel locks the confirmed quota shape (task R): the free
// quota is 1 request/minute PER MODEL, so two DIFFERENT models of the SAME
// provider must each grant their own slot concurrently, while the SAME
// model denies the second request into the queue window.
func TestSlotKeyIsPerModel(t *testing.T) {
	l := NewSlotLimiter()
	l.window = 20 * time.Millisecond

	// Two different models, same provider, booked concurrently: both slots
	// granted. Exercise the concurrency a few rounds under -race.
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		results := make([]bool, 2)
		for i, model := range []string{"model-a", "model-b"} {
			wg.Add(1)
			go func(i int, model string) {
				defer wg.Done()
				granted, _ := l.Reserve("tp/"+model, 1)
				results[i] = granted
			}(i, model)
		}
		wg.Wait()
		if !results[0] || !results[1] {
			t.Fatalf("round %d: different models of one provider must hold SEPARATE slots — got %v", round, results)
		}
		time.Sleep(25 * time.Millisecond) // let the round's reservations age out
	}
	if act := l.activeCount("tp/model-a"); act != 0 {
		t.Fatalf("model-a slot pool must drain with the window, %d active", act)
	}

	// Same model twice (fresh limiter, production one-minute window): only
	// one slot; the second request is denied and pointed ~one window away
	// (it waits in the queue or the walk moves to another model).
	l2 := NewSlotLimiter()
	granted1, _ := l2.Reserve("tp/same", 1)
	granted2, next := l2.Reserve("tp/same", 1)
	if !granted1 || granted2 {
		t.Fatalf("same model must share one slot: first=%v second=%v", granted1, granted2)
	}
	if !next.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("denied same-model request should be told ~one window away, got %s", next)
	}
	if l2.activeCount("tp/same") != 1 {
		t.Fatalf("same-model slot count = %d, want 1", l2.activeCount("tp/same"))
	}
}

// TestSlotEnterPerModelOverride: providers[].rpm_limit_overrides gives a
// specific model its own cap (or 0 = unlimited) while the provider-wide RPM
// still applies to every other model.
func TestSlotEnterPerModelOverride(t *testing.T) {
	l := NewSlotLimiter()
	prov := &config.ProviderConfig{
		ID:  "tp",
		RPM: 1,
		RPMLimits: map[string]int{
			"big-model": 3,
			"unlimited": 0,
		},
	}
	keys := []string{}

	// big-model: override 3 → three slots, the fourth denied.
	for i := 0; i < 3; i++ {
		if !slotEnter(l, prov, "big-model", &keys) {
			t.Fatalf("big-model slot %d/3 must be granted", i+1)
		}
	}
	if slotEnter(l, prov, "big-model", &keys) {
		t.Fatal("big-model fourth request must be denied at the override cap 3")
	}
	// unlimited: override 0 → never denied.
	for i := 0; i < 10; i++ {
		if !slotEnter(l, prov, "unlimited", &keys) {
			t.Fatalf("unlimited model must never be denied (request %d)", i+1)
		}
	}
	// other-model: provider-wide RPM 1 → one slot, the second denied.
	if !slotEnter(l, prov, "plain-model", &keys) {
		t.Fatal("plain-model first request must be granted at the provider-wide rpm_limit 1")
	}
	if slotEnter(l, prov, "plain-model", &keys) {
		t.Fatal("plain-model second request must be denied — separate slot pool from the overrides")
	}
	// The denied keys carry the "provider/model" identity (per-model slots).
	if len(keys) == 0 || keys[len(keys)-1] != "tp/plain-model" {
		t.Fatalf("blocked keys must be per-model, got %v", keys)
	}
}

// Test429FallsBackToNextModelImmediately is the walk-level task-R contract:
// model A answers 429 (with a Retry-After), model B (same provider) answers
// 200 — the walk must land on B in the same request, immediately (no sleep,
// no same-model retry), park A on its slot next_available, NOT flag A as
// NeedsReverification (a quota hit is not "no longer free"), and score the
// burn lighter than a timeout/empty-200 would be.
func Test429FallsBackToNextModelImmediately(t *testing.T) {
	var hitsA, hitsB int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Model {
		case "model-a":
			atomic.AddInt32(&hitsA, 1)
			w.Header().Set("Retry-After", "60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit exceeded for model-a"}}`))
		default:
			atomic.AddInt32(&hitsB, 1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"ok","model":"model-b","choices":[{"index":0,"message":{"role":"assistant","content":"B_OK"},"finish_reason":"stop"}]}`))
		}
	}))
	defer upstream.Close()

	fc := &FallbackChain{
		Cfg: &config.Config{
			Providers: []config.ProviderConfig{{ID: "tp", BaseURL: upstream.URL, Enabled: true, RPM: 1}},
			Fallback: config.FallbackConfig{
				MaxAttempts:         3,
				RetriesPerCandidate: 1,
				RetryOn429:          true, // the retired default must change nothing
			},
		},
		Strategy: &multiStrategy{entries: []strategy.RankedEntry{
			{ProviderID: "tp", ModelID: "model-a"},
			{ProviderID: "tp", ModelID: "model-b"},
		}},
		Catalog: &catalog.Catalog{Entries: []catalog.CatalogEntry{
			{ProviderID: "tp", ModelID: "model-a", IsFree: true, TierType: "free"},
			{ProviderID: "tp", ModelID: "model-b", IsFree: true, TierType: "free"},
		}},
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
		Slots:              NewSlotLimiter(),
	}
	req := Request{
		Model:    "model-a",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Raw:      map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}

	start := time.Now()
	resp, err := fc.Execute(context.Background(), req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute error = %v, want model-b's 200", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), "B_OK") {
		t.Fatalf("status=%d body=%q, want model-b's 200 B_OK", resp.StatusCode, resp.Body)
	}
	// Immediate: no backoff sleep, no same-model retry.
	if elapsed > 2*time.Second {
		t.Fatalf("walk took %s — a 429 must fall back to the next model without sleeping", elapsed)
	}
	if atomic.LoadInt32(&hitsA) != 1 {
		t.Errorf("model-a hits = %d, want 1 (no same-model retry)", hitsA)
	}
	if atomic.LoadInt32(&hitsB) != 1 {
		t.Errorf("model-b hits = %d, want 1", hitsB)
	}
	// A is parked: next_available ~60s from the upstream Retry-After.
	if na := fc.Slots.NextAvailable("tp/model-a"); !na.After(time.Now().Add(50 * time.Second)) {
		t.Errorf("model-a next_available = %s, want ~60s from the 429's Retry-After", na)
	}
	// B took its own (separate) slot.
	if fc.Slots.activeCount("tp/model-b") != 1 {
		t.Errorf("model-b must hold its own slot, active=%d", fc.Slots.activeCount("tp/model-b"))
	}
	// A quota hit must NOT flag the model NeedsReverification (that is the
	// 404 contract) — A returns to the pool at next_available.
	if e := fc.Catalog.Find("tp", "model-a"); e == nil || e.NeedsReverification {
		t.Error("a 429 must not set NeedsReverification on the model")
	}
	// The score burn: a 429 maps to OutcomeRateLimited (0.3), lighter than
	// OutcomeWasted (0.0) for timeout/empty-200 — same slot burned, but the
	// cause is actionable.
	tr := reliability.New()
	tr.RecordOutcome("tp", failureOutcome(&Response{StatusCode: http.StatusTooManyRequests}, nil))
	rate429 := tr.SuccessRate("tp")
	tr2 := reliability.New()
	tr2.RecordOutcome("tp", failureOutcome(nil, context.DeadlineExceeded)) // timeout → wasted
	rateTimeout := tr2.SuccessRate("tp")
	if !(rate429 > rateTimeout) {
		t.Errorf("429 score %v must drop LIGHTER than timeout %v", rate429, rateTimeout)
	}
	_ = elapsed
}

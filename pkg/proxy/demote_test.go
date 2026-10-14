package proxy

import (
	"context"
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

// unknown400Body matches NO marker table — the unrecognized-400 class the
// round-8 policy targets.
const unknown400Body = `{"error":{"message":"mystery barrel exploded","code":400}}`

// TestDemotionRegistryTTLExpiry: a demotion hides the model only for the
// TTL; a lazy-expired entry is popped (handed back off to the ranking).
func TestDemotionRegistryTTLExpiry(t *testing.T) {
	reg := newDemotionRegistry()
	now := time.Now()
	reg.demote("prova", "model-a", now)
	if !reg.isDemoted("prova", "model-a", now.Add(time.Second), time.Minute) {
		t.Fatal("freshly demoted model must be hidden")
	}
	if reg.isDemoted("prova", "model-a", now.Add(2*time.Minute), time.Minute) {
		t.Fatal("expired demotion must hand the model back off")
	}
	if reg.len() != 0 {
		t.Fatalf("registry len = %d, want 0 after expiry", reg.len())
	}
	if reg.isDemoted("provb", "model-b", now, time.Minute) {
		t.Fatal("unknown key must not be demoted")
	}
}

// TestDemotionRegistryFIFOHandOff: the registry is bounded — when full the
// OLDEST demotion is handed back off to make room for the newest.
func TestDemotionRegistryFIFOHandOff(t *testing.T) {
	reg := newDemotionRegistry()
	now := time.Now()
	for i := 0; i <= demotionRegistryMax; i++ { // max+1 demotes → one eviction
		reg.demote("p", fmt.Sprintf("m%02d", i), now)
	}
	if reg.isDemoted("p", "m00", now, time.Minute) {
		t.Fatal("oldest demotion must be handed back off when the registry overflows")
	}
	if !reg.isDemoted("p", fmt.Sprintf("m%02d", demotionRegistryMax), now, time.Minute) {
		t.Fatal("newest demotion must still be hidden")
	}
	if reg.len() != demotionRegistryMax {
		t.Fatalf("registry len = %d, want %d", reg.len(), demotionRegistryMax)
	}
}

// TestDemotionRegistryRedemoteRefreshes: a repeat offender refreshes its
// timestamp but (by construction) keeps its FIFO hand-off position.
func TestDemotionRegistryRedemoteRefreshes(t *testing.T) {
	reg := newDemotionRegistry()
	base := time.Now()
	reg.demote("p", "m1", base)
	reg.demote("p", "m1", base.Add(50*time.Second))
	if !reg.isDemoted("p", "m1", base.Add(100*time.Second), time.Minute) {
		t.Fatal("re-demote must refresh the TTL window")
	}
	if reg.isDemoted("p", "m1", base.Add(50*time.Second+2*time.Minute), time.Minute) {
		t.Fatal("refreshed window must still lapse")
	}
}

// TestFailureDecisionWalkCrossModelConfirm walks the ranked-walk classifier
// through the whole policy: first distinct model walks on, second distinct
// model fail-fasts, same-model re-entry does not grow the set, recognized
// 400 subclasses are untouched, and the legacy (zero-policy) chain stays
// fail-fast with an empty seen-set.
func TestFailureDecisionWalkCrossModelConfirm(t *testing.T) {
	fc := &FallbackChain{
		Cfg: &config.Config{Fallback: config.FallbackConfig{
			Unknown400Policy:          policyCrossModelConfirm,
			Unknown400ConfirmN:        2,
			Unknown400DemotionSeconds: 600,
		}},
		Demotions: newDemotionRegistry(),
	}
	resp := func() *Response {
		return &Response{StatusCode: 400, Body: []byte(unknown400Body)}
	}

	seen := make(map[string]bool)
	if got := fc.failureDecisionWalk(resp(), nil, "pa", "ma", seen); got != FallbackNext {
		t.Fatalf("first distinct unknown-400 = %s, want fallback", got)
	}
	if len(seen) != 1 || !seen["pa/ma"] {
		t.Fatalf("seen-set = %v, want {pa/ma}", seen)
	}
	if !fc.Demotions.isDemoted("pa", "ma", time.Now(), fc.demotionTTL()) {
		t.Fatal("policy-rerouted model must join the demotion registry")
	}
	if got := fc.failureDecisionWalk(resp(), nil, "pb", "mb", seen); got != FailFast {
		t.Fatalf("second distinct unknown-400 = %s, want fail_fast", got)
	}
	if !fc.Demotions.isDemoted("pb", "mb", time.Now(), fc.demotionTTL()) {
		t.Fatal("confirming model must be demoted too")
	}
	if got := fc.failureDecisionWalk(resp(), nil, "pa", "ma", seen); got != FailFast {
		t.Fatalf("same-model re-entry = %s, want fail_fast (set must not grow)", got)
	}
	if len(seen) != 2 {
		t.Fatalf("seen-set = %v, want 2 entries after re-entry", seen)
	}

	// Recognized subclasses: content-policy fail-fasts WITHOUT touching the
	// set; a 500 is decided exactly like the legacy classifier.
	seenPolicy := make(map[string]bool)
	policy400 := &Response{StatusCode: 400, Body: []byte(`{"error":{"code":"content_filter","message":"safety system"}}`)}
	if got := fc.failureDecisionWalk(policy400, nil, "pc", "mc", seenPolicy); got != FailFast || len(seenPolicy) != 0 {
		t.Fatalf("content-policy 400 = %s len=%d, want fail_fast and untouched set", got, len(seenPolicy))
	}
	seen500 := make(map[string]bool)
	if got := fc.failureDecisionWalk(&Response{StatusCode: 500, Body: []byte("boom")}, nil, "pd", "md", seen500); got != FallbackNext || len(seen500) != 0 {
		t.Fatalf("500 = %s len=%d, want fallback and untouched set", got, len(seen500))
	}
	// Transport error (resp nil): context canceled → fail_fast, untouched.
	seenErr := make(map[string]bool)
	if got := fc.failureDecisionWalk(nil, context.Canceled, "pe", "me", seenErr); got != FailFast || len(seenErr) != 0 {
		t.Fatalf("canceled = %s len=%d, want fail_fast and untouched set", got, len(seenErr))
	}

	// Legacy chain: zero policy → first unknown-400 fail-fasts, set empty.
	fcZero := &FallbackChain{Cfg: &config.Config{}}
	seenZero := make(map[string]bool)
	if got := fcZero.failureDecisionWalk(resp(), nil, "pf", "mf", seenZero); got != FailFast || len(seenZero) != 0 {
		t.Fatalf("legacy zero-policy = %s len=%d, want fail_fast and untouched set", got, len(seenZero))
	}
}

// TestWalkModelUnsupported400DemotesAndWalks (round 12, live UnoRouter):
// a 400 naming the MODEL as unserved is a deterministic model defect —
// the walk moves on and the model is parked WITHOUT the cross-model
// confirm dance, so a good payload never surfaces the stale model's 400
// (the 50-request soak's only failure, request 49).
func TestWalkModelUnsupported400DemotesAndWalks(t *testing.T) {
	fc := &FallbackChain{
		Cfg: &config.Config{Fallback: config.FallbackConfig{
			Unknown400Policy:          policyCrossModelConfirm,
			Unknown400ConfirmN:        2,
			Unknown400DemotionSeconds: 600,
		}},
		Demotions: newDemotionRegistry(),
	}
	body := `{"error":{"message":"Model ling-3.0-flash-sante:free not supported (request id: 202610141752059615692358268d9d6WUP8wiNz)","type":"invalid_request_error","param":"","code":"invalid_model"}}`
	resp := &Response{StatusCode: 400, Body: []byte(body)}

	seen := make(map[string]bool)
	if got := fc.failureDecisionWalk(resp, nil, "unorouter", "ling-3.0-flash-sante:free", seen); got != FallbackNext {
		t.Fatalf("model-unsupported 400 = %s, want fallback", got)
	}
	if len(seen) != 0 {
		t.Fatalf("confirm set = %v, want untouched (no confirm dance)", seen)
	}
	if !fc.Demotions.isDemoted("unorouter", "ling-3.0-flash-sante:free", time.Now(), fc.demotionTTL()) {
		t.Fatal("model-unsupported must park the model in the demotion registry")
	}

	// Legacy zero-TTL chain: still walks on (recognized subclass verdict is
	// policy-independent, like tools-unsupported); no registry entry because
	// demotion is disabled by the zeroed config.
	fcNoDemote := &FallbackChain{Cfg: &config.Config{}, Demotions: newDemotionRegistry()}
	seenNoDemote := make(map[string]bool)
	if got := fcNoDemote.failureDecisionWalk(resp, nil, "pa", "ma", seenNoDemote); got != FallbackNext || len(seenNoDemote) != 0 {
		t.Fatalf("legacy model-unsupported = %s len=%d, want fallback and untouched set", got, len(seenNoDemote))
	}
}

// orderedStrategy ranks the catalog entries the WALK hands it, in slice
// order — unlike mockStrategy it must respect the input list so the tests
// can prove that demotion filtering actually removes entries before
// ranking.
type orderedStrategy struct{}

func (orderedStrategy) Name() string { return "ordered" }
func (orderedStrategy) Rank(_ strategy.Request, entries []catalog.CatalogEntry, _ *models.EnrichedCatalog) []strategy.RankedEntry {
	out := make([]strategy.RankedEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, strategy.RankedEntry{Entry: e, ProviderID: e.ProviderID, ModelID: e.ModelID})
	}
	return out
}

// newDemoteChain builds a two-candidate chain (prova/model-a ranked first,
// provb/model-b second) sharing the given demotion registry — mirroring
// production where the registry outlives per-request chains.
func newDemoteChain(aURL, bURL string, dem *demotionRegistry, demotionSeconds int) *FallbackChain {
	return &FallbackChain{
		Cfg: &config.Config{
			Providers: []config.ProviderConfig{
				{ID: "prova", BaseURL: aURL, Enabled: true},
				{ID: "provb", BaseURL: bURL, Enabled: true},
			},
			Fallback: config.FallbackConfig{
				MaxAttempts:               8,
				Unknown400Policy:          policyCrossModelConfirm,
				Unknown400ConfirmN:        2,
				Unknown400DemotionSeconds: demotionSeconds,
			},
		},
		Strategy: orderedStrategy{},
		Catalog: &catalog.Catalog{Entries: []catalog.CatalogEntry{
			{ProviderID: "prova", ModelID: "model-a", IsFree: true, TierType: "free"},
			{ProviderID: "provb", ModelID: "model-b", IsFree: true, TierType: "free"},
		}},
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
		Demotions:          dem,
	}
}

// TestWalkCrossModelConfirmRecovers is the end-to-end lock for round 8:
// request 1 hits the unknown-400 model, the policy walks to the next model
// and the request still succeeds; the failing model lands in the shared
// registry; request 2 never touches it again (gate-level skip, not just
// ordering).
func TestWalkCrossModelConfirmRecovers(t *testing.T) {
	var aHits int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(unknown400Body))
	}))
	defer bad.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"CONFIRM_OK"},"finish_reason":"stop"}]}`))
	}))
	defer ok.Close()

	dem := newDemotionRegistry()

	// Request 1: model-a unknown-400s → confirm walks to model-b → 200.
	fc1 := newDemoteChain(bad.URL, ok.URL, dem, 600)
	resp, err := fc1.Execute(context.Background(), retryRequest())
	if err != nil {
		t.Fatalf("request 1 error = %v, want success via the confirmed next model", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&aHits); got != 1 {
		t.Fatalf("model-a hits after request 1 = %d, want 1", got)
	}
	if !dem.isDemoted("prova", "model-a", time.Now(), 600*time.Second) {
		t.Fatal("model-a must be demoted after the unknown-400")
	}

	// Request 2 (fresh chain, SHARED registry): model-a is filtered at the
	// gate — the bad upstream is never contacted again.
	fc2 := newDemoteChain(bad.URL, ok.URL, dem, 600)
	resp2, err := fc2.Execute(context.Background(), retryRequest())
	if err != nil {
		t.Fatalf("request 2 error = %v, want success via model-b", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200", resp2.StatusCode)
	}
	if got := atomic.LoadInt32(&aHits); got != 1 {
		t.Fatalf("model-a hits after request 2 = %d, want 1 (demoted model must be skipped)", got)
	}
}

// TestWalkUnknown400FailsFastAtConfirmN: two DISTINCT models answering the
// same unrecognized 400 ends the walk with the real upstream 400 surfaced —
// exactly the legacy fail-fast, just two confirmations later.
func TestWalkUnknown400FailsFastAtConfirmN(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(unknown400Body))
	}))
	defer bad.Close()

	fc := newDemoteChain(bad.URL, bad.URL, newDemotionRegistry(), 600)
	resp, err := fc.Execute(context.Background(), retryRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want the surfaced upstream 400", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 surfaced", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "mystery barrel") {
		t.Fatalf("body = %q, want the real upstream error body", resp.Body)
	}
}

// TestFilterDemotedEntriesLastChance: demotion must never starve the walk —
// when every candidate is demoted they are handed back rather than leaving
// the request with zero candidates.
func TestFilterDemotedEntriesLastChance(t *testing.T) {
	reg := newDemotionRegistry()
	reg.demote("prova", "model-a", time.Now())
	reg.demote("provb", "model-b", time.Now())
	fc := &FallbackChain{
		Cfg:       &config.Config{Fallback: config.FallbackConfig{Unknown400DemotionSeconds: 60}},
		Demotions: reg,
	}
	entries := []catalog.CatalogEntry{
		{ProviderID: "prova", ModelID: "model-a", IsFree: true},
		{ProviderID: "provb", ModelID: "model-b", IsFree: true},
	}
	if out := fc.filterDemotedEntries(entries); len(out) != 2 {
		t.Fatalf("all-demoted candidates = %d entries, want 2 handed back (no starvation)", len(out))
	}

	// Mixed case: model-a/b stay demoted, model-c is fresh → only it survives.
	entries = append(entries, catalog.CatalogEntry{ProviderID: "prova", ModelID: "model-c", IsFree: true})
	out := fc.filterDemotedEntries(entries)
	if len(out) != 1 || out[0].ModelID != "model-c" {
		t.Fatalf("mixed filter = %+v, want only model-c", out)
	}

	// Disabled (ttl 0): input passes through untouched.
	fcOff := &FallbackChain{Cfg: &config.Config{}, Demotions: reg}
	if out := fcOff.filterDemotedEntries(entries); len(out) != len(entries) {
		t.Fatalf("disabled demotion filter = %d entries, want %d", len(out), len(entries))
	}
}

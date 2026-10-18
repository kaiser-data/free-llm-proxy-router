package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/alias"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// ttftTestConfig returns a minimal config with the round-10 knobs at their
// documented defaults (mirroring the viper defaults for zeroed structs).
func ttftTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Fallback.Unknown400DemotionSeconds = 600
	cfg.Fallback.TTFTSlowThresholdSeconds = 10
	cfg.Fallback.TTFTBreakerTrips = 3
	cfg.Fallback.TTFTBreakerMinSamples = 3
	return cfg
}

func TestLatencyRegistryEMAAndStreak(t *testing.T) {
	l := newLatencyRegistry()
	// First observation seeds the EMA.
	l.recordTTFTObservation("prov", "m", 1*time.Second, false, 3)
	st, ok := l.get("prov", "m")
	if !ok || st.Samples != 1 || st.TTFTemaMS != 1000 {
		t.Fatalf("first sample: %+v ok=%v", st, ok)
	}
	// Fast observations keep the streak at zero.
	l.recordTTFTObservation("prov", "m", 2*time.Second, false, 3)
	st, _ = l.get("prov", "m")
	if st.SlowStreak != 0 || st.TTFTemaMS != 1000*0.75+2000*0.25 {
		t.Fatalf("ema/streak after fast sample: %+v", st)
	}
	// Slow observations build the streak.
	l.recordTTFTObservation("prov", "m", 12*time.Second, true, 3)
	st, _ = l.get("prov", "m")
	if st.SlowStreak != 1 {
		t.Fatalf("slow streak after one slow sample: %+v", st)
	}
	// A fast observation resets the streak.
	l.recordTTFTObservation("prov", "m", 1*time.Second, false, 3)
	st, _ = l.get("prov", "m")
	if st.SlowStreak != 0 {
		t.Fatalf("streak not reset by fast sample: %+v", st)
	}
}

func TestLatencyBreakerTripsOncePerStreak(t *testing.T) {
	l := newLatencyRegistry()
	slow := time.Duration(11) * time.Second
	if l.recordTTFTObservation("prov", "m", slow, true, 3) {
		t.Fatal("trip on first slow observation")
	}
	if l.recordTTFTObservation("prov", "m", slow, true, 3) {
		t.Fatal("trip on second slow observation")
	}
	if !l.recordTTFTObservation("prov", "m", slow, true, 3) {
		t.Fatal("no trip on third consecutive slow observation")
	}
	st, _ := l.get("prov", "m")
	if st.SlowStreak != 0 || st.BreakerTrips != 1 {
		t.Fatalf("post-trip state: %+v", st)
	}
	// Re-tripping needs another full streak of consecutive slows.
	if l.recordTTFTObservation("prov", "m", slow, true, 3) {
		t.Fatal("immediate re-trip")
	}
	if l.recordTTFTObservation("prov", "m", slow, true, 3) {
		t.Fatal("re-trip after two")
	}
	if !l.recordTTFTObservation("prov", "m", slow, true, 3) {
		t.Fatal("no re-trip after second full streak")
	}
	st, _ = l.get("prov", "m")
	if st.BreakerTrips != 2 {
		t.Fatalf("trips counter: %+v", st)
	}
}

func TestApplyTTFTObservationDemotesOnTrip(t *testing.T) {
	l := newLatencyRegistry()
	d := newDemotionRegistry()
	fb := ttftTestConfig().Fallback
	slow := 12 * time.Second
	applyTTFTObservation(l, d, fb, "prov", "m", slow, false)
	applyTTFTObservation(l, d, fb, "prov", "m", slow, false)
	if d.isDemoted("prov", "m", time.Now(), 600*time.Second) {
		t.Fatal("demoted before the breaker tripped")
	}
	applyTTFTObservation(l, d, fb, "prov", "m", slow, false)
	if !d.isDemoted("prov", "m", time.Now(), 600*time.Second) {
		t.Fatal("breaker trip did not demote the model")
	}
}

func TestApplyTTFTObservationForcedSlowCountsTowardStreak(t *testing.T) {
	l := newLatencyRegistry()
	d := newDemotionRegistry()
	fb := ttftTestConfig().Fallback
	// Watchdog aborts below the slow threshold still count (forced).
	applyTTFTObservation(l, d, fb, "prov", "m", 5*time.Second, true)
	applyTTFTObservation(l, d, fb, "prov", "m", 5*time.Second, true)
	applyTTFTObservation(l, d, fb, "prov", "m", 5*time.Second, true)
	if !d.isDemoted("prov", "m", time.Now(), 600*time.Second) {
		t.Fatal("forced-slow streak did not trip the breaker")
	}
}

func TestDeprioritizeSlowRankedMovesToBack(t *testing.T) {
	cfg := ttftTestConfig()
	cfg.Fallback.TTFTSlowThresholdSeconds = 10 // ms threshold on the EMA
	l := newLatencyRegistry()
	// a and c are fast; b answered slowly three times.
	for i := 0; i < 3; i++ {
		l.recordTTFTObservation("prov", "b", 45*time.Second, true, 3)
	}
	fc := &FallbackChain{Cfg: cfg, Latencies: l}
	ranked := []strategy.RankedEntry{
		{ProviderID: "prov", ModelID: "a"},
		{ProviderID: "prov", ModelID: "b"},
		{ProviderID: "prov", ModelID: "c"},
	}
	out := fc.deprioritizeSlowRanked(ranked)
	if out[0].ModelID != "a" || out[1].ModelID != "c" || out[2].ModelID != "b" {
		t.Fatalf("slow candidate not moved to the back: %v", modelIDs(rankedToAny(out)))
	}
	// Never dropped: the same set comes back.
	if len(out) != len(ranked) {
		t.Fatalf("candidate dropped: %d → %d", len(ranked), len(out))
	}
}

func TestDeprioritizeSlowRankedNotEnoughSamples(t *testing.T) {
	cfg := ttftTestConfig()
	l := newLatencyRegistry()
	// One slow sample only — below ttft_breaker_min_samples (3): the EMA
	// is not trusted yet and the order stays.
	l.recordTTFTObservation("prov", "b", 45*time.Second, true, 3)
	fc := &FallbackChain{Cfg: cfg, Latencies: l}
	ranked := []strategy.RankedEntry{
		{ProviderID: "prov", ModelID: "a"},
		{ProviderID: "prov", ModelID: "b"},
	}
	out := fc.deprioritizeSlowRanked(ranked)
	if out[0].ModelID != "a" || out[1].ModelID != "b" {
		t.Fatalf("order changed below the sample floor: %v", out)
	}
}

func TestDeprioritizeSlowRankedAllSlowKeepsOrder(t *testing.T) {
	cfg := ttftTestConfig()
	l := newLatencyRegistry()
	l.recordTTFTObservation("prov", "a", 45*time.Second, true, 3)
	l.recordTTFTObservation("prov", "b", 45*time.Second, true, 3)
	l.recordTTFTObservation("prov", "c", 45*time.Second, true, 3)
	fc := &FallbackChain{Cfg: cfg, Latencies: l}
	ranked := []strategy.RankedEntry{
		{ProviderID: "prov", ModelID: "a"},
		{ProviderID: "prov", ModelID: "b"},
		{ProviderID: "prov", ModelID: "c"},
	}
	out := fc.deprioritizeSlowRanked(ranked)
	for i, want := range []string{"a", "b", "c"} {
		if out[i].ModelID != want {
			t.Fatalf("all-slow walk reordered: %v", out)
		}
	}
}

func TestDeprioritizeSlowUpstreamsMovesToBack(t *testing.T) {
	cfg := ttftTestConfig()
	l := newLatencyRegistry()
	l.recordTTFTObservation("prov", "slow", 45*time.Second, true, 3)
	l.recordTTFTObservation("prov", "slow", 45*time.Second, true, 3)
	l.recordTTFTObservation("prov", "slow", 45*time.Second, true, 3)
	fc := &FallbackChain{Cfg: cfg, Latencies: l}
	ups := []alias.Upstream{
		{ProviderID: "prov", ModelID: "slow"},
		{ProviderID: "prov", ModelID: "fast"},
	}
	out := fc.deprioritizeSlowUpstreams(ups)
	if out[0].ModelID != "fast" || out[1].ModelID != "slow" {
		t.Fatalf("slow upstream not moved to the back: %v", out)
	}
}

func TestRecordTTFTNilRegistryNoop(t *testing.T) {
	fc := &FallbackChain{Cfg: ttftTestConfig()}
	fc.recordTTFT("prov", "m", 45*time.Second, true) // must not panic
	s := &Server{}
	s.recordTTFT("prov", "m", 45*time.Second, true) // must not panic
}

func rankedToAny(in []strategy.RankedEntry) []any {
	out := make([]any, 0, len(in))
	for _, r := range in {
		out = append(out, r.ModelID)
	}
	return out
}

func modelIDs(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, v.(string))
	}
	return out
}

// TestStreamUpstreamHeldReportsTTFTOnCommit pins the streaming TTFT
// measurement: the commit point (first usable event) is the observation.
func TestStreamUpstreamHeldReportsTTFTOnCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`data: {"model":"m","choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var gotProvider, gotModel string
	var gotTTFT time.Duration
	var forced bool
	cfg := config.ProviderConfig{ID: "fakeprov", BaseURL: srv.URL}
	params := streamClientParams{
		Client:            srv.Client(),
		FirstTokenTimeout: 5 * time.Second,
		OnTTFT: func(p, m string, d time.Duration, f bool) {
			gotProvider, gotModel, gotTTFT, forced = p, m, d, f
		},
	}
	rec := httptest.NewRecorder()
	_, committed, err := streamUpstreamHeld(context.Background(), rec, cfg, map[string]any{"model": "m"}, params)
	if err != nil || !committed {
		t.Fatalf("commit failed: err=%v committed=%v", err, committed)
	}
	if gotProvider != "fakeprov" || gotModel != "m" || gotTTFT <= 0 || forced {
		t.Fatalf("OnTTFT observation: provider=%q model=%q ttft=%v forced=%v", gotProvider, gotModel, gotTTFT, forced)
	}
}

// TestStreamUpstreamHeldReportsTTFTOnWatchdogAbort pins the forced-slow
// observation: a TTFT-watchdog abort is counted slow regardless of the
// configured threshold.
func TestStreamUpstreamHeldReportsTTFTOnWatchdogAbort(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never writes — the watchdog must abort the attempt
	}))
	defer srv.Close()
	defer close(release)

	var gotTTFT time.Duration
	var forced bool
	cfg := config.ProviderConfig{ID: "fakeprov", BaseURL: srv.URL}
	params := streamClientParams{
		Client:            srv.Client(),
		FirstTokenTimeout: 150 * time.Millisecond,
		OnTTFT: func(_, _ string, d time.Duration, f bool) {
			gotTTFT, forced = d, f
		},
	}
	rec := httptest.NewRecorder()
	_, committed, err := streamUpstreamHeld(context.Background(), rec, cfg, map[string]any{"model": "m"}, params)
	if err == nil || committed {
		t.Fatalf("watchdog abort expected: err=%v committed=%v", err, committed)
	}
	if gotTTFT <= 0 || !forced {
		t.Fatalf("forced observation missing: ttft=%v forced=%v", gotTTFT, forced)
	}
}

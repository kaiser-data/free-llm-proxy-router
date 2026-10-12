package proxy

import (
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/alias"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// latencyEMAAlpha is the smoothing factor for the per-model TTFT EMA
// (0..1): higher reacts faster to recent measurements, lower is more
// stable. One slow 45s answer moves the EMA noticeably but does not
// dominate a long history.
const latencyEMAAlpha = 0.25

// Effective defaults for the round-10 knobs; a zeroed FallbackConfig (as
// built by tests) must not read as "everything disabled" or "trip the
// breaker on the first observation".
const (
	ttftSlowThresholdDefault     = 10 * time.Second
	ttftBreakerTripsDefault      = 3
	ttftBreakerMinSamplesDefault = 3
)

// ttftSlowThresholdFrom returns the slow-TTFT observation threshold
// (fallback.ttft_slow_threshold_seconds).
func ttftSlowThresholdFrom(fb config.FallbackConfig) time.Duration {
	if s := fb.TTFTSlowThresholdSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return ttftSlowThresholdDefault
}

// ttftBreakerTripsFrom returns the consecutive-slow streak that trips the
// per-model circuit breaker (fallback.ttft_breaker_trips).
func ttftBreakerTripsFrom(fb config.FallbackConfig) int {
	if n := fb.TTFTBreakerTrips; n > 0 {
		return n
	}
	return ttftBreakerTripsDefault
}

// ttftBreakerMinSamplesFrom returns how many TTFT samples a candidate needs
// before the EMA-based deprioritization trusts it
// (fallback.ttft_breaker_min_samples).
func ttftBreakerMinSamplesFrom(fb config.FallbackConfig) int {
	if n := fb.TTFTBreakerMinSamples; n > 0 {
		return n
	}
	return ttftBreakerMinSamplesDefault
}

// latencyStats is the measured time-to-first-token history of ONE
// provider/model pair. Model-granular on purpose (the provider-level
// reliability EMA already exists): a free pool serves many models per
// provider and a slow 70B weights server must not drag the provider's
// fast small models out of the ranking with it.
type latencyStats struct {
	// Key is "provider/model" (demotionKey).
	Key string `json:"key"`
	// TTFTemaMS is the EMA of the measured TTFT in milliseconds.
	TTFTemaMS float64 `json:"ttft_ema_ms"`
	// LastTTFTMS is the most recent measurement in milliseconds.
	LastTTFTMS int64 `json:"last_ttft_ms"`
	// Samples counts the recorded observations.
	Samples int64 `json:"samples"`
	// SlowStreak counts consecutive slow observations (TTFT at/above the
	// slow threshold, or a forced observation); any fast observation
	// resets it to zero.
	SlowStreak int `json:"slow_streak"`
	// BreakerTrips counts how many times the circuit breaker opened for
	// this pair — each trip parked the model in the demotion registry.
	BreakerTrips int       `json:"breaker_trips"`
	LastUpdated  time.Time `json:"last_updated"`
}

// latencyRegistry is the cross-request latency memory (round 10): the
// measured time-to-first-token of every provider/model pair, an EMA over
// it, and the consecutive-slow streak that drives the slow-TTFT circuit
// breaker. In-memory by design (like the demotion registry) — a proxy
// restart forgets, and the upstreams re-prove themselves with real
// traffic.
type latencyRegistry struct {
	mu      sync.Mutex
	entries map[string]*latencyStats
}

func newLatencyRegistry() *latencyRegistry {
	return &latencyRegistry{entries: make(map[string]*latencyStats)}
}

// recordTTFTObservation records one measured TTFT. slow=true counts toward
// the consecutive-slow streak; a fast observation resets it. When the
// streak reaches breakerTrips (> 0) the breaker "trips": the return value
// is true exactly once per trip and the streak resets, so re-tripping
// needs another N consecutive slow observations. A nil registry or a
// non-positive ttft is a no-op.
func (l *latencyRegistry) recordTTFTObservation(providerID, modelID string, ttft time.Duration, slow bool, breakerTrips int) (tripped bool) {
	if l == nil || ttft <= 0 {
		return false
	}
	key := demotionKey(providerID, modelID)
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.entries[key]
	if !ok {
		s = &latencyStats{Key: key}
		l.entries[key] = s
	}
	ms := float64(ttft.Milliseconds())
	if s.Samples == 0 {
		s.TTFTemaMS = ms
	} else {
		s.TTFTemaMS = latencyEMAAlpha*ms + (1-latencyEMAAlpha)*s.TTFTemaMS
	}
	s.LastTTFTMS = int64(ms)
	s.Samples++
	s.LastUpdated = time.Now()
	if slow {
		s.SlowStreak++
	} else {
		s.SlowStreak = 0
	}
	if breakerTrips > 0 && s.SlowStreak >= breakerTrips {
		s.SlowStreak = 0
		s.BreakerTrips++
		return true
	}
	return false
}

// get returns a copy of one pair's stats.
func (l *latencyRegistry) get(providerID, modelID string) (latencyStats, bool) {
	if l == nil {
		return latencyStats{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.entries[demotionKey(providerID, modelID)]
	if !ok {
		return latencyStats{}, false
	}
	return *s, true
}

// snapshot returns all tracked pairs sorted by key.
func (l *latencyRegistry) snapshot() []latencyStats {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]latencyStats, 0, len(l.entries))
	for _, s := range l.entries {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// applyTTFTObservation is the shared round-10 sink for one measured TTFT:
// it records the observation in the latency registry and applies the
// slow-TTFT circuit breaker — when the consecutive-slow streak reaches
// fallback.ttft_breaker_trips the model joins the cross-request demotion
// registry (the unknown-400 registry: same TTL, same FIFO hand-off, same
// last-chance rule) so the NEXT request's ranking skips it until the TTL
// lapses. Both config values are read per call, so a hot reload takes
// effect immediately.
func applyTTFTObservation(l *latencyRegistry, d *demotionRegistry, fb config.FallbackConfig, providerID, modelID string, ttft time.Duration, forcedSlow bool) {
	if l == nil || ttft <= 0 {
		return
	}
	threshold := ttftSlowThresholdFrom(fb)
	slow := forcedSlow || ttft >= threshold
	tripped := l.recordTTFTObservation(providerID, modelID, ttft, slow, ttftBreakerTripsFrom(fb))
	if slow {
		log.Printf("latency: %s/%s ttft=%v (slow threshold %v) — slow observation recorded", providerID, modelID, ttft, threshold)
	}
	if !tripped {
		return
	}
	ttl := time.Duration(fb.Unknown400DemotionSeconds) * time.Second
	st, _ := l.get(providerID, modelID)
	log.Printf("latency: %s/%s slow-TTFT circuit breaker tripped (EMA %.0fms, trips=%d)", providerID, modelID, st.TTFTemaMS, st.BreakerTrips)
	if d != nil && ttl > 0 {
		d.demote(providerID, modelID, time.Now())
		log.Printf("fallback: %s/%s demoted for %s (slow-TTFT circuit breaker)", providerID, modelID, ttl)
	}
}

// isSlowCandidate reports whether the pair's measured EMA TTFT sits at or
// above the slow threshold with enough samples to trust it.
func (fc *FallbackChain) isSlowCandidate(providerID, modelID string) bool {
	if fc.Latencies == nil {
		return false
	}
	st, ok := fc.Latencies.get(providerID, modelID)
	if !ok || st.Samples < int64(ttftBreakerMinSamplesFrom(fc.Cfg.Fallback)) {
		return false
	}
	return st.TTFTemaMS >= float64(ttftSlowThresholdFrom(fc.Cfg.Fallback).Milliseconds())
}

// recordTTFT is the chain-side TTFT sink (strategy-chain walk: buffered
// calls and streaming attempts).
func (fc *FallbackChain) recordTTFT(providerID, modelID string, ttft time.Duration, forcedSlow bool) {
	applyTTFTObservation(fc.Latencies, fc.Demotions, fc.Cfg.Fallback, providerID, modelID, ttft, forcedSlow)
}

// recordTTFT is the Server-side TTFT sink — the direct/alias streaming
// routes measure through StreamProxy, which carries no FallbackChain. Same
// semantics, same shared registries.
func (s *Server) recordTTFT(providerID, modelID string, ttft time.Duration, forcedSlow bool) {
	if s.latencies == nil {
		return
	}
	cfg := s.cfg.Load()
	if cfg == nil {
		return
	}
	applyTTFTObservation(s.latencies, s.demotions, cfg.Fallback, providerID, modelID, ttft, forcedSlow)
}

// deprioritizeSlowRanked moves candidates whose measured EMA TTFT is at or
// above the slow threshold to the BACK of the already-ranked list (stable,
// never dropped): a free upstream that answers in 45s loses the first hop
// to every faster sibling WITHOUT any config change, while still serving
// as a last resort. Candidates are skipped — never dropped — so a walk
// through a fully-slow pool behaves exactly as before.
func (fc *FallbackChain) deprioritizeSlowRanked(ranked []strategy.RankedEntry) []strategy.RankedEntry {
	if fc.Latencies == nil || len(ranked) <= 1 {
		return ranked
	}
	fast := make([]strategy.RankedEntry, 0, len(ranked))
	var slow []strategy.RankedEntry
	for _, r := range ranked {
		if fc.isSlowCandidate(r.ProviderID, r.ModelID) {
			slow = append(slow, r)
			continue
		}
		fast = append(fast, r)
	}
	if len(fast) == 0 || len(slow) == 0 {
		return ranked
	}
	return append(fast, slow...)
}

// deprioritizeSlowUpstreams is the alias/pool-route twin of
// deprioritizeSlowRanked: that route walks []alias.Upstream, not ranked
// strategy entries. Same semantics — slow-EMA candidates move to the back,
// never dropped.
func (fc *FallbackChain) deprioritizeSlowUpstreams(ups []alias.Upstream) []alias.Upstream {
	if fc.Latencies == nil || len(ups) <= 1 {
		return ups
	}
	fast := make([]alias.Upstream, 0, len(ups))
	var slow []alias.Upstream
	for _, u := range ups {
		if fc.isSlowCandidate(u.ProviderID, u.ModelID) {
			slow = append(slow, u)
			continue
		}
		fast = append(fast, u)
	}
	if len(fast) == 0 || len(slow) == 0 {
		return ups
	}
	return append(fast, slow...)
}

// splitProviderModel splits a "provider/model" key for the debug handler.
func splitProviderModel(key string) (string, string) {
	p, m, _ := strings.Cut(key, "/")
	return p, m
}

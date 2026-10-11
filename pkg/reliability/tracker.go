// Package reliability tracks per-provider success rates using an
// exponential moving average (EMA).
package reliability

import (
	"sync"
	"time"
)

const (
	// emaAlpha is the smoothing factor for the EMA.
	// Higher = faster reaction to recent events, lower = more stable.
	emaAlpha = 0.1

	// defaultSuccessRate is the starting EMA for unknown providers.
	defaultSuccessRate = 1.0
)

// ProviderStats holds the reliability stats for one provider.
type ProviderStats struct {
	ProviderID  string    `json:"provider_id"`
	SuccessRate float64   `json:"success_rate"` // 0.0 – 1.0 EMA
	TotalCalls  int64     `json:"total_calls"`
	LastUpdated time.Time `json:"last_updated"`
}

// Tracker tracks per-provider reliability using EMA.
type Tracker struct {
	mu    sync.RWMutex
	stats map[string]*ProviderStats
}

// New creates an empty reliability Tracker.
func New() *Tracker {
	return &Tracker{stats: make(map[string]*ProviderStats)}
}

// Record adds an observation (success or failure) for the given provider.
func (t *Tracker) Record(providerID string, success bool) {
	if success {
		t.RecordOutcome(providerID, OutcomeSuccess)
		return
	}
	t.RecordOutcome(providerID, OutcomeRateLimited)
}

// Outcome classifies one observed upstream attempt for the EMA score
// (task M). Every outcome costs the upstream slot (a 1 request/minute/model
// quota), but they are not equally bad — the penalty ordering is
//
//	empty-200 / timeout  (slot burned, NO answer, no usable signal)
//	> 429/5xx + transport failures (slot burned, no answer, clear cause)
//	> slow success      (slot burned, but the answer was delivered)
//	> success.
type Outcome int

const (
	OutcomeSuccess Outcome = iota
	OutcomeSlowSuccess
	// OutcomeRateLimited: 429/5xx and generic transport failures — the
	// slot burned with no answer, but the cause is actionable.
	OutcomeRateLimited
	// OutcomeWasted: empty-200 and timeout — the slot burned with no
	// answer and no usable signal; the heaviest penalty.
	OutcomeWasted
)

// emaValue maps the outcome to the EMA observation (1.0 = perfect,
// 0.0 = worst). The weights make ShouldCooldown fire on repeated
// rate-limiting/wasted attempts (EMA < 0.5 over >= 5 calls) while an
// occasional slow success never cools a provider down.
func (o Outcome) emaValue() float64 {
	switch o {
	case OutcomeSuccess:
		return 1.0
	case OutcomeSlowSuccess:
		return 0.8
	case OutcomeRateLimited:
		return 0.3
	default: // OutcomeWasted
		return 0.0
	}
}

// RecordOutcome records one attempt with its penalty class (task M) — the
// EMA score the strategies rank candidates by.
func (t *Tracker) RecordOutcome(providerID string, outcome Outcome) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.getOrCreate(providerID)
	s.TotalCalls++
	s.LastUpdated = time.Now()
	s.SuccessRate = emaAlpha*outcome.emaValue() + (1-emaAlpha)*s.SuccessRate
}

// ShouldCooldown returns true when a provider's EMA failure rate exceeds 50%
// over at least 5 observed requests.
func (t *Tracker) ShouldCooldown(providerID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.stats[providerID]
	if !ok || s.TotalCalls < 5 {
		return false
	}
	return s.SuccessRate < 0.5
}

// SuccessRate returns the current EMA success rate for a provider.
// Returns defaultSuccessRate for unknown providers.
func (t *Tracker) SuccessRate(providerID string) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if s, ok := t.stats[providerID]; ok {
		return s.SuccessRate
	}
	return defaultSuccessRate
}

// All returns a snapshot of all provider stats.
func (t *Tracker) All() []ProviderStats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ProviderStats, 0, len(t.stats))
	for _, s := range t.stats {
		out = append(out, *s)
	}
	return out
}

// LoadFrom overwrites the tracker state with the given stats slice.
func (t *Tracker) LoadFrom(stats []ProviderStats) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range stats {
		cp := s
		t.stats[s.ProviderID] = &cp
	}
}

func (t *Tracker) getOrCreate(providerID string) *ProviderStats {
	if s, ok := t.stats[providerID]; ok {
		return s
	}
	s := &ProviderStats{
		ProviderID:  providerID,
		SuccessRate: defaultSuccessRate,
	}
	t.stats[providerID] = s
	return s
}

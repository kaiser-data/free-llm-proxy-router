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
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.getOrCreate(providerID)
	s.TotalCalls++
	s.LastUpdated = time.Now()

	val := 0.0
	if success {
		val = 1.0
	}
	s.SuccessRate = emaAlpha*val + (1-emaAlpha)*s.SuccessRate
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

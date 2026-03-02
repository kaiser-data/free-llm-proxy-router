package ratelimit

import (
	"sync"
	"time"
)

// Window is a fixed-size time bucket.
type Window struct {
	Count     int
	WindowEnd time.Time
	Size      time.Duration
}

// Add increments the count if the window hasn't expired; resets if it has.
// Returns true if the count was incremented (not exceeded the limit).
func (w *Window) Add(limit int) bool {
	now := time.Now()
	if now.After(w.WindowEnd) {
		w.Count = 0
		w.WindowEnd = now.Add(w.Size)
	}
	if limit > 0 && w.Count >= limit {
		return false
	}
	w.Count++
	return true
}

// Remaining returns the count of remaining slots in the current window.
func (w *Window) Remaining(limit int) int {
	if limit <= 0 {
		return -1
	}
	if time.Now().After(w.WindowEnd) {
		return limit
	}
	r := limit - w.Count
	if r < 0 {
		return 0
	}
	return r
}

// ProviderTracker tracks per-provider RPM, RPD, and optional RPS counters.
type ProviderTracker struct {
	mu sync.Mutex

	// perModel: model_id → per-minute window
	perModel map[string]*Window
	// perModelDay: model_id → per-day window
	perModelDay map[string]*Window
	// perModelSec: model_id → per-second window (Cerebras)
	perModelSec map[string]*Window

	// PreemptiveWaitUntil is set when Cerebras reports low remaining requests.
	preemptiveWaitUntil time.Time
}

// NewProviderTracker creates an empty ProviderTracker.
func NewProviderTracker() *ProviderTracker {
	return &ProviderTracker{
		perModel:    make(map[string]*Window),
		perModelDay: make(map[string]*Window),
		perModelSec: make(map[string]*Window),
	}
}

// CanRequest returns true if we should attempt a request for the given model.
// Pass rpmLimit/rpdLimit = 0 to skip that check.
func (t *ProviderTracker) CanRequest(modelID string, rpmLimit, rpdLimit int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Preemptive slowdown (Cerebras)
	if time.Now().Before(t.preemptiveWaitUntil) {
		return false
	}

	// RPM
	if rpmLimit > 0 {
		w := t.getOrCreate(t.perModel, modelID, time.Minute)
		if !w.Add(rpmLimit) {
			return false
		}
		// Undo the optimistic increment — CanRequest is a check, not a commit
		w.Count--
	}

	// RPD
	if rpdLimit > 0 {
		wd := t.getOrCreate(t.perModelDay, modelID, 24*time.Hour)
		if !wd.Add(rpdLimit) {
			return false
		}
		wd.Count--
	}

	return true
}

// RecordRequest increments counters for a model after successfully sending a request.
func (t *ProviderTracker) RecordRequest(modelID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Create windows if they don't exist yet (with a generous limit so they auto-init)
	t.getOrCreate(t.perModel, modelID, time.Minute).Add(0)
	t.perModel[modelID].Count++

	t.getOrCreate(t.perModelDay, modelID, 24*time.Hour).Add(0)
	t.perModelDay[modelID].Count++
}

// RecordRequestSec increments the per-second window (Cerebras).
func (t *ProviderTracker) RecordRequestSec(modelID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.getOrCreate(t.perModelSec, modelID, time.Second).Add(0)
	t.perModelSec[modelID].Count++
}

// PreemptiveSlowdown sets a wait-until time to prevent imminent rate-limiting
// (used by Cerebras when remaining-requests < 2).
func (t *ProviderTracker) PreemptiveSlowdown(waitDur time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	until := time.Now().Add(waitDur)
	if until.After(t.preemptiveWaitUntil) {
		t.preemptiveWaitUntil = until
	}
}

// ClearPreemptive removes any active preemptive slowdown.
func (t *ProviderTracker) ClearPreemptive() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.preemptiveWaitUntil = time.Time{}
}

func (t *ProviderTracker) getOrCreate(m map[string]*Window, key string, size time.Duration) *Window {
	if w, ok := m[key]; ok {
		return w
	}
	w := &Window{Size: size, WindowEnd: time.Now().Add(size)}
	m[key] = w
	return w
}

// GlobalTracker holds per-provider trackers, keyed by provider ID.
type GlobalTracker struct {
	mu        sync.RWMutex
	trackers  map[string]*ProviderTracker
	cooldowns map[string]time.Time // provider_id → earliest time to retry
}

// NewGlobalTracker creates an empty GlobalTracker.
func NewGlobalTracker() *GlobalTracker {
	return &GlobalTracker{
		trackers:  make(map[string]*ProviderTracker),
		cooldowns: make(map[string]time.Time),
	}
}

// Provider returns the ProviderTracker for the given provider, creating it if needed.
func (g *GlobalTracker) Provider(providerID string) *ProviderTracker {
	g.mu.RLock()
	t, ok := g.trackers[providerID]
	g.mu.RUnlock()
	if ok {
		return t
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok = g.trackers[providerID]; ok {
		return t
	}
	t = NewProviderTracker()
	g.trackers[providerID] = t
	return t
}

// SetCooldown records that a provider is rate-limited until the given time.
// Subsequent IsOnCooldown checks will return true until that time passes.
func (g *GlobalTracker) SetCooldown(providerID string, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Only extend cooldown, never shorten it.
	if existing, ok := g.cooldowns[providerID]; !ok || until.After(existing) {
		g.cooldowns[providerID] = until
	}
}

// IsOnCooldown returns true if the provider is still within its rate-limit cooldown window.
func (g *GlobalTracker) IsOnCooldown(providerID string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	until, ok := g.cooldowns[providerID]
	return ok && time.Now().Before(until)
}

// EarliestRecovery returns the earliest time any currently-cooled-down provider will recover.
// Returns zero time if no cooldowns are active.
func (g *GlobalTracker) EarliestRecovery() time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var earliest time.Time
	now := time.Now()
	for _, until := range g.cooldowns {
		if until.Before(now) {
			continue
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest
}

package proxy

import (
	"fmt"
	"sync"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// SlotLimiter reserves per-candidate RPM request slots (task L0).
//
// The upstream quota this protects is "N requests per minute per model"
// (UnoRouter: 1 request/minute/model), so the limiter keeps a sliding
// one-minute window of atomic reservations per candidate, keyed by
// "provider/model". Semantics the routes rely on:
//
//   - Reserve is atomic: concurrent arrivals either each claim a slot or
//     are told when the next one frees (oldest reservation + window, or the
//     429-driven block).
//   - A reserved slot is never released. A failed upstream attempt still
//     consumed the real quota, so it consumes the slot too.
//   - An upstream 429 adjusts next_available (AdjustNextAvailable): the
//     provider said "not until X", so no reservation is granted before
//     that even when the local window looks free.
//
// window is a fixed one minute in production; tests shorten it. All
// methods are safe for concurrent use.
type SlotLimiter struct {
	mu     sync.Mutex
	window time.Duration
	states map[string]*slotState
}

// slotState is the reservation ring of one candidate. reservations is kept
// ascending; blockedUntil is the hard block an upstream 429 installed.
type slotState struct {
	reservations []time.Time
	blockedUntil time.Time
}

// NewSlotLimiter creates an empty limiter with the production one-minute
// window.
func NewSlotLimiter() *SlotLimiter {
	return &SlotLimiter{window: time.Minute, states: map[string]*slotState{}}
}

// Reserve atomically claims one request slot for the candidate when its
// window is not full (and no 429 block is active). granted=false returns
// the earliest time a later attempt could be granted.
func (l *SlotLimiter) Reserve(key string, rpm int) (granted bool, nextAvailable time.Time) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.states[key]
	if st == nil {
		st = &slotState{}
		l.states[key] = st
	}
	l.pruneLocked(st, now)
	denyAt := time.Time{}
	if !st.blockedUntil.IsZero() && now.Before(st.blockedUntil) {
		// The upstream told us to come back later (429).
		denyAt = st.blockedUntil
	} else if rpm > 0 && len(st.reservations) >= rpm {
		// Window full: the oldest reservation leaves the window first.
		denyAt = st.reservations[0].Add(l.window)
	}
	if !denyAt.IsZero() {
		return false, denyAt
	}
	st.reservations = append(st.reservations, now)
	return true, now.Add(l.window)
}

// AdjustNextAvailable installs (or extends) the candidate's hard block
// until `until` — the 429 contract: only ever extended, never shortened.
func (l *SlotLimiter) AdjustNextAvailable(key string, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.states[key]
	if st == nil {
		st = &slotState{}
		l.states[key] = st
	}
	if until.After(st.blockedUntil) {
		st.blockedUntil = until
	}
}

// NextAvailable returns when the candidate can accept its next request;
// zero time when it is free right now (or unknown to the limiter).
func (l *SlotLimiter) NextAvailable(key string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.states[key]
	if st == nil {
		return time.Time{}
	}
	now := time.Now()
	l.pruneLocked(st, now)
	if !st.blockedUntil.IsZero() && now.Before(st.blockedUntil) {
		return st.blockedUntil
	}
	if len(st.reservations) == 0 {
		return time.Time{}
	}
	return st.reservations[0].Add(l.window)
}

// Earliest returns the soonest next_available across the candidate keys
// that the limiter knows. ok=false when none of the keys has state.
func (l *SlotLimiter) Earliest(keys []string) (at time.Time, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for _, k := range keys {
		st := l.states[k]
		if st == nil {
			continue
		}
		l.pruneLocked(st, now)
		var cand time.Time
		if !st.blockedUntil.IsZero() && now.Before(st.blockedUntil) {
			cand = st.blockedUntil
		} else if len(st.reservations) > 0 {
			cand = st.reservations[0].Add(l.window)
		}
		if cand.IsZero() {
			continue // free right now
		}
		if !ok || cand.Before(at) {
			at, ok = cand, true
		}
	}
	return at, ok
}

// pruneLocked drops reservations that fell out of the window and a 429
// block whose time has passed. Caller holds l.mu.
func (l *SlotLimiter) pruneLocked(st *slotState, now time.Time) {
	cutoff := now.Add(-l.window)
	keep := st.reservations[:0]
	for _, t := range st.reservations {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	st.reservations = keep
	if !st.blockedUntil.IsZero() && !now.Before(st.blockedUntil) {
		st.blockedUntil = time.Time{}
	}
}

// activeCount reports how many reservations the candidate currently holds —
// a test/observability helper, not part of the routing contract.
func (l *SlotLimiter) activeCount(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.states[key]
	if st == nil {
		return 0
	}
	l.pruneLocked(st, time.Now())
	return len(st.reservations)
}

// slotEnter is the one gate every route iteration applies before entering a
// candidate (task L0). When the provider opts into slot limiting
// (providers[].rpm_limit > 0) it atomically reserves the slot; a denied
// candidate is collected into `skips` for the group queue and the caller
// moves on without spending a hop or an attempt.
func slotEnter(l *SlotLimiter, provCfg *config.ProviderConfig, modelID string, keys *[]string) bool {
	if l == nil || provCfg == nil || provCfg.RPM <= 0 {
		return true
	}
	granted, _ := l.Reserve(provCfg.ID+"/"+modelID, provCfg.RPM)
	if granted {
		return true
	}
	*keys = append(*keys, provCfg.ID+"/"+modelID)
	return false
}

// errAllSlotsBusy is the walk's sentinel: the candidate list was fully
// skipped because every eligible upstream lacked an RPM slot — no upstream
// call was entered, so the request may legitimately wait for the next one
// to free instead of failing immediately. keys carries the skipped
// candidates, earliest the soonest any of them recovers.
type errAllSlotsBusy struct {
	keys     []string
	earliest time.Time
}

func (e *errAllSlotsBusy) Error() string {
	if e.earliest.IsZero() {
		return fmt.Sprintf("no upstream slot available for %d candidate(s) %v", len(e.keys), e.keys)
	}
	return fmt.Sprintf("no upstream slot available for %d candidate(s) %v — earliest %s", len(e.keys), e.keys, e.earliest.Format("15:04:05"))
}

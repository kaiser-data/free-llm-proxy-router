package proxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Slot-queue sentinels (task L0). A route turns them into the exhaustion
// response — 503 + Retry-After — or, when the keepalive header already
// went out, into the structured SSE error event.
var (
	// errQueueFull: the request arrived when the group queue was already at
	// fallback.queue_max_waiters — it never entered the queue.
	errQueueFull = errors.New("slot queue is full")
	// errQueueWaitTimeout: the request waited in the queue longer than
	// fallback.queue_wait_seconds without gaining a slot.
	errQueueWaitTimeout = errors.New("slot queue wait timed out")
	// errClientGone: the downstream client disconnected while waiting —
	// the waiter left the queue immediately; there is nobody to answer.
	errClientGone = errors.New("client disconnected while waiting for a slot")
)

// queueGroup is the FIFO queue identity of a request (task L0): the client
// model name — pool alias, canonical alias, or a specific model — so every
// interchangeable request for the same target waits in one line; unnamed
// ("auto"/strategy) requests share the "auto" queue.
func queueGroup(req Request) string {
	g := strings.ToLower(strings.TrimSpace(req.Model))
	if g == "" {
		return "auto"
	}
	return g
}

// queueWaiter is one request waiting for a slot. The manager closes turn
// exactly once — after removing the waiter from the queue — to hand it its
// turn; the waiter otherwise selects on its own deadline and the request
// context.
type queueWaiter struct {
	turn     chan struct{}
	keys     []string  // candidate keys this waiter may walk
	earliest time.Time // fallback wake time when the limiter has no state
	senior   bool      // re-queued after a served walk — keeps its place
}

// routerQueue is one group's FIFO line plus its single wake timer. There
// is no polling: the timer is armed at the earliest next_available among
// the head waiter's candidates and re-armed whenever the line changes.
type routerQueue struct {
	waiters []*queueWaiter
	timer   *time.Timer
}

// queueManager owns the per-group queues and the slot limiter they wake
// against. Safe for concurrent use.
type queueManager struct {
	mu      sync.Mutex
	limiter *SlotLimiter
	queues  map[string]*routerQueue
}

func newQueueManager(l *SlotLimiter) *queueManager {
	return &queueManager{limiter: l, queues: map[string]*routerQueue{}}
}

// wait enqueues the request into its group's FIFO line and blocks until
// one of:
//
//   - its turn (the manager removed it from the line first) → (true, nil);
//   - budget elapsed without a slot → (false, errQueueWaitTimeout);
//   - the request context canceled (client gone / budget dead) →
//     (false, errClientGone) — the waiter is gone from the line either way,
//     no slot is held and no goroutine lingers;
//   - the line was already at maxWaiters → (false, errQueueFull) without
//     ever entering.
//
// senior re-queues the waiter at the HEAD of the line: a request that was
// already handed a turn and came back without a slot keeps its seniority
// over newer arrivals.
func (m *queueManager) wait(ctx context.Context, group string, keys []string, earliest time.Time, budget time.Duration, maxWaiters int, senior bool) (bool, error) {
	if len(keys) == 0 {
		return false, errors.New("wait called with no candidate keys")
	}
	w := &queueWaiter{turn: make(chan struct{}), keys: keys, earliest: earliest}
	waiterTimer := time.NewTimer(budget)
	defer waiterTimer.Stop()

	m.mu.Lock()
	q := m.queues[group]
	if q == nil {
		q = &routerQueue{}
		m.queues[group] = q
	}
	if len(q.waiters) >= maxWaiters {
		m.mu.Unlock()
		return false, errQueueFull
	}
	if senior && len(q.waiters) > 0 {
		// Re-queue at the head: seniority over newer arrivals.
		q.waiters = append([]*queueWaiter{w}, q.waiters...)
	} else {
		q.waiters = append(q.waiters, w)
	}
	m.armLocked(group, q)
	m.mu.Unlock()

	select {
	case <-w.turn:
		return true, nil
	case <-waiterTimer.C:
		m.remove(group, w)
		return false, errQueueWaitTimeout
	case <-ctx.Done():
		m.remove(group, w)
		return false, errClientGone
	}
}

// len returns the current waiter count of one group (test/ops helper).
func (m *queueManager) len(group string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q := m.queues[group]; q != nil {
		return len(q.waiters)
	}
	return 0
}

// remove drops the waiter from its line (timeout/disconnect). Removing the
// head re-arms the timer for the next head. A waiter that was already
// popped for its turn is not found — the remove is a no-op.
func (m *queueManager) remove(group string, w *queueWaiter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[group]
	if q == nil {
		return
	}
	for i, cand := range q.waiters {
		if cand == w {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			if i == 0 {
				m.armLocked(group, q)
			}
			return
		}
	}
}

// armLocked (re)arms the group's wake timer at the earliest next_available
// among the HEAD waiter's candidates — the only waiter that may proceed —
// falling back to the head's recorded earliest. An empty line disarms the
// timer. Caller holds m.mu.
func (m *queueManager) armLocked(group string, q *routerQueue) {
	if q.timer != nil {
		q.timer.Stop()
		q.timer = nil
	}
	if len(q.waiters) == 0 {
		return
	}
	head := q.waiters[0]
	at, ok := m.limiter.Earliest(head.keys)
	if !ok || at.IsZero() {
		at = head.earliest
	}
	if at.IsZero() {
		at = time.Now()
	}
	delay := time.Until(at)
	if delay < 0 {
		delay = 0
	}
	q.timer = time.AfterFunc(delay, func() { m.onFire(group) })
}

// onFire wakes the head waiter: removes it from the line, arms the timer
// for the next head (parallel walkers stay FIFO by entry order), then
// closes the head's turn channel. A fire on an empty line is a no-op.
func (m *queueManager) onFire(group string) {
	m.mu.Lock()
	q := m.queues[group]
	if q == nil || len(q.waiters) == 0 {
		m.mu.Unlock()
		return
	}
	head := q.waiters[0]
	q.waiters = q.waiters[1:]
	m.armLocked(group, q)
	m.mu.Unlock()
	close(head.turn)
}

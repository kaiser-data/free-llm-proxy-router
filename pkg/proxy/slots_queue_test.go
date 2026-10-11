package proxy

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSlotLimiterReserveAtomicAndNextAvailable locks the limiter contract:
// rpm slots are claimed atomically, a full window denies with the earliest
// recovery time (oldest reservation + window), and the window reopens when
// the oldest reservation falls out.
func TestSlotLimiterReserveAtomicAndNextAvailable(t *testing.T) {
	l := NewSlotLimiter()
	l.window = 120 * time.Millisecond

	g1, _ := l.Reserve("tp/m", 2)
	g2, _ := l.Reserve("tp/m", 2)
	if !g1 || !g2 {
		t.Fatalf("expected first two reservations granted, got %v %v", g1, g2)
	}
	g3, at := l.Reserve("tp/m", 2)
	if g3 {
		t.Fatal("third reservation should be denied when the window is full")
	}
	if at.IsZero() {
		t.Fatal("denied reservation must return the earliest recovery time")
	}
	if active := l.activeCount("tp/m"); active != 2 {
		t.Fatalf("expected 2 active reservations, got %d", active)
	}
	time.Sleep(150 * time.Millisecond)
	g4, _ := l.Reserve("tp/m", 2)
	if !g4 {
		t.Fatal("reservation should be granted after the window rolled over")
	}
}

// TestSlotLimiter429AdjustsNextAvailable: an upstream 429 blocks
// reservations until the provider said so — even when the local window is
// free — and the block only ever extends.
func TestSlotLimiter429AdjustsNextAvailable(t *testing.T) {
	l := NewSlotLimiter()
	l.window = 20 * time.Millisecond // window free almost immediately

	until := time.Now().Add(180 * time.Millisecond)
	l.AdjustNextAvailable("tp/m", until)
	g, at := l.Reserve("tp/m", 5)
	if g {
		t.Fatal("reservation must be denied while the 429 block is active")
	}
	if !at.Equal(until) {
		t.Fatalf("denied reservation should recover at the 429 time: got %s want %s", at.Format("15:04:05.000"), until.Format("15:04:05.000"))
	}
	// Extending wins; shortening is ignored.
	l.AdjustNextAvailable("tp/m", time.Now().Add(10*time.Millisecond))
	if na := l.NextAvailable("tp/m"); !na.Equal(until) {
		t.Fatalf("a shorter AdjustNextAvailable must not shorten the block: got %s", na.Format("15:04:05.000"))
	}
	time.Sleep(200 * time.Millisecond)
	g2, _ := l.Reserve("tp/m", 5)
	if !g2 {
		t.Fatal("reservation should be granted after the 429 block expired")
	}
}

// TestSlotLimiterFailedAttemptConsumesSlot: there is no release — a
// reserved slot stays reserved (the real upstream quota was burned),
// which is exactly why a full window denies until the oldest booking ages
// out.
func TestSlotLimiterFailedAttemptConsumesSlot(t *testing.T) {
	l := NewSlotLimiter()
	l.window = time.Minute

	l.Reserve("tp/m", 1)
	g, _ := l.Reserve("tp/m", 1)
	if g {
		t.Fatal("second reservation must be denied at rpm=1")
	}
	if active := l.activeCount("tp/m"); active != 1 {
		t.Fatalf("the denied attempt must not add a reservation, got %d", active)
	}
	if na := l.NextAvailable("tp/m"); na.Before(time.Now().Add(50 * time.Second)) {
		t.Fatalf("next_available should be ~one window away, got %s", na.Format("15:04:05"))
	}
}

// TestSlotQueueFIFOHandsTurnsInEnqueueOrder: N concurrent requests waiting
// on M occupied slots are handed their turns strictly in enqueue order —
// each handed walker re-reserves the (rpm=1) slot, so the next head wakes
// only when that booking ages out of the window. The waiters are parked
// one at a time so the enqueue order is deterministic.
func TestSlotQueueFIFOHandsTurnsInEnqueueOrder(t *testing.T) {
	l := NewSlotLimiter()
	l.window = 300 * time.Millisecond
	m := newQueueManager(l)

	// Occupy the single slot so every waiter queues.
	l.Reserve("tp/m", 1)

	var mu sync.Mutex
	var served []int
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ok, err := m.wait(context.Background(), "g", []string{"tp/m"}, time.Time{}, 10*time.Second, 10, false)
			if !ok {
				mu.Lock()
				t.Logf("waiter %d not served: %v", n, err)
				mu.Unlock()
				return
			}
			l.Reserve("tp/m", 1) // the re-walk's atomic reservation
			mu.Lock()
			served = append(served, n)
			mu.Unlock()
		}(i)
		// Park waiter n before starting n+1 — deterministic enqueue order.
		deadline := time.Now().Add(2 * time.Second)
		for m.len("g") < i+1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	wg.Wait()

	if len(served) != 4 {
		t.Fatalf("expected all 4 waiters served, got %v", served)
	}
	for i, n := range served {
		if n != i {
			t.Fatalf("turns must be handed in FIFO order: got %v", served)
		}
	}
	if m.len("g") != 0 {
		t.Fatalf("queue should drain, %d waiters left", m.len("g"))
	}
}

// TestSlotQueueClientDisconnectLeavesQueueImmediately: a client that cuts
// the connection while waiting leaves the queue at once, holds no slot,
// and leaves no goroutine behind (the goroutine count settles back to the
// pre-call level — the same bar task J set for the hold).
func TestSlotQueueClientDisconnectLeavesQueueImmediately(t *testing.T) {
	l := NewSlotLimiter()
	l.window = time.Minute // nothing recovers during the test
	m := newQueueManager(l)
	l.Reserve("tp/m", 1) // occupy the only slot

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = m.wait(ctx, "g", []string{"tp/m"}, time.Time{}, time.Minute, 10, false)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for m.len("g") < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting request did not exit after client disconnect")
	}
	if m.len("g") != 0 {
		t.Fatalf("disconnected waiter must leave the queue, %d left", m.len("g"))
	}
	if active := l.activeCount("tp/m"); active != 1 {
		t.Fatalf("the departed waiter must not hold a slot, active=%d", active)
	}
	// No lingering goroutines: count settles EXACTLY back to baseline.
	settled := false
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() == before {
			settled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
	}
}

// TestSlotQueueWaitTimeout: a request whose queue budget expires gets the
// wait-timeout sentinel and leaves the queue.
func TestSlotQueueWaitTimeout(t *testing.T) {
	l := NewSlotLimiter()
	l.window = time.Minute
	m := newQueueManager(l)
	l.Reserve("tp/m", 1)

	ok, err := m.wait(context.Background(), "g", []string{"tp/m"}, time.Time{}, 60*time.Millisecond, 10, false)
	if ok || !errors.Is(err, errQueueWaitTimeout) {
		t.Fatalf("expected wait-timeout sentinel, got ok=%v err=%v", ok, err)
	}
	if m.len("g") != 0 {
		t.Fatalf("timed-out waiter must leave the queue, %d left", m.len("g"))
	}
}

// TestSlotQueueFullRejectsImmediately: a request arriving on a full queue
// gets errQueueFull without ever entering the line.
func TestSlotQueueFullRejectsImmediately(t *testing.T) {
	l := NewSlotLimiter()
	l.window = time.Minute
	m := newQueueManager(l)
	l.Reserve("tp/m", 1)

	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ { // maxWaiters = 2: both park
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.wait(context.Background(), "g", []string{"tp/m"}, time.Time{}, 5*time.Second, 2, false)
			<-release
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.len("g") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ok, err := m.wait(context.Background(), "g", []string{"tp/m"}, time.Time{}, 5*time.Second, 2, false)
	if ok || !errors.Is(err, errQueueFull) {
		t.Fatalf("expected queue-full sentinel, got ok=%v err=%v", ok, err)
	}
	close(release)
	wg.Wait()
}


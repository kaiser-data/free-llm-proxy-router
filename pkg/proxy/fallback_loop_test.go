package proxy

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// --- Task AA: the loop-until-deadline walk. The deadline is the primary
// stop; transiently failed candidates re-enter after their park TTL;
// fail-fast classes end the walk; the client never receives an empty
// completion (the task-W contract holds in every round).

// loopHandler scripts one upstream: bodies[i] serves the i-th hit (the last
// entry repeats). Each entry is a full response: "status|body".
type loopScript []string

func loopServer(script loopScript) (*httptest.Server, *int32) {
	var hits int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&hits, 1)) - 1
		if i >= len(script) {
			i = len(script) - 1
		}
		parts := strings.SplitN(script[i], "|", 2)
		status := http.StatusOK
		if len(parts) == 2 {
			fmt.Sscanf(parts[0], "%d", &status)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, parts[1])
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, script[i])
	})), &hits
}

// loopChain builds a buffered chain over the ordered (provider, server)
// pairs with loop-mode knobs.
type loopPair struct {
	id  string
	url string
	rpm int // 0 = unlimited
}

func loopChain(t *testing.T, pairs []loopPair, fb config.FallbackConfig) *FallbackChain {
	t.Helper()
	cfg := &config.Config{Fallback: fb}
	providers := make([]config.ProviderConfig, len(pairs))
	entries := make([]strategy.RankedEntry, len(pairs))
	catEntries := make([]catalog.CatalogEntry, len(pairs))
	for i, p := range pairs {
		providers[i] = config.ProviderConfig{ID: p.id, BaseURL: p.url, Enabled: true, RPM: p.rpm}
		entries[i] = strategy.RankedEntry{ProviderID: p.id, ModelID: "model-" + p.id}
		catEntries[i] = catalog.CatalogEntry{ProviderID: p.id, ModelID: "model-" + p.id, IsFree: true, TierType: "free"}
	}
	cfg.Providers = providers
	if cfg.Fallback.MaxAttempts <= 0 {
		cfg.Fallback.MaxAttempts = 3
	}
	if cfg.Fallback.MaxFallbackHops <= 0 {
		cfg.Fallback.MaxFallbackHops = 40
	}
	return &FallbackChain{
		Cfg:                cfg,
		Strategy:           &multiMockStrategy{entries: entries},
		Catalog:            &catalog.Catalog{Entries: catEntries},
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
	}
}

// loopRequest is the buffered chain request for the loop tests.
func loopRequest() Request {
	return Request{
		Model:       "model-pa",
		ClientModel: "model-pa",
		Messages:    []map[string]any{{"role": "user", "content": "hi"}},
		Raw:         map[string]any{"model": "model-pa", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}
}

// loopFB is the loop-mode FallbackConfig: the deadline is the primary stop,
// a tiny cooldown between rounds, a high hop ceiling.
func loopFB(deadlineSeconds int) config.FallbackConfig {
	return config.FallbackConfig{
		LoopUntilDeadline:    true,
		RoundCooldownMs:      5,
		TotalDeadlineSeconds: deadlineSeconds,
		MaxAttempts:          3,
		MaxFallbackHops:      40,
		RetriesPerCandidate:  0,
	}
}

// TestLoopUntilDeadlineGetsAnswerAfterTransientFailures: both candidates
// fail transiently in the first rounds (empty, then 503) — the loop parks
// them, cools down, and re-enters them; when the scripted upstream recovers
// the client gets the REAL answer. The invariant: only a real answer leaves
// the loop.
func TestLoopUntilDeadlineGetsAnswerAfterTransientFailures(t *testing.T) {
	var hitsA int32
	hA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer hA.Close()
	srvB, hitsB := loopServer(loopScript{"503|{\"error\":{\"message\":\"overloaded\"}}", "503|{\"error\":{\"message\":\"overloaded\"}}", okBody("b")})
	defer srvB.Close()

	fc := loopChain(t, []loopPair{{id: "pa", url: hA.URL}, {id: "pb", url: srvB.URL}}, loopFB(10))
	resp, err := fc.Execute(context.Background(), loopRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want provider-b's real answer after the loop", err)
	}
	if !strings.Contains(string(resp.Body), "ANSWER_B") {
		t.Fatalf("client body %q — want ANSWER_B", resp.Body)
	}
	if got := atomic.LoadInt32(&hitsA); got < 2 {
		t.Errorf("provider-a hits = %d, want >= 2 (the empty candidate re-enters the loop after its park TTL)", got)
	}
	if got := atomic.LoadInt32(hitsB); got != 3 {
		t.Errorf("provider-b hits = %d, want 3 (two 503 rounds + the answer)", got)
	}
}

// TestLoopExhaustsIntoClearError: when NOTHING can answer before the
// deadline the loop ends with the clear budget error (the caller answers
// 503 + Retry-After envelope) — never a 200, never a fake message, and no
// goroutine stays behind.
func TestLoopExhaustsIntoClearError(t *testing.T) {
	srvA, _ := loopServer(loopScript{`{"choices":[]}`})
	defer srvA.Close()
	srvB, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvB.Close()

	fc := loopChain(t, []loopPair{{id: "pa", url: srvA.URL}, {id: "pb", url: srvB.URL}}, loopFB(1))
	before := runtime.NumGoroutine()
	start := time.Now()
	_, err := fc.Execute(context.Background(), loopRequest())
	dur := time.Since(start)
	if err == nil {
		t.Fatal("Execute error = nil, want the clear budget error")
	}
	if !strings.Contains(err.Error(), "request budget exhausted") {
		t.Errorf("error = %v, want the request budget exhaustion message", err)
	}
	if dur < 900*time.Millisecond {
		t.Errorf("the loop gave up after %v — the deadline must be the primary stop", dur)
	}
	if dur > 5*time.Second {
		t.Errorf("the loop ran %v — way past the 1s deadline", dur)
	}
	fc.HTTPClient.CloseIdleConnections() // release the idle keep-alive conns
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() == before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines did not settle back to %d: now=%d — possible leak", before, runtime.NumGoroutine())
}

// TestLoopRespectsHopCeiling: max_fallback_hops is the anti-corruption
// ceiling across ALL rounds — with many candidates failing transiently the
// loop aborts at the ceiling (fast), not at the deadline, with the clear
// hop-ceiling error.
func TestLoopRespectsHopCeiling(t *testing.T) {
	fb := loopFB(30)
	fb.MaxFallbackHops = 3
	fb.MaxAttempts = 9
	var servers []*httptest.Server
	var pairs []loopPair
	for _, id := range []string{"pa", "pb", "pc"} {
		srv, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
		defer srv.Close()
		servers = append(servers, srv)
		pairs = append(pairs, loopPair{id: id, url: srv.URL})
	}
	fc := loopChain(t, pairs, fb)
	start := time.Now()
	_, err := fc.Execute(context.Background(), loopRequest())
	dur := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "hop ceiling") {
		t.Fatalf("error = %v, want the hop-ceiling abort", err)
	}
	if dur > 5*time.Second {
		t.Errorf("the ceiling aborted after %v — it must stop the loop well before the deadline", dur)
	}
}

// TestLastResortTierEngages: with the tier configured, the loop's final
// seconds engage the last-resort provider — the REAL answer is delivered,
// the WARN log fires, and the cost counter moves.
func TestLastResortTierEngages(t *testing.T) {
	logs := captureLogs(t)
	srvA, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvA.Close()
	srvB, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvB.Close()
	rescue, rescueHits := loopServer(loopScript{okBody("rescue")})
	defer rescue.Close()

	fb := loopFB(2)
	fb.LastResort = config.LastResortConfig{Provider: "rescue", Model: "model-rescue", WhenSeconds: 1}
	fc := loopChain(t, []loopPair{{id: "pa", url: srvA.URL}, {id: "pb", url: srvB.URL}}, fb)
	// The rescue tier is NOT a ranked candidate — only findProvider reaches
	// it (the last-resort tier is never part of the normal walk).
	fc.Cfg.Providers = append(fc.Cfg.Providers, config.ProviderConfig{ID: "rescue", BaseURL: rescue.URL, Enabled: true})

	resp, err := fc.Execute(context.Background(), loopRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want the last-resort answer", err)
	}
	if !strings.Contains(string(resp.Body), "ANSWER_RESCUE") {
		t.Fatalf("client body %q — want the last-resort answer", resp.Body)
	}
	if got := atomic.LoadInt32(rescueHits); got != 1 {
		t.Errorf("rescue hits = %d, want 1", got)
	}
	if lastResortUseCount() == 0 {
		t.Error("the last-resort counter did not move")
	}
	out := logs.String()
	if !strings.Contains(out, "WARN last-resort engaged") {
		t.Errorf("missing the WARN engagement log:\n%s", out)
	}
	if !strings.Contains(out, "last-resort delivered") {
		t.Errorf("missing the delivery log:\n%s", out)
	}
}

// TestLastResortSkippedWhenUnset: no provider configured → the tier never
// engages (no warn, no counter movement) and the deadline still ends in the
// clear error.
func TestLastResortSkippedWhenUnset(t *testing.T) {
	logs := captureLogs(t)
	srvA, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvA.Close()
	srvB, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvB.Close()

	fb := loopFB(1) // LastResort stays zero — the tier is off
	fc := loopChain(t, []loopPair{{id: "pa", url: srvA.URL}, {id: "pb", url: srvB.URL}}, fb)

	_, err := fc.Execute(context.Background(), loopRequest())
	if err == nil || !strings.Contains(err.Error(), "request budget exhausted") {
		t.Fatalf("error = %v, want the budget error (503 + Retry-After is the final result without last_resort)", err)
	}
	if strings.Contains(logs.String(), "last-resort") {
		t.Errorf("the disabled tier must stay silent:\n%s", logs.String())
	}
}

// TestLastResortEmptyResultNeverDelivered: the tier obeys the SAME emptiness
// rules — a scripted empty answer from the tier fails and the loop continues
// to the clear deadline error (no 200, ever).
func TestLastResortEmptyResultNeverDelivered(t *testing.T) {
	srvA, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvA.Close()
	srvB, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvB.Close()
	rescue, _ := loopServer(loopScript{`{"choices":[]}`})
	defer rescue.Close()

	fb := loopFB(1)
	fb.LastResort = config.LastResortConfig{Provider: "rescue", Model: "model-rescue", WhenSeconds: 1}
	fc := loopChain(t, []loopPair{{id: "pa", url: srvA.URL}, {id: "pb", url: srvB.URL}}, fb)
	// Same as the engagement test — the tier is outside the ranking.
	fc.Cfg.Providers = append(fc.Cfg.Providers, config.ProviderConfig{ID: "rescue", BaseURL: rescue.URL, Enabled: true})

	resp, err := fc.Execute(context.Background(), loopRequest())
	if err == nil {
		t.Fatalf("got resp %q with err=nil — the empty last-resort result must NEVER be delivered", resp)
	}
	if !strings.Contains(err.Error(), "request budget exhausted") {
		t.Errorf("error = %v, want the budget error", err)
	}
}

// TestLoopFailFastDoesNotLoop: a genuine payload defect (unrecognized 400)
// surfaces immediately — no loop rounds, no second candidate, no park.
func TestLoopFailFastDoesNotLoop(t *testing.T) {
	srvA, hitsA := loopServer(loopScript{`400|{"error":{"message":"invalid type for tool_choice"}}`})
	defer srvA.Close()
	srvB, hitsB := loopServer(loopScript{okBody("b")})
	defer srvB.Close()

	fc := loopChain(t, []loopPair{{id: "pa", url: srvA.URL}, {id: "pb", url: srvB.URL}}, loopFB(10))
	start := time.Now()
	resp, err := fc.Execute(context.Background(), loopRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want the surfaced 400", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 surfaced as-is", resp.StatusCode)
	}
	if got := atomic.LoadInt32(hitsB); got != 0 {
		t.Errorf("provider-b hits = %d, want 0 (fail-fast — the loop must not continue)", got)
	}
	if got := atomic.LoadInt32(hitsA); got != 1 {
		t.Errorf("provider-a hits = %d, want 1 (no retry on a payload defect)", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("fail-fast took %v — it must surface immediately", time.Since(start))
	}
}

// TestLoopClientDisconnectStopsImmediately: a client that disconnects
// mid-loop stops the walk at once — no further rounds, no extra slot
// reservations, and the goroutine count settles back.
func TestLoopClientDisconnectStopsImmediately(t *testing.T) {
	srvA, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvA.Close()
	srvB, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvB.Close()

	fc := loopChain(t, []loopPair{{id: "pa", url: srvA.URL}, {id: "pb", url: srvB.URL}}, loopFB(30))
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fc.Execute(ctx, loopRequest())
		done <- err
	}()
	time.Sleep(150 * time.Millisecond) // let a round or two run
	cancel()                           // the client disconnects

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Execute error = nil after the client disconnected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Execute did not return within 3s of the disconnect — the loop leaked")
	}
	fc.HTTPClient.CloseIdleConnections() // release the idle keep-alive conns
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() == before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines did not settle back to %d: now=%d — possible leak", before, runtime.NumGoroutine())
}

// TestLoopSlotsRecover: with the slot limiter wired (short test window), the
// loop's reservations always expire back to zero — no slot stays held after
// the walk ends (the reservation ring prunes by the sliding window).
func TestLoopSlotsRecover(t *testing.T) {
	srvA, _ := loopServer(loopScript{`503|{"error":{"message":"overloaded"}}`})
	defer srvA.Close()

	fb := loopFB(1)
	slots := &SlotLimiter{window: 60 * time.Millisecond, states: map[string]*slotState{}}
	fb.MaxAttempts = 2
	fc := loopChain(t, []loopPair{{id: "pa", url: srvA.URL, rpm: 1}}, fb)
	fc.Slots = slots

	_, err := fc.Execute(context.Background(), loopRequest())
	if err == nil {
		t.Fatal("Execute error = nil, want the budget error (rpm=1 with a 503-ing upstream)")
	}
	// After the window lapses every reservation is pruned — nothing is held.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := slots.activeCount("pa/model-pa"); got == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("slot reservations never returned to 0: active=%d — a slot is stuck", slots.activeCount("pa/model-pa"))
}

// propExpect is the per-round expected outcome the generator computes.
type propExpect int

const (
	propExpectAnswer      propExpect = iota // a 200 with the answerable marker
	propExpectSurfaced400                   // fail-fast surfaced 400 (payload defect ranked first)
	propExpectError                         // the clear budget/hop-ceiling error
)

// TestPropertyLoopNeverDeliversEmptyCompletion: the property test (round 11,
// task AA). Random upstream failure sequences — empty / slow / 5xx / 429 /
// 400 / disconnect — over many rounds; the invariant must hold EVERY round:
//
//  1. the client NEVER receives a 200 whose completion is empty;
//  2. when a candidate can answer before the deadline → the client gets it;
//  3. deadline exhausted (or the anti-corruption hop ceiling) → the clear
//     error only;
//  4. no goroutine stays behind.
//
// A 429 that parked the answerable model (per-model quota cooldown) counts
// as "no answer reachable" — the walk correctly moves on and ends in the
// clear error; a 400 payload defect ranked before the answerable candidate
// fail-fasts and surfaces (the standing policy — the loop never re-tries a
// fail-fast class).
func TestPropertyLoopNeverDeliversEmptyCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("property test — skipped in -short")
	}
	rng := rand.New(rand.NewSource(20261014))
	const rounds = 1000
	before := runtime.NumGoroutine()

	transientForms := []string{"empty", "503", "429", "slow", "hang"}
	pick := func() string { return transientForms[rng.Intn(len(transientForms))] }

	for round := 0; round < rounds; round++ {
		if testing.Verbose() && round%100 == 0 {
			t.Logf("progress: round %d", round)
		}
		n := 1 + rng.Intn(3)
		type candPlan struct {
			id      string
			form    string // transient failure form of every hit (or of the pre-answer hits)
			failK   int    // hits that fail before the answer (answerable only)
			bad400  bool
			answers bool
		}
		plans := make([]candPlan, n)
		answerableIdx := -1
		if rng.Float64() < 0.5 {
			answerableIdx = rng.Intn(n)
		}
		for i := range plans {
			plans[i].id = fmt.Sprintf("c%d", i)
			if i == answerableIdx {
				plans[i].answers = true
				plans[i].failK = rng.Intn(2) // 0 or 1 failing hit before the answer
				plans[i].form = pick()
				continue
			}
			switch r := rng.Float64(); {
			case r < 0.30:
				plans[i].form = "empty"
			case r < 0.60:
				plans[i].form = "503"
			case r < 0.70:
				plans[i].form = "429"
			case r < 0.80:
				plans[i].form = "slow"
			case r < 0.85:
				plans[i].form = "hang"
			default:
				plans[i].bad400 = true
			}
		}

		// Expected outcome (computed from the plans, checked below). The
		// ranked walk enters candidates IN ORDER until one succeeds — so a
		// bad400 candidate is reached in round 1 unless an earlier
		// candidate ANSWERS FIRST (failK=0). An answerable candidate with
		// failing hits recovers via the loop (park TTL) — unless its
		// failure form is 429, whose per-model quota cooldown outlives the
		// deadline (the model is parked; no answer reachable in-request).
		firstBad400 := -1
		for i, p := range plans {
			if p.bad400 {
				firstBad400 = i
				break
			}
		}
		expect := propExpectError
		var expectedMarker string
		if answerableIdx >= 0 {
			a := plans[answerableIdx]
			answersBeforeAny400 := firstBad400 == -1 || firstBad400 > answerableIdx
			switch {
			case a.failK == 0 && answersBeforeAny400:
				expect = propExpectAnswer // answers on the first hit, before any 400
			case a.failK > 0 && firstBad400 == -1 && a.form != "429":
				expect = propExpectAnswer // the loop recovers it after the park TTL
			}
			if expect == propExpectAnswer {
				expectedMarker = "ANSWER_" + strings.ToUpper(a.id)
			}
		}
		if expect == propExpectError && firstBad400 != -1 {
			expect = propExpectSurfaced400 // the walk reaches the 400 and fail-fasts
		}

		// Build the round's upstreams.
		var closers []func()
		pairs := make([]loopPair, n)
		for i, p := range plans {
			var hits int32
			id := p.id
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				k := int(atomic.AddInt32(&hits, 1)) - 1
				failNow := p.bad400
				if p.answers && k >= p.failK {
					// answered
				} else if p.answers {
					failNow = true // the scripted pre-answer failure hit
				} else {
					failNow = true // this candidate never answers
				}
				switch {
				case failNow && p.bad400:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"invalid type for tool_choice"}}`)
				case failNow && p.form == "503":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, `{"error":{"message":"overloaded"}}`)
				case failNow && p.form == "429":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
				case failNow && p.form == "slow":
					time.Sleep(30 * time.Millisecond)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"choices":[]}`)
				case failNow && p.form == "hang":
					// The proxy's 100ms client timeout aborts the attempt.
					// Self-terminate too: a handler blocked ONLY on
					// r.Context().Done() (no flushed headers, nothing read)
					// is not woken by a client abort on this stack, and
					// Server.Close waits for handlers forever.
					select {
					case <-r.Context().Done():
					case <-time.After(150 * time.Millisecond):
					}
				case failNow: // empty
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"choices":[]}`)
				default:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, okBody(id))
				}
			}))
			closers = append(closers, srv.Close)
			pairs[i] = loopPair{id: id, url: srv.URL}
		}

		// Run the round: loop mode ON, the ctx deadline is the primary stop.
		fb := config.FallbackConfig{
			LoopUntilDeadline: true,
			RoundCooldownMs:   2,
			MaxAttempts:       3,
			MaxFallbackHops:   40,
		}
		fc := loopChain(t, pairs, fb)
		fc.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond} // hangs abort as timeouts

		ctxTTL := 40 * time.Millisecond // hopeless rounds end at the deadline
		if expect == propExpectAnswer || expect == propExpectSurfaced400 {
			// The walk needs room to reach the answer (or the fail-fast 400)
			// past the scripted failing hits.
			ctxTTL = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), ctxTTL)
		resp, err := fc.Execute(ctx, loopRequest())
		cancel()
		fc.HTTPClient.CloseIdleConnections() // release the round's idle conns

		// The invariant, checked EVERY round.
		switch expect {
		case propExpectAnswer:
			if err != nil {
				t.Fatalf("round %d: error %v, want the answer %q (plans: %+v)", round, err, expectedMarker, plans)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("round %d: status %d, want 200 (plans: %+v)", round, resp.StatusCode, plans)
			}
			if !strings.Contains(string(resp.Body), expectedMarker) {
				t.Fatalf("round %d: body %q, want %q — never an empty/fake completion", round, resp.Body, expectedMarker)
			}
		case propExpectSurfaced400:
			if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("round %d: (resp=%v, err=%v), want the surfaced 400 (plans: %+v)", round, resp, err, plans)
			}
		case propExpectError:
			if err == nil {
				t.Fatalf("round %d: got resp %q with err=nil — a hopeless round must end in the clear error (plans: %+v)", round, resp, plans)
			}
			if !strings.Contains(err.Error(), "request budget exhausted") && !strings.Contains(err.Error(), "hop ceiling") {
				t.Fatalf("round %d: error %q is not a clear budget/ceiling error (plans: %+v)", round, err, plans)
			}
		}

		for _, c := range closers {
			c()
		}
	}

	// No goroutine stays behind after 1000 rounds.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() == before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines did not settle back to %d: now=%d — possible leak", before, runtime.NumGoroutine())
}

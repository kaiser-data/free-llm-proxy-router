package proxy

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// The loop-until-deadline walk (round 11, task AA).
//
// Invariant (unchanged, task W): a client never receives an empty
// completion. This layer adds the OTHER half: the walk does not give up
// while it can still try — for a request in loop mode the total deadline
// (fallback.total_deadline_seconds) is the PRIMARY stop; every candidate
// that failed transiently (empty completion, timeout, 5xx, 429, transport)
// re-enters the loop when its per-request park TTL lapses; only a fail-fast
// (400 payload defect, content_filter, client gone) or a deadline/ceiling
// ends the request.
//
// Spin protection:
//   - a transiently failed candidate is PARKED for round_cooldown_ms —
//     skipped in the next ranking unless every candidate is parked (last
//     chance, never starve);
//   - a small cooldown separates rounds when the previous round tried
//     upstreams and all failed;
//   - max_fallback_hops stays as an anti-corruption ceiling counted across
//     ALL rounds (the walk seeds its hop counter from the loop state);
//     reaching it aborts with the exhaustion error.

// loopState is the per-request memory of the loop walk. Created per request
// (the chain is per-request too); all methods are safe for concurrent use.
type loopState struct {
	mu        sync.Mutex
	parked    map[string]time.Time // "provider/model" → park-until
	totalHops int
}

func newLoopState() *loopState {
	return &loopState{parked: make(map[string]time.Time)}
}

// park parks a transiently failed candidate until `until` (the round
// cooldown): the next ranking skips it so the loop tries candidates that
// have not just failed.
func (ls *loopState) park(key string, until time.Time) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.parked[key] = until
}

// parkedUntil reports the park TTL of one candidate; lazy-expired entries
// are dropped. ok=true means the candidate is still parked at `now`.
func (ls *loopState) parkedUntil(key string, now time.Time) (time.Time, bool) {
	if ls == nil {
		return time.Time{}, false
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	at, ok := ls.parked[key]
	if !ok {
		return time.Time{}, false
	}
	if !now.Before(at) {
		delete(ls.parked, key) // TTL lapsed — the candidate re-enters the loop
		return time.Time{}, false
	}
	return at, true
}

// earliestParked returns the soonest park TTL across all parked candidates
// (zero time when none are parked) — the loop sleeps at least that long so
// the next round can actually try someone.
func (ls *loopState) earliestParked(now time.Time) time.Time {
	if ls == nil {
		return time.Time{}
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	var earliest time.Time
	for k, at := range ls.parked {
		if !now.Before(at) {
			delete(ls.parked, k)
			continue
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest
}

// setHops records the walk's running hop total (the walk seeds its counter
// from here at the start of every round, so max_fallback_hops bounds the
// WHOLE request, not one round).
func (ls *loopState) setHops(n int) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	ls.totalHops = n
	ls.mu.Unlock()
}

// hops returns the recorded total.
func (ls *loopState) hops() int {
	if ls == nil {
		return 0
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.totalHops
}

// errHopCeiling marks the max_fallback_hops abort: in loop mode the loop
// must NOT continue past it — the ceiling exists to bound the damage of a
// broken pool, and retrying the walk would only re-reach it.
var errHopCeiling = errors.New("fallback hop ceiling reached")

// lastResortUses counts last-resort engagements process-wide (round 11):
// every engagement burns a real upstream call on a configured tier, so the
// cost must be observable. Exposed at GET /debug/latency
// ("last_resort_uses").
var lastResortUses atomic.Uint64

// lastResortUseCount returns the process-wide last-resort counter.
func lastResortUseCount() uint64 { return lastResortUses.Load() }

// loopNoDeadlineWarned fires once per process: loop_until_deadline without
// a total deadline is ignored (fail-closed), and the operator must see why.
var loopNoDeadlineWarned sync.Once

// loopUntilDeadline reports whether the chain walks in loop mode: the flag
// is on AND a deadline exists — either fallback.total_deadline_seconds (the
// walk wraps the ctx itself) or one the caller already installed on the
// request ctx (the handler's max_request ceiling, a test). Without ANY
// deadline the flag is ignored (fail-closed — an undying loop would burn
// quota forever).
func (fc *FallbackChain) loopUntilDeadline(ctx context.Context) bool {
	if !fc.Cfg.Fallback.LoopUntilDeadline {
		return false
	}
	if fc.totalDeadline() <= 0 {
		if _, ok := ctx.Deadline(); !ok {
			loopNoDeadlineWarned.Do(func() {
				log.Printf("WARN fallback.loop_until_deadline=true but no deadline exists (fallback.total_deadline_seconds=0 and the request ctx carries none) — the deadline is the loop's primary stop, so the flag is ignored (fail-closed)")
			})
			return false
		}
		return true // the ctx deadline rides as-is — do not wrap twice
	}
	return true
}

// roundCooldown returns the pause between loop rounds — also the park TTL
// of a transiently failed candidate (one knob, one rhythm). Default 1000ms
// for zeroed/negative configs; 0 is not honored so the loop can never spin.
func (fc *FallbackChain) roundCooldown() time.Duration {
	ms := fc.Cfg.Fallback.RoundCooldownMs
	if ms <= 0 {
		ms = 1000
	}
	return time.Duration(ms) * time.Millisecond
}

// filterLoopParked drops loop-parked candidates from the ranking unless
// EVERY candidate is parked — the last-chance rule the demotion registry
// uses (never starve a request when a parked upstream is all there is).
func (fc *FallbackChain) filterLoopParked(ranked []strategy.RankedEntry) []strategy.RankedEntry {
	if fc.Loop == nil || len(ranked) == 0 {
		return ranked
	}
	now := time.Now()
	kept := make([]strategy.RankedEntry, 0, len(ranked))
	var parkedEntries []strategy.RankedEntry
	for _, r := range ranked {
		if _, busy := fc.Loop.parkedUntil(demotionKey(r.ProviderID, r.ModelID), now); busy {
			parkedEntries = append(parkedEntries, r)
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return parkedEntries // every candidate parked — last chance beats starving
	}
	return kept
}

// transientFailure reports whether a failed attempt is the kind a loop
// round may retry after the park TTL: empty completions, timeouts, transport
// errors, 5xx and 429. Fail-fast classes (surfaced upstream responses, the
// content_filter refusal) never park — they end the request instead.
func transientFailure(resp *Response, err error) bool {
	if errors.Is(err, errEmptyCompletion) {
		return true
	}
	if err != nil {
		// Transport error / timeout (canceled contexts are handled by the
		// caller before this runs).
		return true
	}
	if resp == nil {
		return true
	}
	return resp.StatusCode >= 500 || resp.StatusCode == 429
}

// hopOK consumes one candidate hop against the ceiling; false means the
// ceiling blocks. With the loop state the counter spans all rounds.
func (fc *FallbackChain) hopOK(hops *int, maxHops int) bool {
	if *hops >= maxHops {
		return false
	}
	*hops++
	if fc.Loop != nil {
		fc.Loop.setHops(*hops)
	}
	return true
}

// sleepRound waits the cooldown between loop rounds — at least until the
// earliest parked candidate recovers (so the next round can try someone),
// never past the request's remaining budget. false = the client/budget is
// gone; stop immediately, reserve no slots.
func (fc *FallbackChain) sleepRound(ctx context.Context, reqID string) bool {
	wait := fc.roundCooldown()
	if at := fc.Loop.earliestParked(time.Now()); !at.IsZero() {
		if d := time.Until(at); d > wait {
			wait = d
		}
	}
	if rem := remainingBudget(ctx); rem > 0 && wait > rem {
		wait = rem
	}
	if wait <= 0 {
		return ctx.Err() == nil
	}
	log.Printf("loop: req=%s all candidates failed transiently — cooldown %v before the next round", reqID, wait)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}

// lastResortBody builds the last-resort request body: the same shaping the
// ranked walk applies (model override, stream stripped, Anthropic fields
// stripped) plus the context truncation the routes apply.
func (fc *FallbackChain) lastResortBody(req Request, providerID, modelID string) map[string]any {
	body := fc.buildBody(providerID, req.Raw, modelID)
	applyContextTruncation(fc.Cfg, body)
	return body
}

// lastResortEligible reports whether the last-resort tier is configured and
// its trigger condition (remaining_seconds <= when) holds right now. An
// unset provider/model or a non-positive `when` disables the tier entirely.
func (fc *FallbackChain) lastResortEligible(ctx context.Context, lr config.LastResortConfig) bool {
	if lr.Provider == "" || lr.Model == "" || lr.WhenSeconds <= 0 {
		return false
	}
	rem := remainingBudget(ctx)
	return rem > 0 && rem <= time.Duration(lr.WhenSeconds)*time.Second
}

// lastResortAttempt is the shared last-resort engagement (round 11):
// stream=true runs it as a streaming attempt (committing straight to the
// client), stream=false as a buffered call. The SAME emptiness rules apply
// — callProvider/streamAttemptOnce already fail an empty completion, so a
// failed engagement never delivers an empty 200.
//
// Returns (resp, committed, engaged): engaged=false → the tier was not
// configured/eligible and nothing happened; engaged=true → the attempt ran
// and either delivered (resp for buffered / committed for stream) or failed
// (the caller logs and lets the loop continue).
func (fc *FallbackChain) lastResortAttempt(ctx context.Context, req Request, w http.ResponseWriter, stream bool) (resp *Response, committed, engaged bool) {
	lr := fc.Cfg.Fallback.LastResort
	if !fc.lastResortEligible(ctx, lr) {
		return nil, false, false
	}
	provCfg := fc.findProvider(lr.Provider)
	if provCfg == nil {
		// Tier configured but the provider is missing/disabled — skip it.
		log.Printf("WARN last-resort provider %q not found or disabled — skipping the tier", lr.Provider)
		return nil, false, false
	}
	uses := lastResortUses.Add(1)
	log.Printf("WARN last-resort engaged: req=%s provider=%s model=%s remaining=%v uses=%d — the deadline loop is in its final seconds", req.RequestID, lr.Provider, lr.Model, remainingBudget(ctx), uses)
	body := fc.lastResortBody(req, lr.Provider, lr.Model)
	start := time.Now()
	if stream {
		resp, committed, err := fc.streamAttemptOnce(ctx, w, *provCfg, body, req.RequestID)
		if committed {
			log.Printf("last-resort delivered (stream): req=%s provider=%s model=%s in %v", req.RequestID, lr.Provider, lr.Model, time.Since(start))
			return resp, true, true
		}
		log.Printf("WARN last-resort failed (stream): req=%s provider=%s model=%s cause=%v — the loop continues", req.RequestID, lr.Provider, lr.Model, failureCause(resp, err))
		return nil, false, true
	}
	resp, err := fc.callProvider(ctx, *provCfg, body)
	fc.logAttempt(req.RequestID, lr.Provider, lr.Model, int(uses), time.Since(start), failureCause(resp, err), finalFailureDecision(resp, err).String())
	if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
		log.Printf("last-resort delivered: req=%s provider=%s model=%s in %v", req.RequestID, lr.Provider, lr.Model, time.Since(start))
		return resp, false, true
	}
	log.Printf("WARN last-resort failed: req=%s provider=%s model=%s cause=%v — the loop continues", req.RequestID, lr.Provider, lr.Model, failureCause(resp, err))
	return nil, false, true
}

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/alias"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// FallbackChain manages the ordered provider fallback logic.
//
// Fallback order:
//  1. OpenRouter with models[] array (native intra-provider fallback)
//  2. Groq with service_tier: auto
//  3. Gemini (4-dimensional rate limit check)
//  4. Remaining providers (sorted by strategy)
//  5. OpenRouter with model: "openrouter/free" (ultimate fallback)
//
// Special cases:
//   - HuggingFace 503+loading: wait estimated_time, retry SAME provider
//   - Cerebras: pre-emptive slowdown if remaining-requests < 2
type FallbackChain struct {
	Cfg                *config.Config
	Strategy           strategy.Strategy
	Catalog            *catalog.Catalog
	RateLimiter        *ratelimit.GlobalTracker
	GeminiTracker      *ratelimit.GeminiTracker
	ReliabilityTracker *reliability.Tracker
	HTTPClient         *http.Client
	// StreamHTTPClient caps streaming attempts separately from buffered
	// calls (fallback.stream_timeout_seconds — NG6). Nil falls back to
	// HTTPClient.
	StreamHTTPClient *http.Client

	// Resolver maps canonical model names to raw upstream model IDs for
	// response rewriting (CanonicalFor). May be nil — nil means canonical
	// aliasing is off and UsedModel keeps the raw upstream name.
	Resolver *alias.Resolver

	// Slots is the RPM slot limiter (task L0). Non-nil + a positive
	// providers[].rpm_limit gates candidate entry on an atomic slot
	// reservation; a fully slotless walk returns the errAllSlotsBusy
	// sentinel instead of a real exhaustion error. Nil = no gating.
	Slots *SlotLimiter
	// Queues is the FIFO slot-queue manager (task L0). When both Queues and
	// fallback.queue_wait_seconds are set, the errAllSlotsBusy sentinel
	// waits in its model group's queue instead of failing immediately.
	Queues *queueManager
	// Demotions, when non-nil, is the cross-request unknown-400 demotion
	// registry (round 8 steps 3–4): models that answered an unrecognized
	// 400 leave the ranking for fallback.unknown_400_demotion_seconds and
	// are handed back FIFO when the TTL lapses or the registry overflows.
	// Nil disables demotion (tests build bare chains).
	Demotions *demotionRegistry
	// Latencies, when non-nil, is the cross-request slow-TTFT tracker
	// (round 10): every attempt's measured time-to-first-token feeds a
	// per-model EMA and the consecutive-slow circuit breaker (breaker
	// trips demote into the Demotions registry), and slow-EMA candidates
	// are deprioritized to the back of the ranking. Nil disables (tests
	// build bare chains).
	Latencies *latencyRegistry
	// Gate, when non-nil, is the request's capability demand (task Q) — the
	// walk ranks only entries passing it (NG8 exclusion, context fit,
	// tools/vision). Nil keeps the old full-catalog ranking.
	Gate *capabilityDemand

	// Loop, when non-nil, is the loop-until-deadline state (round 11, task
	// AA): the per-request park TTLs of transiently failed candidates and
	// the hop counter shared across rounds. Execute/ExecuteStream install
	// it when fallback.loop_until_deadline is on (and a deadline exists);
	// nil keeps the single-walk behavior.
	Loop *loopState
}

// Request is the parsed incoming chat-completions request.
type Request struct {
	Model     string           `json:"model"`
	Messages  []map[string]any `json:"messages"`
	Stream    bool             `json:"stream,omitempty"`
	MaxTokens int              `json:"max_tokens,omitempty"`
	// ClientModel is the model name the client originally asked for, kept
	// intact before any routing redirection (agent profiles, raw- prefix,
	// alias-route fallback clearing). The pool-alias route uses it to keep
	// X-Used-Model reporting the public identity across the strategy-chain
	// fallback.
	ClientModel string `json:"-"`
	// Raw holds the original request body for passthrough.
	Raw map[string]any `json:"-"`
	// RequestID identifies the request across the per-attempt logs and the
	// X-Request-Id response header; the handler mints one when the client
	// did not send a usable X-Request-Id.
	RequestID string `json:"-"`
}

// Response wraps an upstream HTTP response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	// UsedModel is the model that actually served the request (from response body).
	UsedModel string
}

// candidateAttempt executes one candidate (including its bounded
// same-candidate retries). committed reports that a streaming attempt
// already answered the client — the walk must not touch the response again
// and the request is done either way.
type candidateAttempt func(ctx context.Context, callCfg config.ProviderConfig, body map[string]any, attempt *int) (*Response, bool, error)

// Execute runs the fallback chain for the given request.
//
// Returns the first successful response, the first fail-fast upstream error
// response (genuine payload defects such as 400/413/content-policy refusals
// are surfaced to the caller as-is instead of being retried or walked —
// see ClassifyFailure), or an error when every candidate failed with a
// retryable failure. Only genuine candidate exhaustion produces an error;
// callers turn that into the exhaustion response.
func (fc *FallbackChain) Execute(ctx context.Context, req Request) (*Response, error) {
	// Per-request wall-clock budget (fallback.total_deadline_seconds): the
	// deadline rides the request context, so every attempt, HF-loading wait
	// and backoff sleep observes it. It wraps the QUEUE loop too — a slot
	// wait counts against the same budget.
	ctx, cancelTotal := fc.withTotalDeadline(ctx)
	if cancelTotal != nil {
		defer cancelTotal()
	}
	// Loop mode (round 11, task AA): install the per-request loop state —
	// the deadline becomes the walk's primary stop and transiently failed
	// candidates re-enter the ranking after their park TTL. The invariant
	// holds unchanged: only a REAL answer or a clear error leaves this loop.
	if fc.loopUntilDeadline(ctx) {
		fc.Loop = newLoopState()
	}
	for {
		resp, committed, err := fc.executeOnce(ctx, req)
		busy, ok := err.(*errAllSlotsBusy)
		if ok {
			served, qerr := fc.waitInQueue(ctx, req, busy, false)
			if served {
				continue
			}
			if fc.Loop == nil {
				return nil, qerr // single walk — the historical behavior
			}
			// Loop mode: a slotless walk is quota pressure, not a defect —
			// wait a round (bounded by the deadline) and walk again.
			if ctx.Err() != nil {
				return nil, errRequestBudget(ctx)
			}
			if !fc.sleepRound(ctx, req.RequestID) {
				return nil, errRequestBudget(ctx)
			}
			continue
		}
		if committed {
			// Unreachable for the buffered walk; kept for the shared signature.
			return nil, nil
		}
		if err == nil {
			return resp, nil // success or a surfaced fail-fast response
		}
		if fc.Loop == nil {
			return resp, err // single walk — the historical behavior
		}
		// --- Loop mode: the deadline is the primary stop (round 11). ---
		if ctx.Err() != nil {
			// Deadline fired or the client disconnected mid-loop: the clear
			// budget error (the caller answers 503 + envelope).
			return nil, errRequestBudget(ctx)
		}
		if errors.Is(err, errHopCeiling) {
			// The anti-corruption ceiling is absolute — looping past it
			// would only re-reach it.
			return nil, err
		}
		// Last-resort tier (round 11): when configured and the remaining
		// budget is at/below `when`, try the tier once per round — the same
		// emptiness rules; a failed engagement never delivers anything.
		if lrResp, _, engaged := fc.lastResortAttempt(ctx, req, nil, false); engaged && lrResp != nil {
			return lrResp, nil
		}
		// Small cooldown between rounds (also the park TTL): wait for the
		// transiently failed candidates to recover — never spin.
		if !fc.sleepRound(ctx, req.RequestID) {
			return nil, errRequestBudget(ctx)
		}
	}
}

// executeOnce is one full candidate walk (the previous Execute body). The
// total deadline wrap lives in Execute/ExecuteStream so slot-queue waits
// share the request's budget.
func (fc *FallbackChain) executeOnce(ctx context.Context, req Request) (*Response, bool, error) {
	resp, committed, err := fc.walk(ctx, req, fc.bufferedTry(req), fc.bufferedTryOnce(req.RequestID))
	return resp, committed, err
}

// waitInQueue parks the request in its model group's FIFO slot queue after
// the walk reported errAllSlotsBusy. served=true hands the caller its turn
// (it has been removed from the line); served=false answers why the wait
// ended — an immediate no-slot error when queueing is off, the queue-full
// or wait-timeout sentinels, or errClientGone when the request context
// died while waiting.
func (fc *FallbackChain) waitInQueue(ctx context.Context, req Request, busy *errAllSlotsBusy, senior bool) (bool, error) {
	if fc.Queues == nil || fc.queueWait() <= 0 {
		// Queueing off (or not wired): the walk fails with the clear
		// slotless-exhaustion cause — no waiting, no fake success.
		return false, busy
	}
	return fc.Queues.wait(ctx, queueGroup(req), busy.keys, busy.earliest, fc.queueWait(), fc.queueMaxWaiters(), senior)
}

// queueWait returns the effective slot-queue wait budget in seconds; 0
// disables queueing.
func (fc *FallbackChain) queueWait() time.Duration {
	if s := fc.Cfg.Fallback.QueueWaitSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

// queueMaxWaiters returns the effective per-queue waiter cap (viper
// default 64 for zeroed Config structs).
func (fc *FallbackChain) queueMaxWaiters() int {
	if s := fc.Cfg.Fallback.QueueMaxWaiters; s > 0 {
		return s
	}
	return 64
}

// bufferedTry wraps the buffered attempt with its same-candidate retry.
func (fc *FallbackChain) bufferedTry(req Request) candidateAttempt {
	reqID := req.RequestID
	return func(ctx context.Context, callCfg config.ProviderConfig, body map[string]any, attempt *int) (*Response, bool, error) {
		resp, err := fc.callProviderWithRetries(ctx, callCfg, body, attempt, reqID)
		return resp, false, err
	}
}

// bufferedTryOnce is a single buffered call — the OpenRouter steps and the
// HF cold-start re-attempt run without the retry ladder, as before. They
// still consume the attempt budget (task G) and emit the same per-attempt
// log (task F); on failure the walk decides with finalFailureDecision, so
// that is the decision logged here.
func (fc *FallbackChain) bufferedTryOnce(reqID string) candidateAttempt {
	return func(ctx context.Context, callCfg config.ProviderConfig, body map[string]any, attempt *int) (*Response, bool, error) {
		start := time.Now()
		resp, err := fc.callProvider(ctx, callCfg, body)
		dur := time.Since(start)
		*attempt++
		if err == nil && resp.StatusCode == http.StatusOK {
			fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, dur, "ok", "ok")
			return resp, false, err
		}
		fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, dur, failureCause(resp, err), finalFailureDecision(resp, err).String())
		return resp, false, err
	}
}

// walk runs the ranked candidate walk shared by the buffered (Execute) and
// streaming (ExecuteStream) modes. The per-candidate behavior is injected:
// try = one candidate with its same-candidate retry ladder; tryOnce = one
// bare attempt (OpenRouter steps, HF cold-start re-attempt).
//
// Returns:
//
//	err != nil   — genuine exhaustion / hop ceiling / request budget; callers
//	               answer with the exhaustion response
//	committed    — a streaming attempt answered the client; the request is done
//	resp != nil  — a response to deliver: success (200) or a fail-fast
//	               upstream error to surface verbatim
func (fc *FallbackChain) walk(ctx context.Context, req Request, try, tryOnce candidateAttempt) (*Response, bool, error) {
	freeEntries := fc.Catalog.FreeEntries()
	// Capability gate (task Q): rank only entries the request can actually
	// use — NG8-flagged entries are never selected; with a demand set,
	// context fit and tools/vision also filter. An empty surviving set
	// reaches the routes' immediate no-capable-candidate error, never a
	// queue wait.
	freeEntries = gateEntries(freeEntries, fc.gateDemand(), fc.Cfg.Fallback.ModelBlocklist)
	// Unknown-400 demotion (round 8 steps 3–4): models that unrecognized-
	// 400-ed an earlier request stay out of the ranking until their TTL
	// lapses; when every candidate is demoted they come back (last chance)
	// rather than starving the request.
	freeEntries = fc.filterDemotedEntries(freeEntries)
	stratReq := strategy.Request{
		Messages:              req.Messages,
		EstimatedPromptTokens: estimateTokens(req.Messages),
		MaxTokens:             req.MaxTokens,
		Stream:                req.Stream,
		Model:                 req.Model,
	}

	ranked := fc.Strategy.Rank(stratReq, freeEntries, nil)
	// Slow-TTFT deprioritization (round 10): candidates whose measured
	// EMA TTFT sits at/above fallback.ttft_slow_threshold_seconds move to
	// the BACK of the walk (never dropped) — a free upstream that answers
	// in 45s loses the first hop to every faster sibling without any
	// config change, while still serving as a last resort.
	ranked = fc.deprioritizeSlowRanked(ranked)
	// Loop-parked candidates (round 11, task AA): candidates that failed
	// transiently in an earlier round of this request sit out the ranking
	// until their park TTL lapses — unless EVERY candidate is parked (last
	// chance, never starve).
	ranked = fc.filterLoopParked(ranked)
	maxAttempts := fc.maxAttemptsBudget()
	maxHops := fc.maxFallbackHops()
	// Loop mode: the hop counter is seeded from the loop state so
	// max_fallback_hops bounds the WHOLE request across all rounds; the
	// single-walk mode starts at 0 as always.
	hops := fc.Loop.hops()
	// slotSkips collects the candidates that were skipped for lack of an
	// RPM slot and entered counts the upstream calls actually entered —
	// when the walk ends with entered==0 and skips>0 it failed for QUOTA,
	// not for defects, and the errAllSlotsBusy sentinel lets the caller
	// wait for a slot instead of failing immediately (task L0).
	slotSkips := []string{}
	entered := 0
	// attempt is the per-request upstream-call budget counter
	// (fallback.max_attempts). EVERY upstream call consumes it — the ranked
	// walk's attempts with their same-candidate retries AND the once-paths:
	// the OpenRouter models[] step, the openrouter/free ultimate fallback,
	// and the HuggingFace cold-start re-attempt (task G).
	attempt := 0
	// unknown400Seen is the per-request cross-model confirm state (round 8
	// step 2): the distinct upstream models ("provider/model") that
	// answered an unrecognized 400 with this request's payload. The
	// classifier reads its size through failureDecisionWalk — below
	// unknown_400_confirm_n the walk moves to the next candidate, at the
	// ceiling it fail-fasts and surfaces the upstream 400 exactly as
	// before.
	unknown400Seen := make(map[string]bool)

	// Step 1: Try OpenRouter with models[] array (native fallback)
	if or := fc.findProvider("openrouter"); or != nil {
		openRouterModels := fc.collectProviderModels("openrouter", ranked)
		if len(openRouterModels) > 0 {
			// The models[] request is one candidate/hop. The ceiling check
			// is new but equivalent for the single walk (hops==0 here and
			// maxHops >= 8); in loop mode the seeded counter obeys it too.
			if !fc.hopOK(&hops, maxHops) {
				return nil, false, fmt.Errorf("fallback hop ceiling %d reached — aborting walk (%w)", maxHops, errHopCeiling)
			}
			body := BuildOpenRouterRequest(req.Raw, openRouterModels)
			if req.Stream {
				body["stream"] = true // streaming walk keeps SSE end-to-end (NG3)
			}
			// The models[] request is one upstream call: it consumes the
			// attempt budget like the ranked walk (task G). attempt is 0
			// here and max_attempts >= 1, so this step always runs.
			resp, committed, err := tryOnce(ctx, *or, body, &attempt)
			if committed {
				return nil, true, nil
			}
			if err == nil && resp.StatusCode == http.StatusOK {
				fc.ReliabilityTracker.RecordOutcome("openrouter", reliability.OutcomeSuccess)
				return resp, false, nil
			}
			// Fail-fast client/request errors abort the whole chain — no
			// other step can succeed with the same payload. Auth/model
			// failures fall through to the next candidate instead
			// (three-way classification, task C).
			if finalFailureDecision(resp, err) == FailFast && resp != nil {
				log.Printf("fallback: openrouter models[] fail-fast status %d — surfacing to client, aborting chain", resp.StatusCode)
				fc.ReliabilityTracker.RecordOutcome("openrouter", failureOutcome(resp, err))
				return resp, false, nil
			}
			if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
				// The models[] candidate failed on auth: the same 401/403
				// contract as the ranked walk (task H) — cooldown + warn —
				// then walk on to the ranked candidates.
				noteAuthFailure(fc.RateLimiter, "openrouter", "", resp.StatusCode)
			}
			log.Printf("fallback: openrouter models[] failed (%v) — continuing", err)
			fc.ReliabilityTracker.RecordOutcome("openrouter", failureOutcome(resp, err))
		}
	}

	// Step 2–4: Try one model per provider in ranked order.
	//
	// Key invariant: once a provider returns an error, ALL remaining entries
	// for that provider are skipped immediately — EXCEPT a 429, which is a
	// per-model quota signal (confirmed fact: the free quota is per model,
	// e.g. UnoRouter 1 request/minute/model): the provider stays walkable
	// for its other models, each holding its own slot. The provider-level
	// skip still prevents a large broken provider (e.g. NVIDIA NIM with 172
	// models) from exhausting maxAttempts before faster, more reliable
	// providers (Groq, Cerebras, Gemini) are even tried.
	failedProviders := make(map[string]bool)
	for _, r := range ranked {
		if ctx.Err() != nil {
			// The request budget (or the client) canceled the context —
			// stop the walk immediately, answer with the exhaustion
			// response.
			return nil, false, errRequestBudget(ctx)
		}
		if attempt >= maxAttempts {
			break
		}
		// Skip OpenRouter entries — already tried above via models[] array.
		if r.ProviderID == "openrouter" {
			continue
		}
		// Skip providers that already returned an error this request.
		if failedProviders[r.ProviderID] {
			continue
		}

		providerCfg := fc.findProvider(r.ProviderID)
		if providerCfg == nil {
			continue
		}

		// RPM slot gate (task L0): entering this candidate consumes one
		// atomic reservation per sliding minute. A denied candidate is
		// collected for the group queue and costs no hop and no attempt.
		if !slotEnter(fc.Slots, providerCfg, r.ModelID, &slotSkips) {
			continue
		}

		// Skip any provider currently on rate-limit cooldown (auth errors).
		if fc.RateLimiter.IsOnCooldown(r.ProviderID) {
			log.Printf("fallback: %s on cooldown — skipping provider", r.ProviderID)
			failedProviders[r.ProviderID] = true
			continue
		}
		// Skip this specific model if it is rate-limited, but keep trying other models.
		if fc.RateLimiter.IsOnCooldown(r.ProviderID + "/" + r.ModelID) {
			log.Printf("fallback: %s/%s rate-limited — skipping model", r.ProviderID, r.ModelID)
			continue
		}

		// Cerebras pre-emptive check (RPS-level, finer than cooldown).
		if r.ProviderID == "cerebras" {
			tracker := fc.RateLimiter.Provider("cerebras")
			if !tracker.CanRequest(r.ModelID, 0, 0) {
				log.Printf("fallback: cerebras pre-emptive skip (rate limited)")
				failedProviders[r.ProviderID] = true
				continue
			}
			spacingMs := fc.Cfg.Fallback.CerebrasRequestSpacingMs
			if spacingMs > 0 {
				select {
				case <-ctx.Done():
					return nil, false, errRequestBudget(ctx)
				case <-time.After(time.Duration(spacingMs) * time.Millisecond):
				}
			}
		}

		// Gemini 4-dimensional rate check.
		if r.ProviderID == "gemini" && fc.GeminiTracker != nil {
			if !fc.GeminiTracker.CanRequest(r.ModelID, stratReq.EstimatedPromptTokens) {
				log.Printf("fallback: gemini %s rate limited — skipping", r.ModelID)
				failedProviders[r.ProviderID] = true
				continue
			}
		}

		// Hop ceiling (fallback.max_fallback_hops): a hard stop — the walk
		// answers with the exhaustion response immediately; no further
		// candidates and no ultimate-fallback step. This bounds the walk
		// even when max_attempts / retries_per_candidate are cranked up.
		// Loop mode (round 11): the counter spans ALL rounds and
		// errHopCeiling ends the loop — the ceiling is the anti-corruption
		// bound, never retried past.
		if !fc.hopOK(&hops, maxHops) {
			return nil, false, fmt.Errorf("fallback hop ceiling %d reached — aborting walk (%w)", maxHops, errHopCeiling)
		}

		body := fc.buildBody(r.ProviderID, req.Raw, r.ModelID)
		if req.Stream {
			body["stream"] = true // streaming walk keeps SSE end-to-end (NG3)
		}
		// Apply active key slot when multiple API keys are configured.
		callCfg := *providerCfg
		if keys := providerCfg.AllKeys(); len(keys) > 1 {
			if idx := fc.RateLimiter.CurrentKeyIndex(r.ProviderID); idx < len(keys) {
				callCfg.APIKey = keys[idx]
				callCfg.APIKeyEnv = ""
			}
		}
		hopStart := time.Now()
		resp, committed, err := try(ctx, callCfg, body, &attempt)
		entered++

		if !committed && (err != nil || resp.StatusCode != http.StatusOK) {
			// The request budget (or the client) canceled the context while
			// the attempt ran — every further attempt is pointless.
			if ctx.Err() != nil {
				return nil, false, errRequestBudget(ctx)
			}
			// HuggingFace 503+loading: wait and retry SAME provider (cold
			// start). The re-attempt consumes the attempt budget like every
			// other upstream call (task G) — with the budget spent, the 503
			// drops through to the normal classification below.
			if r.ProviderID == "huggingface" && resp != nil && resp.StatusCode == 503 && attempt < maxAttempts {
				if waitMs := parseHFLoadingWait(resp.Body); waitMs > 0 {
					log.Printf("fallback: huggingface model loading, waiting %dms", waitMs)
					select {
					case <-ctx.Done():
						return nil, false, errRequestBudget(ctx)
					case <-time.After(time.Duration(waitMs) * time.Millisecond):
					}
					resp, committed, err = tryOnce(ctx, *providerCfg, body, &attempt)
					if committed || (err == nil && resp != nil && resp.StatusCode == http.StatusOK) {
						fc.ReliabilityTracker.RecordOutcome(r.ProviderID, successOutcome(time.Since(hopStart)))
						return resp, committed, nil
					}
				}
			}
			// Apply per-status cooldown logic.
			if resp != nil {
				switch resp.StatusCode {
				case 429:
					// 429 is the per-model quota signal (confirmed:
					// 1 request/minute PER MODEL on UnoRouter free) —
					// deliberately NOT MarkNeedsReverification (that is
					// reserved for 404 "model gone"): a quota hit must not
					// remove the model from the pool; it recovers at
					// next_available below.
					info := ratelimit.ExtractRateLimitInfo(r.ProviderID, resp.Header, resp.Body)
					waitDur := info.WaitDuration()
					until := time.Now().Add(waitDur)
					// Cooldown on the specific model, not the whole provider.
					fc.RateLimiter.SetCooldown(r.ProviderID+"/"+r.ModelID, until)
					// The 429 also adjusts the slot limiter's next_available
					// (task L0): no reservation is granted before the
					// provider said so, even when the local window is free.
					if fc.Slots != nil {
						fc.Slots.AdjustNextAvailable(r.ProviderID+"/"+r.ModelID, until)
					}
					// Rotate to the next API key slot if multiple keys are configured.
					if providerCfg != nil && providerCfg.NumKeys() > 1 {
						newIdx := fc.RateLimiter.RotateKey(r.ProviderID, providerCfg.NumKeys())
						log.Printf("fallback: %s/%s rate limited — rotated to key slot %d", r.ProviderID, r.ModelID, newIdx+1)
					} else {
						log.Printf("fallback: %s/%s rate limited — cooldown until %s", r.ProviderID, r.ModelID, until.Format("15:04:05"))
					}
				case 401, 403:
					noteAuthFailure(fc.RateLimiter, r.ProviderID, r.ModelID, resp.StatusCode)
				case 404:
					fc.Catalog.MarkNeedsReverification(r.ProviderID, r.ModelID)
					fc.RateLimiter.SetCooldown(r.ProviderID, time.Now().Add(5*time.Minute))
					log.Printf("fallback: %s model %s not found — cooldown 5min", r.ProviderID, r.ModelID)
				}
			}
			// Fail-fast upstream error: the request itself was rejected.
			// Walking more providers cannot fix it and would mask the real
			// error behind the exhaustion response, so abort the chain and
			// surface the upstream response as-is. Auth/model failures are
			// classified FallbackNext and continue below with the cooldowns
			// above already applied.
			if fc.failureDecisionWalk(resp, err, r.ProviderID, r.ModelID, unknown400Seen) == FailFast {
				if resp != nil {
					log.Printf("fallback: %s/%s fail-fast status %d — surfacing to client, aborting chain", r.ProviderID, r.ModelID, resp.StatusCode)
					fc.ReliabilityTracker.RecordOutcome(r.ProviderID, failureOutcome(resp, err))
					return resp, false, nil
				}
				return nil, false, err // context canceled — client is gone
			}
			statusCode := 0
			if resp != nil {
				statusCode = resp.StatusCode
			}
			log.Printf("fallback: %s/%s status %d — skipping candidate", r.ProviderID, r.ModelID, statusCode)
			fc.ReliabilityTracker.RecordOutcome(r.ProviderID, failureOutcome(resp, err))
			if fc.ReliabilityTracker.ShouldCooldown(r.ProviderID) {
				fc.RateLimiter.SetCooldown(r.ProviderID, time.Now().Add(30*time.Second))
				log.Printf("fallback: %s reliability below 50%% â short cooldown", r.ProviderID)
			}
			if statusCode != http.StatusTooManyRequests {
				// 429 is a PER-MODEL quota signal (confirmed fact: the free quota
				// is per model): the provider stays walkable — its other models
				// hold separate slots. The model itself is parked by the per-model
				// cooldown + slot next_available above. Every other failure class
				// keeps the provider-level skip.
				failedProviders[r.ProviderID] = true // don't try other models from this provider
			}
			// Loop mode (round 11, task AA): a transiently failed candidate
			// (empty completion, timeout, 5xx, 429, transport) is parked for
			// the round cooldown — it re-enters the NEXT rounds' ranking when
			// the TTL lapses, never cut off for the whole request. Fail-fast
			// classes ended the walk above and never park.
			if fc.Loop != nil && transientFailure(resp, err) {
				fc.Loop.park(demotionKey(r.ProviderID, r.ModelID), time.Now().Add(fc.roundCooldown()))
			}
			continue
		}

		// Success path.
		if r.ProviderID == "cerebras" {
			info := ratelimit.ExtractRateLimitInfo("cerebras", resp.Header, resp.Body)
			if info.RemainingRequests >= 0 && info.RemainingRequests < 2 {
				tracker := fc.RateLimiter.Provider("cerebras")
				tracker.PreemptiveSlowdown(info.WaitDuration())
			}
			_ = fc.GeminiTracker
		}
		fc.ReliabilityTracker.RecordOutcome(r.ProviderID, successOutcome(time.Since(hopStart)))
		if r.ProviderID == "gemini" && fc.GeminiTracker != nil {
			fc.GeminiTracker.RecordRequest(r.ModelID, stratReq.EstimatedPromptTokens)
		}
		return resp, committed, nil
	}

	// Step 5: Ultimate fallback — OpenRouter free router. It is one more
	// upstream call: it consumes the attempt budget like every other path
	// (task G) and only runs while the budget lasts.
	if or := fc.findProvider("openrouter"); or != nil && attempt < maxAttempts {
		// Loop mode (round 11): the seeded counter obeys the ceiling.
		if !fc.hopOK(&hops, maxHops) {
			return nil, false, fmt.Errorf("fallback hop ceiling %d reached — aborting walk (%w)", maxHops, errHopCeiling)
		}
		log.Printf("fallback: trying openrouter/free ultimate fallback")
		body := OpenRouterFreeRouterRequest(req.Raw)
		if req.Stream {
			body["stream"] = true // streaming walk keeps SSE end-to-end (NG3)
		}
		resp, committed, err := tryOnce(ctx, *or, body, &attempt)
		entered++
		if committed {
			return nil, true, nil
		}
		if err == nil && resp.StatusCode == http.StatusOK {
			fc.ReliabilityTracker.RecordOutcome("openrouter", reliability.OutcomeSuccess)
			return resp, false, nil
		}
		fc.ReliabilityTracker.RecordOutcome("openrouter", failureOutcome(resp, err))
		// A fail-fast error from the last step still surfaces the real
		// upstream response instead of degrading into the exhaustion path.
		if finalFailureDecision(resp, err) == FailFast && resp != nil {
			log.Printf("fallback: openrouter/free fail-fast status %d — surfacing to client", resp.StatusCode)
			return resp, false, nil
		}
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			// The same 401/403 contract as every other path (task H) — the
			// cooldown applies even though this walk ends in exhaustion.
			noteAuthFailure(fc.RateLimiter, "openrouter", bodyModel(body), resp.StatusCode)
		}
	}

	// The walk entered NO upstream call but skipped candidates for lack of
	// an RPM slot: this is quota pressure, not a defect — hand the sentinel
	// to the caller so it can wait in the group queue (task L0).
	if entered == 0 && len(slotSkips) > 0 {
		earliest, _ := fc.Slots.Earliest(slotSkips)
		return nil, false, &errAllSlotsBusy{keys: slotSkips, earliest: earliest}
	}

	return nil, false, fmt.Errorf("all providers exhausted after %d attempts", attempt)
}

// maxAttemptsBudget returns the effective per-request upstream-call budget:
// fallback.max_attempts with the <=0 guard (viper default 5; a zeroed
// FallbackConfig — as built by some tests — must not read as "unbounded").
// Every site that compares against the budget, including the same-candidate
// retry loop, must use this helper.
func (fc *FallbackChain) maxAttemptsBudget() int {
	if fc.Cfg.Fallback.MaxAttempts <= 0 {
		return 5
	}
	return fc.Cfg.Fallback.MaxAttempts
}

// maxFallbackHops returns the effective per-request candidate ceiling
// (fallback.max_fallback_hops); the <=0 guard mirrors the viper default 8
// for zeroed Config structs.
func (fc *FallbackChain) maxFallbackHops() int {
	if fc.Cfg.Fallback.MaxFallbackHops <= 0 {
		return 8
	}
	return fc.Cfg.Fallback.MaxFallbackHops
}

// totalDeadline returns the request's wall-clock budget; 0 = disabled.
func (fc *FallbackChain) totalDeadline() time.Duration {
	if fc.Cfg.Fallback.TotalDeadlineSeconds <= 0 {
		return 0
	}
	return time.Duration(fc.Cfg.Fallback.TotalDeadlineSeconds) * time.Second
}

// gateDemand returns the walk's capability gate: the route-installed demand
// when set, otherwise the empty demand — which still applies the NG8 rule
// (NeedsReverification entries are never selected).
func (fc *FallbackChain) gateDemand() capabilityDemand {
	if fc.Gate != nil {
		return *fc.Gate
	}
	return capabilityDemand{}
}

// withTotalDeadline wraps ctx with the request's total wall-clock deadline
// when fallback.total_deadline_seconds is configured. The returned cancel
// is nil when no deadline was added; callers must call it when non-nil.
func (fc *FallbackChain) withTotalDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if d := fc.totalDeadline(); d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, nil
}

// errRequestBudget wraps a context error as the walk's budget-exhaustion
// error: the request's total deadline fired (fallback.total_deadline_seconds)
// or the client canceled — no further attempt is meaningful.
func errRequestBudget(ctx context.Context) error {
	return fmt.Errorf("request budget exhausted: %w", ctx.Err())
}

// callProviderWithRetries wraps callProvider with the bounded same-candidate
// retry: previously one failed call dropped the provider for the whole
// request (gap G1), so a single transient 5xx or timeout on the best-ranked
// candidate already degraded the response. Retries consume the caller's
// maxAttempts budget — every upstream call increments *attempt — keeping
// the global call bound intact. Every attempt (including the initial one)
// is logged with the request id, candidate, attempt number, cause, duration
// and the ladder's decision (task F).
func (fc *FallbackChain) callProviderWithRetries(ctx context.Context, callCfg config.ProviderConfig, body map[string]any, attempt *int, reqID string) (*Response, error) {
	start := time.Now()
	resp, err := fc.callProvider(ctx, callCfg, body)
	dur := time.Since(start)
	*attempt++
	if err == nil && resp.StatusCode == http.StatusOK {
		fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, dur, "ok", "ok")
		return resp, nil
	}
	return fc.retrySameCandidate(ctx, callCfg, body, resp, err, attempt, reqID, dur)
}

// bodyModel extracts the requested upstream model id for logging.
func bodyModel(body map[string]any) string {
	id, _ := body["model"].(string)
	return id
}

// logAttempt emits the per-attempt observability record (task F): request
// id, candidate, attempt number, cause, duration, decision. cause carries
// failure summaries only — never API keys or prompt content.
func (fc *FallbackChain) logAttempt(reqID, providerID, modelID string, n int, dur time.Duration, cause string, decision string) {
	if reqID == "" {
		reqID = "-"
	}
	if len(cause) > 200 {
		cause = cause[:200] + "…"
	}
	log.Printf("attempt: req=%s candidate=%s/%s n=%d duration=%s cause=%q decision=%s", reqID, providerID, modelID, n, dur, cause, decision)
}

// failureCause summarizes a failed attempt for the attempt log (transport
// error text or the status code — no request content).
func failureCause(resp *Response, err error) string {
	if err != nil {
		return err.Error()
	}
	if resp == nil {
		return "no response"
	}
	return fmt.Sprintf("status %d", resp.StatusCode)
}

// slowSuccessThreshold separates a prompt success from a slow one (task M):
// a success that burned the slot this long still ranks below a prompt
// success — every second holds the 1/minute slot busy.
const slowSuccessThreshold = 15 * time.Second

// errEmptyCompletion is the buffered empty-200: the upstream answered 200
// with no usable completion (round 11 task W — the full emptiness contract
// lives in empty.go). The attempt failed and the scored outcome is
// OutcomeWasted (task M) — the worst slot burn.
var errEmptyCompletion = errors.New("upstream answered 200 with an empty completion")

// contentFilterResponse builds the surfaced response for a
// finish_reason=content_filter refusal (round 11 task W): a 422 with an
// OpenAI-style error envelope — the upstream said 200 but the prompt was
// rejected, and surfacing that 200 verbatim would hand the client a
// content-less completion. The router-owned upstream headers keep the real
// identity.
func contentFilterResponse(cfg config.ProviderConfig, body map[string]any) *Response {
	hdr := http.Header{}
	hdr.Set("X-Free-Router-Upstream-Provider", cfg.ID)
	if m, ok := body["model"].(string); ok && m != "" {
		hdr.Set("X-Free-Router-Upstream-Model", m)
	}
	env := `{"error":{"message":"upstream refused the prompt with finish_reason=content_filter","type":"content_filter_error","code":"content_filter"}}`
	return &Response{StatusCode: http.StatusUnprocessableEntity, Header: hdr, Body: []byte(env)}
}

// failureOutcome classifies a failed attempt for the reliability EMA
// (task M): timeouts and empty-200s waste the slot hardest (no answer, no
// usable signal); 429/5xx and transport failures burn it with an
// actionable cause. Both outrank only... nothing — they ARE the penalties;
// the ordering lives in reliability.Outcome. The empty-200 (round 11 task
// W: any form of the emptiness contract) is recognized through the
// errEmptyCompletion sentinel — the heaviest penalty, whatever the shape.
func failureOutcome(resp *Response, err error) reliability.Outcome {
	if err != nil {
		if errors.Is(err, errEmptyCompletion) {
			return reliability.OutcomeWasted
		}
		if isTimeoutErr(err) {
			return reliability.OutcomeWasted
		}
		return reliability.OutcomeRateLimited
	}
	if resp == nil {
		return reliability.OutcomeRateLimited
	}
	if resp.StatusCode == http.StatusOK {
		// The failure block only sees status 200 when the attempt produced
		// nothing usable (empty/error first event on a stream) — an
		// empty-200, the worst kind of slot burn.
		return reliability.OutcomeWasted
	}
	return reliability.OutcomeRateLimited
}

// successOutcome distinguishes a prompt success from a slow one (task M):
// a slow success still delivered the answer, but the slot stayed busy
// longer — it ranks between plain success and the failure classes.
func successOutcome(dur time.Duration) reliability.Outcome {
	if dur > slowSuccessThreshold {
		return reliability.OutcomeSlowSuccess
	}
	return reliability.OutcomeSuccess
}

// retrySameCandidate retries the SAME provider/model up to
// fallback.retries_per_candidate times, sleeping between attempts with
// ratelimit.ExponentialBackoff + DefaultBackoffParams (exponential backoff
// with jitter — previously dead code, gap G4). The three-way classifier
// (ClassifyFailure) now owns the failure classes the old two-gate
// (classifyUpstreamFailure + sameCandidateRetryAllowed) used to split:
// 5xx follows fallback.retry_on_5xx, transport errors and 408 retry while
// retries remain, and a 429 NEVER retries the same candidate (per-model
// quota — it falls back immediately, no sleep). 404/401/403 and genuine
// payload errors fall straight through to the walk. A backoff sleep
// never runs past the request's remaining total deadline. The HuggingFace
// 503-loading recovery in Execute owns that case with its dedicated
// estimated-time wait, so it is excluded here to avoid double-retrying in
// front of it.
func (fc *FallbackChain) retrySameCandidate(ctx context.Context, callCfg config.ProviderConfig, body map[string]any, resp *Response, err error, attempt *int, reqID string, lastDur time.Duration) (*Response, error) {
	maxRetries := fc.Cfg.Fallback.RetriesPerCandidate
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > 5 {
		maxRetries = 5 // bound config abuse; the backoff alone reaches 16s by then
	}
	if callCfg.ID == "huggingface" && resp != nil && resp.StatusCode == http.StatusServiceUnavailable && parseHFLoadingWait(resp.Body) > 0 {
		fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, lastDur, "huggingface 503 loading — deferring to the walk's cold-start wait", "fallback")
		return resp, err
	}
	modelID := bodyModel(body)
	timeouts := 0
	if err != nil && isTimeoutErr(err) {
		timeouts = 1
	}
	retriesDone := 0
	for {
		dec := ClassifyFailure(fc.classifierInput(ctx, resp, err, retriesDone, maxRetries, timeouts))
		if dec == RetrySame {
			// Budget/deadline guards turn a nominal retry_same into a
			// fallback so the logged decision is the one acted on.
			if *attempt >= fc.maxAttemptsBudget() {
				dec = FallbackNext
			} else {
				base, maxDelay := ratelimit.DefaultBackoffParams()
				if delay := ratelimit.ExponentialBackoff(retriesDone, base, maxDelay); remainingBudget(ctx) > 0 && delay >= remainingBudget(ctx) {
					// Never sleep past the request's total deadline.
					dec = FallbackNext
				}
			}
		}
		fc.logAttempt(reqID, callCfg.ID, modelID, *attempt, lastDur, failureCause(resp, err), dec.String())
		if dec != RetrySame {
			return resp, err // client/request defect or class says move on
		}
		base, maxDelay := ratelimit.DefaultBackoffParams()
		delay := ratelimit.ExponentialBackoff(retriesDone, base, maxDelay)
		log.Printf("fallback: %s/%s retryable failure — same-candidate retry %d/%d after %v", callCfg.ID, modelID, retriesDone+1, maxRetries, delay)
		select {
		case <-ctx.Done():
			return resp, err
		case <-time.After(delay):
		}
		start := time.Now()
		resp, err = fc.callProvider(ctx, callCfg, body)
		lastDur = time.Since(start)
		*attempt++
		retriesDone++
		if err != nil && isTimeoutErr(err) {
			timeouts++
		}
		if err == nil && resp.StatusCode == http.StatusOK {
			fc.logAttempt(reqID, callCfg.ID, modelID, *attempt, lastDur, "ok", "ok")
			return resp, nil
		}
	}
}

// classifierInput assembles the pure classifier's input for a failed
// buffered attempt: Retry-After is parsed from the failure response headers
// (429/503) and the remaining budget from the request context's deadline
// (set by fallback.total_deadline_seconds when configured).
func (fc *FallbackChain) classifierInput(ctx context.Context, resp *Response, err error, retriesDone, maxRetries, timeouts int) FailureInput {
	in := FailureInput{
		Err:             err,
		RetriesDone:     retriesDone,
		MaxRetries:      maxRetries,
		RetryOn5xx:      fc.Cfg.Fallback.RetryOn5xx,
		RemainingBudget: remainingBudget(ctx),
		TimeoutsDone:    timeouts,
		// RetryOn429 retired (no-op): a 429 always falls back immediately —
		// per-model quota; sleeping against it is pointless.
	}
	if resp != nil {
		in.StatusCode = resp.StatusCode
		in.Body = resp.Body
		in.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return in
}

// sameCandidateRetryAllowed was folded into ClassifyFailure (task C): the
// 429/5xx flags now gate the retry_same verdict inside the classifier.

func (fc *FallbackChain) callProvider(ctx context.Context, cfg config.ProviderConfig, body map[string]any) (*Response, error) {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"

	// Per-provider timeout override (providers[].timeout, e.g. "30s").
	// The field was declared but never read before — every attempt
	// inherited only the shared client's provider_timeout_seconds cap.
	// The deadline must wrap the context BEFORE the request is built so
	// the request context carries it; it aborts the call as a transport
	// error, which the failure classification treats as retryable, so the
	// walk continues on the next candidate.
	if d := cfg.ResolvedTimeout(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	h, v := cfg.ResolvedAuth()
	if v != "" && v != "Bearer " {
		req.Header.Set(h, v)
	}
	for k, val := range cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}

	start := time.Now()
	resp, err := fc.HTTPClient.Do(req)
	if err != nil {
		// A transport timeout burned the whole wait without one usable
		// byte — the hardest TTFT observation there is (forced slow,
		// round 10).
		if isTimeoutErr(err) {
			fc.recordTTFT(cfg.ID, bodyModel(body), time.Since(start), true)
		}
		return nil, err
	}
	defer resp.Body.Close()
	// TTFT for buffered calls ≈ time to the response headers (round 10):
	// the body read that follows is generation time, not latency to the
	// first token.
	fc.recordTTFT(cfg.ID, bodyModel(body), time.Since(start), false)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	// Empty-200 guard (tasks M + W, round 11): an upstream that answers 200
	// with nothing usable has burned the attempt — surface it as a failed
	// attempt so the ladder retries/falls back and the client never
	// receives an empty "completion" (every request must get a REAL
	// answer). The scored outcome is OutcomeWasted — the worst slot burn.
	// The emptiness contract (empty.go) covers the whole family: blank or
	// unparseable bodies, empty choices, null/""/whitespace-only content,
	// finish_reason=stop/length with no content, usage.completion_tokens=0
	// without a usable signal, and reasoning-only results (unless
	// fallback.commit_on_reasoning delivers them). The reason and the
	// prompt size ride the error — the attempt log carries req= with the
	// exact "empty" cause. A finish_reason=content_filter refusal is NOT
	// empty: it is fail-fast, surfaced as a clear error (never a masked
	// 200).
	if resp.StatusCode == http.StatusOK {
		verdict := inspectCompletion(respBody, fc.commitOnReasoning())
		if verdict.contentFilter {
			return contentFilterResponse(cfg, body), errContentFilter
		}
		if verdict.empty {
			promptBytes := 0
			if data, merr := json.Marshal(body); merr == nil {
				promptBytes = len(data)
			}
			msgs, _ := body["messages"].([]any)
			return nil, fmt.Errorf("%w (reason=%s prompt_bytes=%d prompt_tokens_est=%d)", errEmptyCompletion, verdict.reason, promptBytes, estimateTokensAny(msgs))
		}
	}

	// Extract the actual model used (OpenRouter fills this in). The value
	// only feeds the X-Used-Model header: when the upstream model belongs
	// to a canonical alias group, the header reports the canonical name.
	// The response body is returned verbatim, and the raw upstream identity
	// is exposed via the X-Free-Router-Upstream-* headers.
	usedModel := ""
	if resp.StatusCode == http.StatusOK {
		var parsed struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(respBody, &parsed) == nil {
			usedModel = parsed.Model
		}
	}
	if requested, ok := body["model"].(string); ok && requested != "" {
		resp.Header.Set("X-Free-Router-Upstream-Provider", cfg.ID)
		resp.Header.Set("X-Free-Router-Upstream-Model", requested)
		if fc.Resolver != nil && usedModel != "" {
			if canon := fc.Resolver.CanonicalFor(cfg.ID, usedModel); canon != "" {
				usedModel = canon
			} else if canon := fc.Resolver.CanonicalFor(cfg.ID, requested); canon != "" {
				usedModel = canon
			}
		}
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       respBody,
		UsedModel:  usedModel,
	}, nil
}

func (fc *FallbackChain) findProvider(id string) *config.ProviderConfig {
	for i := range fc.Cfg.Providers {
		if fc.Cfg.Providers[i].ID == id && fc.Cfg.Providers[i].Enabled {
			return &fc.Cfg.Providers[i]
		}
	}
	return nil
}

func (fc *FallbackChain) collectProviderModels(providerID string, ranked []strategy.RankedEntry) []string {
	var models []string
	for _, r := range ranked {
		if r.ProviderID == providerID {
			models = append(models, r.ModelID)
		}
	}
	return models
}

// anthropicOnlyFields are request fields specific to the Anthropic Messages API
// that are not part of the OpenAI spec and will cause 400 errors on other providers.
var anthropicOnlyFields = []string{
	"promptcachekey",
	"prompt_cache_key",
	"thinking",
	"betas",
}

func (fc *FallbackChain) buildBody(providerID string, raw map[string]any, modelID string) map[string]any {
	body := copyMap(raw)
	body["model"] = modelID
	delete(body, "stream") // always non-streaming to providers; fallback requires buffered JSON

	// Strip Anthropic-specific fields that other providers reject with 400.
	for _, f := range anthropicOnlyFields {
		delete(body, f)
	}

	// Context truncation (round 10): fragile free upstreams queue hard
	// on oversized prompts — slide the history window down before the
	// first byte leaves. The capability gate ranked the FULL prompt; this
	// only trims what is sent. Tool-calling requests pass untouched.
	applyContextTruncation(fc.Cfg, body)

	// Apply max_tokens_cap from strategy_overrides if the strategy has one configured
	// and the client hasn't already requested fewer tokens.
	if fc.Strategy != nil {
		if overrides, ok := fc.Cfg.Proxy.StrategyOverrides[fc.Strategy.Name()]; ok {
			if capRaw, ok := overrides["max_tokens_cap"]; ok {
				var cap int
				switch v := capRaw.(type) {
				case int:
					cap = v
				case float64:
					cap = int(v)
				}
				if cap > 0 {
					current := 0
					if mt, ok := body["max_tokens"].(float64); ok {
						current = int(mt)
					}
					if current == 0 || current > cap {
						body["max_tokens"] = cap
					}
				}
			}
		}
	}

	if providerID == "groq" {
		body = BuildGroqRequest(body)
	}
	return body
}

// parseHFLoadingWait extracts the estimated_time from a HuggingFace 503 body.
// Returns 0 if not a loading response.
func parseHFLoadingWait(body []byte) int64 {
	if !strings.Contains(string(body), "loading") {
		return 0
	}
	var hfErr struct {
		EstimatedTime float64 `json:"estimated_time"`
	}
	if json.Unmarshal(body, &hfErr) == nil && hfErr.EstimatedTime > 0 {
		ms := int64(hfErr.EstimatedTime * 1000)
		if ms > 60_000 {
			ms = 60_000 // cap at 60s
		}
		return ms
	}
	return 30_000 // default 30s if not specified
}

// estimateTokens gives a rough token count for a message list (4 chars ≈ 1 token).
func estimateTokens(msgs []map[string]any) int {
	total := 0
	for _, m := range msgs {
		if content, ok := m["content"].(string); ok {
			total += len(content) / 4
		}
	}
	return total
}

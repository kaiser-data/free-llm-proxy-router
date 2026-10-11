package proxy

import (
	"bytes"
	"context"
	"encoding/json"
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
}

// Request is the parsed incoming chat-completions request.
type Request struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Stream   bool             `json:"stream,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
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
	// and backoff sleep observes it.
	ctx, cancelTotal := fc.withTotalDeadline(ctx)
	if cancelTotal != nil {
		defer cancelTotal()
	}
	resp, committed, err := fc.walk(ctx, req, fc.bufferedTry(req), fc.bufferedTryOnce(req.RequestID))
	if committed {
		// Unreachable for the buffered walk; kept for the shared signature.
		return nil, nil
	}
	return resp, err
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
	stratReq := strategy.Request{
		Messages:              req.Messages,
		EstimatedPromptTokens: estimateTokens(req.Messages),
		MaxTokens:             req.MaxTokens,
		Stream:                req.Stream,
		Model:                 req.Model,
	}

	ranked := fc.Strategy.Rank(stratReq, freeEntries, nil)
	maxAttempts := fc.maxAttemptsBudget()
	maxHops := fc.maxFallbackHops()
	hops := 0
	// attempt is the per-request upstream-call budget counter
	// (fallback.max_attempts). EVERY upstream call consumes it — the ranked
	// walk's attempts with their same-candidate retries AND the once-paths:
	// the OpenRouter models[] step, the openrouter/free ultimate fallback,
	// and the HuggingFace cold-start re-attempt (task G).
	attempt := 0

	// Step 1: Try OpenRouter with models[] array (native fallback)
	if or := fc.findProvider("openrouter"); or != nil {
		openRouterModels := fc.collectProviderModels("openrouter", ranked)
		if len(openRouterModels) > 0 {
			hops++ // the models[] request is one candidate/hop
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
				fc.ReliabilityTracker.Record("openrouter", true)
				return resp, false, nil
			}
			// Fail-fast client/request errors abort the whole chain — no
			// other step can succeed with the same payload. Auth/model
			// failures fall through to the next candidate instead
			// (three-way classification, task C).
			if finalFailureDecision(resp, err) == FailFast && resp != nil {
				log.Printf("fallback: openrouter models[] fail-fast status %d — surfacing to client, aborting chain", resp.StatusCode)
				fc.ReliabilityTracker.Record("openrouter", false)
				return resp, false, nil
			}
			log.Printf("fallback: openrouter models[] failed (%v) — continuing", err)
			fc.ReliabilityTracker.Record("openrouter", false)
		}
	}

	// Step 2–4: Try one model per provider in ranked order.
	//
	// Key invariant: once a provider returns any error, ALL remaining entries
	// for that provider are skipped immediately. This prevents a large provider
	// (e.g. NVIDIA NIM with 172 models) from exhausting maxAttempts before
	// faster, more reliable providers (Groq, Cerebras, Gemini) are even tried.
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
		if hops >= maxHops {
			return nil, false, fmt.Errorf("fallback hop ceiling %d reached — aborting walk", maxHops)
		}
		hops++

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
		resp, committed, err := try(ctx, callCfg, body, &attempt)

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
						fc.ReliabilityTracker.Record(r.ProviderID, true)
						return resp, committed, nil
					}
				}
			}
			// Apply per-status cooldown logic.
			if resp != nil {
				switch resp.StatusCode {
				case 429:
					fc.Catalog.MarkNeedsReverification(r.ProviderID, r.ModelID)
					info := ratelimit.ExtractRateLimitInfo(r.ProviderID, resp.Header, resp.Body)
					waitDur := info.WaitDuration()
					until := time.Now().Add(waitDur)
					// Cooldown on the specific model, not the whole provider.
					fc.RateLimiter.SetCooldown(r.ProviderID+"/"+r.ModelID, until)
					// Rotate to the next API key slot if multiple keys are configured.
					if providerCfg != nil && providerCfg.NumKeys() > 1 {
						newIdx := fc.RateLimiter.RotateKey(r.ProviderID, providerCfg.NumKeys())
						log.Printf("fallback: %s/%s rate limited — rotated to key slot %d", r.ProviderID, r.ModelID, newIdx+1)
					} else {
						log.Printf("fallback: %s/%s rate limited — cooldown until %s", r.ProviderID, r.ModelID, until.Format("15:04:05"))
					}
				case 401, 403:
					fc.RateLimiter.SetCooldown(r.ProviderID, time.Now().Add(10*time.Minute))
					log.Printf("fallback: %s auth error %d — cooldown 10min", r.ProviderID, resp.StatusCode)
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
			if finalFailureDecision(resp, err) == FailFast {
				if resp != nil {
					log.Printf("fallback: %s/%s fail-fast status %d — surfacing to client, aborting chain", r.ProviderID, r.ModelID, resp.StatusCode)
					fc.ReliabilityTracker.Record(r.ProviderID, false)
					return resp, false, nil
				}
				return nil, false, err // context canceled — client is gone
			}
			statusCode := 0
			if resp != nil {
				statusCode = resp.StatusCode
			}
			log.Printf("fallback: %s/%s status %d — skipping provider", r.ProviderID, r.ModelID, statusCode)
			fc.ReliabilityTracker.Record(r.ProviderID, false)
			if fc.ReliabilityTracker.ShouldCooldown(r.ProviderID) {
				fc.RateLimiter.SetCooldown(r.ProviderID, time.Now().Add(30*time.Second))
				log.Printf("fallback: %s reliability below 50%% â short cooldown", r.ProviderID)
			}
			failedProviders[r.ProviderID] = true // don't try other models from this provider
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
		fc.ReliabilityTracker.Record(r.ProviderID, true)
		if r.ProviderID == "gemini" && fc.GeminiTracker != nil {
			fc.GeminiTracker.RecordRequest(r.ModelID, stratReq.EstimatedPromptTokens)
		}
		return resp, committed, nil
	}

	// Step 5: Ultimate fallback — OpenRouter free router. It is one more
	// upstream call: it consumes the attempt budget like every other path
	// (task G) and only runs while the budget lasts.
	if or := fc.findProvider("openrouter"); or != nil && attempt < maxAttempts {
		if hops >= maxHops {
			return nil, false, fmt.Errorf("fallback hop ceiling %d reached — aborting walk", maxHops)
		}
		hops++
		log.Printf("fallback: trying openrouter/free ultimate fallback")
		body := OpenRouterFreeRouterRequest(req.Raw)
		if req.Stream {
			body["stream"] = true // streaming walk keeps SSE end-to-end (NG3)
		}
		resp, committed, err := tryOnce(ctx, *or, body, &attempt)
		if committed {
			return nil, true, nil
		}
		if err == nil && resp.StatusCode == http.StatusOK {
			fc.ReliabilityTracker.Record("openrouter", true)
			return resp, false, nil
		}
		fc.ReliabilityTracker.Record("openrouter", false)
		// A fail-fast error from the last step still surfaces the real
		// upstream response instead of degrading into the exhaustion path.
		if finalFailureDecision(resp, err) == FailFast && resp != nil {
			log.Printf("fallback: openrouter/free fail-fast status %d — surfacing to client", resp.StatusCode)
			return resp, false, nil
		}
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

// retrySameCandidate retries the SAME provider/model up to
// fallback.retries_per_candidate times, sleeping between attempts with
// ratelimit.ExponentialBackoff + DefaultBackoffParams (exponential backoff
// with jitter — previously dead code, gap G4). The three-way classifier
// (ClassifyFailure) now owns the failure classes the old two-gate
// (classifyUpstreamFailure + sameCandidateRetryAllowed) used to split:
// 429 follows fallback.retry_on_429, 5xx follows fallback.retry_on_5xx,
// transport errors and 408 retry while retries remain, 404/401/403 and
// genuine payload errors fall straight through to the walk. A backoff sleep
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
		RetryOn429:      fc.Cfg.Fallback.RetryOn429,
		RetryOn5xx:      fc.Cfg.Fallback.RetryOn5xx,
		RemainingBudget: remainingBudget(ctx),
		TimeoutsDone:    timeouts,
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

	resp, err := fc.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
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

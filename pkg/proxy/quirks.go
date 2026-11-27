package proxy

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
)

// providerQuirks holds what one provider does differently from the generic
// OpenAI-compatible path. Every hook is optional; a provider without an entry
// in providerQuirksByID behaves generically. callOnce and attempt call these
// hooks at fixed points and know no provider by name, so changing how one
// provider handles a quota error is a change here (and in quirks_test.go),
// not in the retry loop.
type providerQuirks struct {
	// admit runs before every upstream call. A non-nil skip returns that
	// result without calling. A non-nil release is called unless the model
	// answered (a quota reservation handed back).
	admit func(ctx context.Context, env quirkEnv, modelID string, estTokens int) (release func(), skip *attemptResult)
	// retryWait looks at a failed answer and returns how long to wait before
	// calling the same model once more (0: no retry).
	retryWait func(resp *Response) time.Duration
	// onSuccess observes a 200 (rate-limit headers that ask to slow down).
	onSuccess func(env quirkEnv, resp *Response)
	// usageModel names the model a 200 is counted under in usage tracking.
	usageModel func(modelID string, resp *Response) string
	// rateLimit decides a 429's cooldown and whether it covers every model
	// of the provider instead of the one key slot. Default: defaultRateLimit.
	rateLimit func(resp *Response) (wait time.Duration, wholeProvider bool)
}

// quirkEnv is the shared state a quirk may use. It is passed by value rather
// than as *FallbackChain: handing the chain to a function value would make
// it escape, costing every request an allocation.
type quirkEnv struct {
	rateLimiter   *ratelimit.GlobalTracker
	geminiTracker *ratelimit.GeminiTracker
	// cerebrasSpacing is fallback.cerebras_request_spacing_ms.
	cerebrasSpacing time.Duration
}

func (fc *FallbackChain) quirkEnv() quirkEnv {
	return quirkEnv{
		rateLimiter:     fc.RateLimiter,
		geminiTracker:   fc.GeminiTracker,
		cerebrasSpacing: time.Duration(fc.Cfg.Fallback.CerebrasRequestSpacingMs) * time.Millisecond,
	}
}

// providerQuirksByID is keyed by the config provider ID.
var providerQuirksByID = map[string]providerQuirks{
	"cerebras": {
		admit:     cerebrasAdmit,
		onSuccess: cerebrasOnSuccess,
	},
	"gemini": {
		admit: geminiAdmit,
	},
	"huggingface": {
		retryWait: huggingFaceRetryWait,
	},
	"openrouter": {
		usageModel: openRouterUsageModel,
	},
}

func quirksFor(providerID string) providerQuirks {
	return providerQuirksByID[providerID]
}

// rateLimitCooldown is the 429 cooldown for providerID: the provider's own
// rule if it has one, else defaultRateLimit.
func (q providerQuirks) rateLimitCooldown(providerID string, resp *Response) (time.Duration, bool) {
	if q.rateLimit != nil {
		return q.rateLimit(resp)
	}
	return defaultRateLimit(providerID, resp), false
}

// usageModelFor is the model a successful call is counted under.
func (q providerQuirks) usageModelFor(modelID string, resp *Response) string {
	if q.usageModel != nil {
		return q.usageModel(modelID, resp)
	}
	return modelID
}

// defaultRateLimit waits as long as the provider's rate-limit headers or
// body say, and a day when the quota is "limit: 0" (the model is listed but
// not on this key's free tier, so retrying soon is pointless).
func defaultRateLimit(providerID string, resp *Response) time.Duration {
	if isZeroQuota(resp.Body) {
		return zeroQuotaCooldown
	}
	return ratelimit.ExtractRateLimitInfo(providerID, resp.Header, resp.Body).WaitDuration()
}

// cerebrasAdmit applies the RPS-level pre-emptive check (finer than a
// cooldown) and the request spacing shared by all concurrent requests.
func cerebrasAdmit(ctx context.Context, env quirkEnv, modelID string, _ int) (func(), *attemptResult) {
	tracker := env.rateLimiter.Provider("cerebras")
	if !tracker.CanRequest(modelID, 0, 0) {
		log.Printf("fallback: cerebras pre-emptive skip (rate limited)")
		return nil, &attemptResult{outcome: outSkipProvider}
	}
	if env.cerebrasSpacing > 0 {
		if err := sleepCtx(ctx, tracker.NextSlot(env.cerebrasSpacing)); err != nil {
			return nil, &attemptResult{err: err}
		}
	}
	return nil, nil
}

// cerebrasOnSuccess slows down pre-emptively when the headers say fewer
// than two requests are left in the window.
func cerebrasOnSuccess(env quirkEnv, resp *Response) {
	info := ratelimit.ExtractRateLimitInfo("cerebras", resp.Header, resp.Body)
	if info.RemainingRequests >= 0 && info.RemainingRequests < 2 {
		env.rateLimiter.Provider("cerebras").PreemptiveSlowdown(info.WaitDuration())
	}
}

// geminiAdmit checks and counts the per-model quota in one step, so
// concurrent requests cannot overshoot it. The slot is handed back unless
// the model actually answered.
func geminiAdmit(_ context.Context, env quirkEnv, modelID string, estTokens int) (func(), *attemptResult) {
	if env.geminiTracker == nil {
		return nil, nil
	}
	release, ok := env.geminiTracker.Reserve(modelID, estTokens)
	if !ok {
		log.Printf("fallback: gemini %s over local quota — skipping model", modelID)
		return nil, &attemptResult{outcome: outSkipModel}
	}
	return release, nil
}

// huggingFaceRetryWait waits out a cold start: a 503 whose body says the
// model is loading is retried once on the same model.
func huggingFaceRetryWait(resp *Response) time.Duration {
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		return 0
	}
	return time.Duration(parseHFLoadingWait(resp.Body)) * time.Millisecond
}

// openRouterUsageModel counts a call under the model OpenRouter actually
// used (its models[] fallback may pick another), except for the
// openrouter/free router, which is tracked as itself.
func openRouterUsageModel(modelID string, resp *Response) string {
	if resp.UsedModel != "" && modelID != "openrouter/free" {
		return resp.UsedModel
	}
	return modelID
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

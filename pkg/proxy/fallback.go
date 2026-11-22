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
//  2. Groq (service_tier only if configured; free orgs reject "auto")
//  3. Gemini (4-dimensional rate limit check)
//  4. Remaining providers (sorted by strategy)
//  5. OpenRouter with model: "openrouter/free" (ultimate fallback)
//
// Special cases:
//   - HuggingFace 503+loading: wait estimated_time, retry SAME provider
//   - Cerebras: pre-emptive slowdown if remaining-requests < 2
type FallbackChain struct {
	Cfg             *config.Config
	Strategy        strategy.Strategy
	Catalog         *catalog.Catalog
	// Free is Catalog.FreeEntries() computed once per catalog load and shared
	// by every request; it must not be modified. Nil computes it per call.
	Free        []catalog.CatalogEntry
	RateLimiter    *ratelimit.GlobalTracker
	GeminiTracker   *ratelimit.GeminiTracker
	ReliabilityTracker *reliability.Tracker
	HTTPClient      *http.Client
	// Filters drop candidates that cannot serve the request (proxy.filters),
	// built once per config load. Nil filters nothing.
	Filters strategy.Filters
	// Stream asks providers for SSE. A provider's stream is committed to the
	// client at its first content chunk; everything before that still falls
	// back. Gemini is always buffered (its thoughts are stripped from the
	// full body).
	Stream bool
}

// Request is the parsed incoming chat-completions request.
type Request struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Stream   bool             `json:"stream,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	// Raw holds the original request body for passthrough.
	Raw map[string]any `json:"-"`
}

// Response wraps an upstream HTTP response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	// UsedModel is the model that actually served the request (from response body).
	UsedModel string
	// Truncated marks an empty finish_reason=length reply returned only because
	// nothing better answered; it must not be cached.
	Truncated bool

	// stream is set when the provider answered with SSE and content is
	// flowing; Body is then unused and the caller must write or Close it.
	stream *upstreamStream
	// emptyStream: the provider's stream ended without any content.
	emptyStream bool
}

// Close releases a committed upstream stream, if any.
func (r *Response) Close() {
	if r != nil && r.stream != nil {
		r.stream.Close()
	}
}

// withRequestTimeout applies fallback.request_timeout to ctx.
func (fc *FallbackChain) withRequestTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if d := fc.Cfg.Fallback.RequestTimeout; d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return context.WithCancel(ctx)
}

// Execute runs the fallback chain for the given request.
// Returns the first successful response or an error if all providers fail.
func (fc *FallbackChain) Execute(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := fc.withRequestTimeout(ctx)
	resp, err := fc.execute(ctx, req)
	if resp != nil && resp.stream != nil {
		// The request deadline keeps bounding the stream; it ends with it.
		resp.stream.onClose(cancel)
	} else {
		cancel()
	}
	return resp, err
}

func (fc *FallbackChain) execute(ctx context.Context, req Request) (*Response, error) {
	freeEntries := fc.Free
	if freeEntries == nil {
		freeEntries = fc.Catalog.FreeEntries()
	}
	stratReq := strategy.Request{
		Messages:              req.Messages,
		EstimatedPromptTokens: estimateTokens(req.Messages),
		MaxTokens:             req.MaxTokens,
		Stream:                req.Stream,
		Model:                 req.Model,
		HasTools:              hasTools(req.Raw),
	}
	est := stratReq.EstimatedPromptTokens

	// Filters drop models that cannot serve the request before ranking. If
	// none is left, only the openrouter/free router below can still answer.
	candidates := fc.Filters.Apply(stratReq, freeEntries)
	filteredOut := len(candidates) == 0 && len(freeEntries) > 0
	if filteredOut {
		log.Printf("fallback: filters %v left no candidate model", fc.Filters.Names())
	}

	ranked := fc.Strategy.Rank(stratReq, candidates, nil)
	maxAttempts := fc.Cfg.Fallback.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	// Try one model per provider in ranked order.
	//
	// Key invariant: once a provider fails at provider level, ALL remaining
	// entries for it are skipped, and model-level failures are capped at
	// maxModelsPerProvider. This keeps a large provider (NVIDIA NIM, 70+
	// models) from using up maxAttempts before Groq, Cerebras or Gemini.
	//
	// OpenRouter is called once, with all its ranked models in a models[]
	// array (native fallback), at the position of its best-ranked entry, so a
	// strategy that ranks Groq or Cerebras first gets them first.
	attempts := 0
	openRouterTried := false
	failedProviders := make(map[string]bool)
	providerTries := make(map[string]int)
	var lastTruncated *Response // empty finish_reason=length reply, used only if nothing better answers
	// If every upstream call rejected the request itself (400/413/422), the
	// client gets that rejection, not a retryable 503: retrying cannot help.
	//
	// Only when that is certain: a provider skipped for cooldown or quota
	// might have accepted the request, so then one provider's 400 (its own
	// quirk) stays a 503 unless a second provider rejected the request too.
	var lastRejection *Response
	otherFailures, skipped := false, false
	rejectedBy := map[string]bool{}
	noteFailure := func(providerID string, res attemptResult) {
		if res.calls == 0 {
			skipped = true // cooldown, local quota, every key rejected
			return
		}
		if isRequestRejection(res.resp) {
			lastRejection = res.resp
			rejectedBy[providerID] = true
		} else {
			otherFailures = true
		}
	}
	for _, r := range ranked {
		if attempts >= maxAttempts {
			break
		}
		if failedProviders[r.ProviderID] {
			continue
		}
		modelID := r.ModelID
		body := (map[string]any)(nil)
		if r.ProviderID == "openrouter" {
			if openRouterTried {
				continue
			}
			openRouterTried = true
			orModels := fc.availableModels("openrouter", ranked)
			if len(orModels) == 0 {
				continue
			}
			modelID = orModels[0]
			body = fc.buildBody("openrouter", req.Raw, modelID)
			body["models"] = orModels
		} else {
			body = fc.buildBody(r.ProviderID, req.Raw, modelID)
		}

		res := fc.attempt(ctx, r.ProviderID, modelID, body, est)
		attempts += res.calls
		if res.err != nil {
			return nil, res.err // request deadline or client gone
		}
		if res.outcome != outSuccess {
			noteFailure(r.ProviderID, res)
		}
		switch res.outcome {
		case outSuccess:
			return res.resp, nil
		case outSkipModel:
			continue
		case outSkipProvider, outProviderFail:
			failedProviders[r.ProviderID] = true
			continue
		case outEmpty:
			if isEmptyTruncated(res.resp.Body) {
				lastTruncated = res.resp
			}
		}
		// outModelFail / outEmpty: the provider is fine, try its next model.
		providerTries[r.ProviderID]++
		if r.ProviderID == "openrouter" || providerTries[r.ProviderID] >= maxModelsPerProvider {
			failedProviders[r.ProviderID] = true
		}
	}

	// Ultimate fallback: OpenRouter's free router picks any free model. It
	// runs even after maxAttempts, as the one designed last resort.
	//
	// The filters judge it like any model. It has no context window or
	// supported_parameters, so it runs only where the filters keep unknowns.
	if fc.findProvider("openrouter") != nil && !failedProviders["openrouter"] &&
		fc.Filters.Keeps(stratReq, openRouterFreeEntry) {
		log.Printf("fallback: trying openrouter/free ultimate fallback")
		body := OpenRouterFreeRouterRequest(fc.buildBody("openrouter", req.Raw, "openrouter/free"))
		if stratReq.HasTools && len(fc.Filters) > 0 {
			requireParameters(body)
		}
		res := fc.attempt(ctx, "openrouter", "openrouter/free", body, est)
		attempts += res.calls
		if res.err != nil {
			return nil, res.err
		}
		if res.outcome == outSuccess {
			return res.resp, nil
		}
		noteFailure("openrouter", res)
	}

	if lastTruncated != nil {
		lastTruncated.Truncated = true
		return lastTruncated, nil
	}
	if lastRejection != nil && !otherFailures && (!skipped || len(rejectedBy) >= 2) {
		return lastRejection, nil
	}
	if filteredOut {
		return nil, fmt.Errorf("%w (filters: %v)", strategy.ErrNoCandidates, fc.Filters.Names())
	}
	return nil, fmt.Errorf("all providers exhausted after %d attempts", attempts)
}

// openRouterFreeEntry stands for the openrouter/free router when the filters
// judge it: it picks the model itself, so nothing about it is known.
var openRouterFreeEntry = catalog.CatalogEntry{ProviderID: "openrouter", ModelID: "openrouter/free", IsFree: true}

// requireParameters asks OpenRouter to route only to providers that support
// every request parameter, so a tool request is not silently answered by a
// provider that ignores the tools.
func requireParameters(body map[string]any) {
	prov, _ := body["provider"].(map[string]any)
	prov = copyMap(prov) // the client's own provider object stays untouched
	prov["require_parameters"] = true
	body["provider"] = prov
}

// availableModels returns a provider's ranked models that are not on cooldown.
func (fc *FallbackChain) availableModels(providerID string, ranked []strategy.RankedEntry) []string {
	var out []string
	for _, m := range fc.collectProviderModels(providerID, ranked) {
		if !fc.RateLimiter.IsOnCooldown(providerID + "/" + m) {
			out = append(out, m)
		}
	}
	return out
}

// outcome says what one attempt on a provider/model means for the chain.
type outcome int

const (
	outSkipModel    outcome = iota // not called: this model is unavailable (cooldown, local quota)
	outSkipProvider                // not called: the whole provider is unavailable
	outSuccess                     // a usable completion
	outEmpty                       // 200 without content: try another model
	outModelFail                   // this model failed; the provider's other models may answer
	outProviderFail                // the provider rejects this request or is unusable: skip it
)

type attemptResult struct {
	resp    *Response
	outcome outcome
	calls   int   // upstream calls made (counted against max_attempts)
	err     error // set only when ctx ended; the chain must stop
}

// cooldownKey is the rate-limit cooldown key for one model on one key slot.
// With a single key it is "provider/model", so a model-wide cooldown (404)
// and a 429 share the key.
func cooldownKey(providerID, modelID string, slot, numKeys int) string {
	if numKeys <= 1 {
		return providerID + "/" + modelID
	}
	return fmt.Sprintf("%s/%s#%d", providerID, modelID, slot+1)
}

// attempt is the single path every upstream chat call goes through — the
// strategy chain, OpenRouter's models[] call and the direct-model route:
// admit (cooldowns, local quotas, spacing) → reserve → call → record outcome.
//
// A 429 cools down only the key slot that hit it, so the same model is tried
// again right away on the next free key slot.
func (fc *FallbackChain) attempt(ctx context.Context, providerID, modelID string, body map[string]any, estTokens int) attemptResult {
	providerCfg := fc.findProvider(providerID)
	if providerCfg == nil {
		return attemptResult{outcome: outSkipProvider}
	}
	if fc.RateLimiter.IsOnCooldown(providerID) {
		log.Printf("fallback: %s on cooldown — skipping provider", providerID)
		return attemptResult{outcome: outSkipProvider}
	}
	if fc.RateLimiter.IsOnCooldown(providerID + "/" + modelID) {
		log.Printf("fallback: %s/%s on cooldown — skipping model", providerID, modelID)
		return attemptResult{outcome: outSkipModel}
	}

	keys := providerCfg.AllKeys()
	numSlots := len(keys)
	if numSlots == 0 {
		numSlots = 1 // keyless provider (local Ollama) or no key configured
	}
	calls := 0
	var last attemptResult
	triedSlot := false
	for i := 0; i < numSlots; i++ {
		slot := i // fill keys in order; a slot on cooldown is skipped without a call
		if fc.RateLimiter.IsOnCooldown(cooldownKey(providerID, modelID, slot, len(keys))) ||
			fc.RateLimiter.IsOnCooldown(authCooldownKey(providerID, slot, len(keys))) {
			continue
		}
		triedSlot = true
		callCfg := *providerCfg
		if len(keys) > 0 {
			// Always send the chosen key, so a config with only KEY_2 set works.
			callCfg.APIKey = keys[slot]
			callCfg.APIKeyEnv = ""
		}
		res := fc.callOnce(ctx, callCfg, providerID, modelID, body, estTokens)
		calls += res.calls
		res.calls = calls
		if isKeyRejection(res.resp) {
			// A rejected key says nothing about the provider's other keys:
			// cool down this key for every model and try the next one.
			fc.RateLimiter.SetCooldown(authCooldownKey(providerID, slot, len(keys)), time.Now().Add(authCooldown))
			log.Printf("fallback: %s auth error %d on key slot %d — key cooldown %s", providerID, res.resp.StatusCode, slot+1, authCooldown)
			last = res
			continue
		}
		if res.outcome != outModelFail || res.resp == nil || res.resp.StatusCode != http.StatusTooManyRequests {
			return res
		}
		// 429: cool down this key slot only and try the next one.
		fc.cooldown429(providerID, modelID, slot, len(keys), res.resp)
		if len(keys) > 1 {
			log.Printf("fallback: %s/%s rate limited on key slot %d — trying next key", providerID, modelID, slot+1)
		}
		last = res
	}
	if !triedSlot {
		if fc.allKeysRejected(providerID, numSlots, len(keys)) {
			return attemptResult{outcome: outSkipProvider}
		}
		log.Printf("fallback: %s/%s rate limited on every key — skipping model", providerID, modelID)
		return attemptResult{outcome: outSkipModel}
	}
	return last
}

// allKeysRejected reports whether every key slot of a provider is cooling down
// after a 401/403, so the provider can be skipped without walking its models.
func (fc *FallbackChain) allKeysRejected(providerID string, numSlots, numKeys int) bool {
	for slot := 0; slot < numSlots; slot++ {
		if !fc.RateLimiter.IsOnCooldown(authCooldownKey(providerID, slot, numKeys)) {
			return false
		}
	}
	return true
}

// authCooldownKey is the cooldown key for a key slot whose key was rejected
// (401/403). It spans all models. With a single key it is the provider itself,
// so a provider whose only key is bad sits out entirely.
func authCooldownKey(providerID string, slot, numKeys int) string {
	if numKeys <= 1 {
		return providerID
	}
	return fmt.Sprintf("%s#key%d", providerID, slot+1)
}

// cooldown429 puts one key slot of a model on cooldown after a 429.
func (fc *FallbackChain) cooldown429(providerID, modelID string, slot, numKeys int, resp *Response) {
	info := ratelimit.ExtractRateLimitInfo(providerID, resp.Header, resp.Body)
	waitDur := info.WaitDuration()
	if isZeroQuota(resp.Body) {
		// Quota "limit: 0" means the model is not on this key's free tier.
		waitDur = zeroQuotaCooldown
	}
	fc.RateLimiter.SetCooldown(cooldownKey(providerID, modelID, slot, numKeys), time.Now().Add(waitDur))
	log.Printf("fallback: %s/%s rate limited — cooldown %s", providerID, modelID, waitDur.Round(time.Second))
}

// callOnce makes one upstream call with a fixed key and classifies it.
func (fc *FallbackChain) callOnce(ctx context.Context, callCfg config.ProviderConfig, providerID, modelID string, body map[string]any, estTokens int) attemptResult {
	// Cerebras pre-emptive check (RPS-level, finer than cooldown) and spacing
	// shared by all concurrent requests.
	if providerID == "cerebras" {
		tracker := fc.RateLimiter.Provider("cerebras")
		if !tracker.CanRequest(modelID, 0, 0) {
			log.Printf("fallback: cerebras pre-emptive skip (rate limited)")
			return attemptResult{outcome: outSkipProvider}
		}
		if ms := fc.Cfg.Fallback.CerebrasRequestSpacingMs; ms > 0 {
			if err := sleepCtx(ctx, tracker.NextSlot(time.Duration(ms)*time.Millisecond)); err != nil {
				return attemptResult{err: err}
			}
		}
	}

	// Gemini: check and count in one step, so concurrent requests cannot
	// overshoot the per-model quota. The slot is handed back unless the
	// model actually answered.
	keepReservation := false
	if providerID == "gemini" && fc.GeminiTracker != nil {
		release, ok := fc.GeminiTracker.Reserve(modelID, estTokens)
		if !ok {
			log.Printf("fallback: gemini %s over local quota — skipping model", modelID)
			return attemptResult{outcome: outSkipModel}
		}
		defer func() {
			if !keepReservation {
				release()
			}
		}()
	}

	resp, err := fc.callProvider(ctx, callCfg, body)
	calls := 1
	// HuggingFace 503+loading: wait and retry the same model (cold start).
	if err == nil && providerID == "huggingface" && resp.StatusCode == http.StatusServiceUnavailable {
		if waitMs := parseHFLoadingWait(resp.Body); waitMs > 0 {
			log.Printf("fallback: huggingface model loading, waiting %dms", waitMs)
			if serr := sleepCtx(ctx, time.Duration(waitMs)*time.Millisecond); serr != nil {
				return attemptResult{calls: calls, err: serr}
			}
			resp, err = fc.callProvider(ctx, callCfg, body)
			calls++
		}
	}
	if ctx.Err() != nil {
		// Deadline or client went away; not the provider's fault.
		resp.Close()
		return attemptResult{calls: calls, err: ctx.Err()}
	}

	if err == nil && resp.StatusCode == http.StatusOK {
		keepReservation = true
		// Counted per model per day; this is what usage.json persists.
		recordModel := modelID
		if providerID == "openrouter" && resp.UsedModel != "" && modelID != "openrouter/free" {
			recordModel = resp.UsedModel
		}
		fc.RateLimiter.Provider(providerID).RecordRequest(recordModel)
		if providerID == "cerebras" {
			info := ratelimit.ExtractRateLimitInfo("cerebras", resp.Header, resp.Body)
			if info.RemainingRequests >= 0 && info.RemainingRequests < 2 {
				fc.RateLimiter.Provider("cerebras").PreemptiveSlowdown(info.WaitDuration())
			}
		}
		if resp.emptyStream || (resp.stream == nil && isEmptyCompletion(resp.Body)) {
			// Reasoning models (gpt-oss) can spend a small max_tokens budget on
			// hidden reasoning, and some free endpoints return 200 with nothing
			// in it. Neither is an answer: try another model.
			log.Printf("fallback: %s/%s returned an empty completion — trying next model", providerID, modelID)
			return attemptResult{resp: resp, outcome: outEmpty, calls: calls}
		}
		fc.ReliabilityTracker.Record(providerID, true)
		return attemptResult{resp: resp, outcome: outSuccess, calls: calls}
	}

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	switch {
	case resp == nil, isKeyRejection(resp):
		// Only transport and auth failures mean the provider itself is
		// unusable, so only they count against its reliability.
		if status == 0 {
			log.Printf("fallback: %s/%s transport error — skipping provider%s", providerID, modelID, upstreamErrSnippet(resp, err))
		}
		fc.ReliabilityTracker.Record(providerID, false)
		if fc.ReliabilityTracker.ShouldCooldown(providerID) {
			fc.RateLimiter.SetCooldown(providerID, time.Now().Add(30*time.Second))
			log.Printf("fallback: %s reliability below 50%% — short cooldown", providerID)
		}
		return attemptResult{resp: resp, outcome: outProviderFail, calls: calls}
	case status == http.StatusForbidden && isConsent403(resp.Body):
		// Provider: UnoRouter (verified 2026-11-20): a 403 consent_required
		// means the account has not consented to this model's upstream
		// provider yet (unorouter.com/consents). The key is fine and models
		// behind already-consented providers still answer, so this cools the
		// model only — cooling the key would hide them all.
		fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(consentCooldown))
		log.Printf("fallback: %s/%s needs upstream consent (403 consent_required) — model cooldown %s, trying next model%s", providerID, modelID, consentCooldown, upstreamErrSnippet(resp, err))
	case status == http.StatusForbidden && isUpstreamCredit403(resp.Body):
		// Provider: UnoRouter (verified 2026-11-20): "This provider ran out
		// of credit on their side. That is on us, not you: retry and the
		// request goes to another provider." Upstream capacity, not the key
		// — a short model cooldown like a 5xx instead of cooling the key.
		fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(serverErrorCooldown))
		fc.ReliabilityTracker.Record(providerID, false)
		log.Printf("fallback: %s/%s status 403 — upstream out of credit, model cooldown %s, trying next model%s", providerID, modelID, serverErrorCooldown, upstreamErrSnippet(resp, err))
	case status == http.StatusBadRequest && isModelSpecific400(resp.Body):
		// The 400 blames the model (decommissioned, "only supports
		// Interactions API"): it sits out, and the provider's next model can
		// answer this very request.
		fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(badRequestCooldown))
		log.Printf("fallback: %s/%s unusable (400) — model cooldown %s, trying next model%s", providerID, modelID, badRequestCooldown, upstreamErrSnippet(resp, err))
	case isRequestRejection(resp):
		// The provider rejects this request's shape, size or content (a
		// moderation 403); its other models most likely will too, so skip the
		// provider for this request. Not a health problem: no reliability
		// penalty, no cooldown.
		log.Printf("fallback: %s/%s status %d — provider rejects this request, skipping provider%s", providerID, modelID, status, upstreamErrSnippet(resp, err))
		return attemptResult{resp: resp, outcome: outProviderFail, calls: calls}
	case status == 404:
		fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(missingModelCooldown))
		log.Printf("fallback: %s model %s not found — model cooldown %s", providerID, modelID, missingModelCooldown)
	case status >= 500:
		// An overloaded or failing model: let it rest briefly so concurrent
		// requests stop spending attempts on it, and count it against the
		// provider (a provider failing every model gets a short cooldown).
		fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(serverErrorCooldown))
		fc.ReliabilityTracker.Record(providerID, false)
		if fc.ReliabilityTracker.ShouldCooldown(providerID) {
			fc.RateLimiter.SetCooldown(providerID, time.Now().Add(30*time.Second))
		}
		log.Printf("fallback: %s/%s status %d — model cooldown %s, trying next model%s", providerID, modelID, status, serverErrorCooldown, upstreamErrSnippet(resp, err))
	case status != http.StatusTooManyRequests:
		// Free tiers often fail one model (quota, retired, overloaded) while
		// the provider's other models still answer.
		log.Printf("fallback: %s/%s status %d — trying next model%s", providerID, modelID, status, upstreamErrSnippet(resp, err))
	}
	return attemptResult{resp: resp, outcome: outModelFail, calls: calls}
}

func (fc *FallbackChain) callProvider(ctx context.Context, cfg config.ProviderConfig, body map[string]any) (*Response, error) {
	// The provider timeout bounds a buffered answer, and a stream only up to
	// its first content chunk: once tokens flow, a long answer from a slow
	// model may take longer, and the request deadline bounds it instead.
	ctx, cancelCall := context.WithCancel(ctx)
	stopTimer := func() bool { return true }
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		stopTimer = time.AfterFunc(d, cancelCall).Stop
	}
	committed := false
	defer func() {
		if !committed {
			stopTimer()
			cancelCall()
		}
	}()
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"

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

	if body["stream"] == true {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := fc.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !committed {
			resp.Body.Close()
		}
	}()
	if resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		out, err := peekStream(resp.StatusCode, resp.Header, resp.Body)
		if err != nil || out.stream == nil {
			return out, err
		}
		// Committed: the stream owns the connection; the provider timeout ends.
		if !stopTimer() {
			// The provider timeout fired just as the first chunk came in:
			// the call's context is already cancelled.
			return nil, context.DeadlineExceeded
		}
		committed = true
		out.stream.onClose(func() { resp.Body.Close() })
		out.stream.onClose(cancelCall)
		return out, nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if cfg.ID == "gemini" && resp.StatusCode == http.StatusOK {
		respBody = StripGeminiThoughts(respBody)
	}

	// Extract the actual model used (OpenRouter fills this in)
	usedModel := ""
	if resp.StatusCode == http.StatusOK {
		var parsed struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(respBody, &parsed) == nil {
			usedModel = parsed.Model
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
	return findProviderCfg(fc.Cfg, id)
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
	// copyMap is shallow: body["messages"] would still alias the client's
	// parsed request, and every provider attempt builds from the same raw.
	// Without a private copy, a transform that writes into a message (a
	// tool-call rewrite for one provider's quirk, say) leaks into the next
	// provider's attempt and mutates the caller's request in place.
	if msgs, ok := body["messages"].([]any); ok {
		body["messages"] = deepCopySlice(msgs)
	}
	body["model"] = modelID
	if fc.Stream && providerID != "gemini" {
		body["stream"] = true
	} else {
		// Buffered: the chain parses the JSON answer. stream_options is only
		// valid with stream:true and strict backends reject it otherwise.
		delete(body, "stream")
		delete(body, "stream_options")
	}

	// Strip Anthropic-specific fields that other providers reject with 400.
	for _, f := range anthropicOnlyFields {
		delete(body, f)
	}

	normalizeMaxTokens(body)

	// Apply max_tokens_cap from strategy_overrides if the strategy has one configured
	// and the client hasn't already requested fewer tokens.
	if fc.Strategy != nil {
		if overrides, ok := fc.Cfg.Proxy.StrategyOverrides[fc.Strategy.Name()]; ok {
			if capRaw, ok := overrides["max_tokens_cap"]; ok {
				cap, _ := toInt(capRaw)
				if cap > 0 {
					current, _ := toInt(body["max_tokens"])
					if current == 0 || current > cap {
						body["max_tokens"] = cap
					}
				}
			}
		}
	}

	// Provider-native service tier (e.g. Groq), only when explicitly configured:
	// Groq rejects service_tier "auto" for free-tier orgs with a 400.
	if pc := fc.findProvider(providerID); pc != nil && pc.NativeFeatures.ServiceTier != "" {
		body["service_tier"] = pc.NativeFeatures.ServiceTier
	}
	return body
}

const (
	// maxModelsPerProvider bounds how many models of one provider a single
	// request tries after model-level errors (anything but auth/transport), so a large catalog
	// such as NVIDIA NIM cannot use up every attempt.
	maxModelsPerProvider = 3
	// zeroQuotaCooldown applies when a 429 reports a quota "limit: 0": the
	// model is listed but not on the free tier, so retrying soon is pointless.
	zeroQuotaCooldown = 24 * time.Hour
	// missingModelCooldown applies when a provider returns 404 for a model.
	missingModelCooldown = time.Hour
	// badRequestCooldown applies to a model whose 400 names the model itself
	// (see isModelSpecific400).
	badRequestCooldown = time.Hour
	// authCooldown applies to a key slot (or a single-key provider) after 401/403.
	authCooldown = 10 * time.Minute
	// consentCooldown applies to a model whose upstream provider awaits the
	// account's consent (Provider: UnoRouter, 403 consent_required). Consent
	// is given by a human on the provider's site, so retrying sooner than an
	// hour cannot succeed; the model stays cataloged for after consenting.
	consentCooldown = time.Hour
	// serverErrorCooldown applies to a model after a 5xx.
	serverErrorCooldown = 30 * time.Second
)

// isRequestRejection reports whether the provider rejected this request
// itself (its shape, size or content) rather than being unavailable: a 400
// that does not name the model, 413, 422, or a moderation 403.
func isRequestRejection(resp *Response) bool {
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return !isModelSpecific400(resp.Body)
	case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	case http.StatusForbidden:
		return isModeration403(resp.Body)
	}
	return false
}

// isKeyRejection reports whether a response rejects the API key (401, or a
// 403 that is not about the request's content, its consent gate, or the
// upstream provider's own credit).
func isKeyRejection(resp *Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized ||
		(resp.StatusCode == http.StatusForbidden && !isModeration403(resp.Body) && !isConsent403(resp.Body) && !isUpstreamCredit403(resp.Body))
}

// isConsent403 reports whether a 403 asks for the account's consent to an
// upstream provider (Provider: UnoRouter, verified 2026-11-20:
// `{"error":{"code":"consent_required","message":"…needs your consent for
// these providers: …"}}`). The key is valid — cooling it down would hide
// every model behind already-consented providers.
func isConsent403(body []byte) bool {
	return bytes.Contains(body, []byte("consent_required"))
}

// isUpstreamCredit403 reports whether a 403 says the upstream provider ran
// out of credit (Provider: UnoRouter, verified 2026-11-20: "This provider
// ran out of credit on their side. That is on us, not you"). The key is
// valid and the failure is upstream capacity, so it is transient like a 5xx.
func isUpstreamCredit403(body []byte) bool {
	return bytes.Contains(body, []byte("ran out of credit"))
}

// isModeration403 reports whether a 403 blocks the request's content
// (OpenRouter: "…requires moderation and your input was flagged") rather than
// the key. Retrying it on the next key or cooling keys down cannot help.
func isModeration403(body []byte) bool {
	b := strings.ToLower(string(body))
	return strings.Contains(b, "moderation") || strings.Contains(b, "flagged")
}

// modelSpecific400 are phrases in a 400 body that blame the model itself, not
// the request. Most 400s blame the request (context too long, tools or images
// the model can't take, a bad parameter); cooling the model down for those
// would let one client's request take the model from everyone for an hour.
// So the list is deliberately narrow: a missed phrase costs one wasted call on
// a later request, a wrong one costs an hour of capacity.
var modelSpecific400 = []string{
	"decommissioned",
	"model_not_found",
	"model not found",
	"does not exist or you do not have access",
	"is not a valid model",
	"unknown model",
	"interactions api",            // Gemini: "This model only supports Interactions API."
	"only supported in v1/responses", // OpenAI: model needs the Responses API
	"not a chat model",
}

func isModelSpecific400(body []byte) bool {
	b := strings.ToLower(string(body))
	for _, p := range modelSpecific400 {
		if strings.Contains(b, p) {
			return true
		}
	}
	return false
}

// isZeroQuota reports whether a 429 body says the quota for this model is zero
// (Gemini returns "limit: 0" for models outside the free tier).
func isZeroQuota(body []byte) bool {
	return bytes.Contains(body, []byte("limit: 0,"))
}

// upstreamErrSnippet returns a short " (...)" suffix describing why an upstream
// call failed, for logging. It never includes request data or API keys.
func upstreamErrSnippet(resp *Response, err error) string {
	if err != nil {
		return fmt.Sprintf(" (%v)", err)
	}
	if resp == nil || len(resp.Body) == 0 {
		return ""
	}
	msg := strings.Join(strings.Fields(string(resp.Body)), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return " (" + msg + ")"
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

// normalizeMaxTokens collapses the two OpenAI max-token spellings into
// "max_tokens". Clients on newer OpenAI SDKs send "max_completion_tokens",
// sometimes alongside "max_tokens"; Gemini's OpenAI-compatible endpoint
// rejects a body carrying both with a 400. When both are set, max_tokens wins.
func normalizeMaxTokens(m map[string]any) {
	v, has := m["max_completion_tokens"]
	if !has {
		return
	}
	if _, hasTokens := m["max_tokens"]; !hasTokens {
		m["max_tokens"] = v
	}
	delete(m, "max_completion_tokens")
}

// hasTools reports whether the request carries tool definitions.
func hasTools(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	tools, ok := raw["tools"]
	if !ok {
		return false
	}
	switch t := tools.(type) {
	case []any:
		return len(t) > 0
	case []map[string]any:
		return len(t) > 0
	}
	return false
}

// toInt converts a JSON (float64) or YAML (int) number to int. Request bodies
// mix both once agent-profile defaults/overrides from the config are applied.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// isEmptyTruncated reports whether a chat completion has no usable output
// because it hit max_tokens: empty content, no tool calls, finish_reason "length".
func isEmptyTruncated(body []byte) bool {
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   *string `json:"content"`
				ToolCalls []any   `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &parsed) != nil || len(parsed.Choices) == 0 {
		return false
	}
	c := parsed.Choices[0]
	empty := c.Message.Content == nil || strings.TrimSpace(*c.Message.Content) == ""
	return c.FinishReason == "length" && empty && len(c.Message.ToolCalls) == 0
}

// sleepCtx waits for d or until ctx ends, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isEmptyCompletion reports whether a 200 body is not a usable chat
// completion: unparseable, without choices, or where no choice carries text
// content, tool calls or a function call. Reasoning-only output does not count.
// Adapted from PR #16 by BenediktChlopik.
func isEmptyCompletion(body []byte) bool {
	var c struct {
		Choices []struct {
			Message struct {
				Content      any   `json:"content"`
				ToolCalls    []any `json:"tool_calls"`
				FunctionCall any   `json:"function_call"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return true
	}
	for _, ch := range c.Choices {
		m := ch.Message
		if len(m.ToolCalls) > 0 || m.FunctionCall != nil {
			return false
		}
		if strings.TrimSpace(ch.Text) != "" || hasContent(m.Content) {
			return false
		}
	}
	return true
}

// hasContent reports whether a message "content" value (a string or an array
// of parts) holds anything beyond whitespace.
func hasContent(v any) bool {
	switch c := v.(type) {
	case string:
		return strings.TrimSpace(c) != ""
	case []any:
		for _, part := range c {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, isText := m["text"].(string); isText {
				if strings.TrimSpace(t) != "" {
					return true
				}
			} else if len(m) > 0 {
				return true // non-text part (image, audio, ...)
			}
		}
	}
	return false
}

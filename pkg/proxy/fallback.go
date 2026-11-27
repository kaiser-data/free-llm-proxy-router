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
	// Signatures caches Gemini thought signatures across requests; see
	// gemini_signatures.go. Shared from the Server, never per-request.
	Signatures *thoughtSignatureCache
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
	// bodyChecked/emptyBody: callProvider already parsed a buffered 200
	// body (once, for both UsedModel and emptiness).
	bodyChecked, emptyBody bool
}

// isEmpty reports whether a 200 carries no usable completion.
func (r *Response) isEmpty() bool {
	switch {
	case r.emptyStream:
		return true
	case r.stream != nil:
		return false
	case r.bodyChecked:
		return r.emptyBody
	}
	return isEmptyCompletion(r.Body)
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
			// Provider: OpenRouter
			// The free tier accepts at most 3 items in the models[] array
			// (400 "'models' array must have 3 items or fewer").
			// Last verified: 2026-11-20
			if len(orModels) > openRouterModelsArrayLimit {
				orModels = orModels[:openRouterModelsArrayLimit]
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

// cooldown429 puts one key slot of a model on cooldown after a 429, or the
// whole provider when its quirks say the quota spans every model.
func (fc *FallbackChain) cooldown429(providerID, modelID string, slot, numKeys int, resp *Response) {
	waitDur, wholeProvider := quirksFor(providerID).rateLimitCooldown(providerID, resp)
	key := cooldownKey(providerID, modelID, slot, numKeys)
	if wholeProvider {
		key = providerID
	}
	fc.RateLimiter.SetCooldown(key, time.Now().Add(waitDur))
	log.Printf("fallback: %s rate limited — cooldown %s", key, waitDur.Round(time.Second))
}

// callOnce makes one upstream call with a fixed key and classifies it.
// Provider-specific steps come from quirksFor; the failure handling from
// classifyFailure.
func (fc *FallbackChain) callOnce(ctx context.Context, callCfg config.ProviderConfig, providerID, modelID string, body map[string]any, estTokens int) attemptResult {
	q := quirksFor(providerID)
	keepReservation := false
	if q.admit != nil {
		release, skip := q.admit(ctx, fc.quirkEnv(), modelID, estTokens)
		if skip != nil {
			return *skip
		}
		if release != nil {
			defer func() {
				if !keepReservation {
					release()
				}
			}()
		}
	}

	resp, err := fc.callProvider(ctx, callCfg, body)
	calls := 1
	if err == nil && q.retryWait != nil {
		if wait := q.retryWait(resp); wait > 0 {
			log.Printf("fallback: %s/%s not ready, retrying in %s", providerID, modelID, wait)
			if serr := sleepCtx(ctx, wait); serr != nil {
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
		fc.RateLimiter.Provider(providerID).RecordRequest(q.usageModelFor(modelID, resp))
		if q.onSuccess != nil {
			q.onSuccess(fc.quirkEnv(), resp)
		}
		if resp.isEmpty() {
			// Reasoning models (gpt-oss) can spend a small max_tokens budget on
			// hidden reasoning, and some free endpoints return 200 with nothing
			// in it. Neither is an answer: try another model.
			log.Printf("fallback: %s/%s returned an empty completion — trying next model", providerID, modelID)
			return attemptResult{resp: resp, outcome: outEmpty, calls: calls}
		}
		fc.ReliabilityTracker.Record(providerID, true)
		return attemptResult{resp: resp, outcome: outSuccess, calls: calls}
	}

	v := classifyFailure(resp)
	if v.modelCooldown > 0 {
		fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(v.modelCooldown))
	}
	if v.countFailure {
		fc.ReliabilityTracker.Record(providerID, false)
		if v.coolIfUnreliable && fc.ReliabilityTracker.ShouldCooldown(providerID) {
			fc.RateLimiter.SetCooldown(providerID, time.Now().Add(unreliableProviderCooldown))
			log.Printf("fallback: %s reliability below 50%% — short cooldown", providerID)
		}
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		// A 429 is logged by cooldown429 once its cooldown is known.
		log.Printf("fallback: %s/%s %s%s", providerID, modelID, v.reason, upstreamErrSnippet(resp, err))
	}
	return attemptResult{resp: resp, outcome: v.outcome, calls: calls}
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if int64(len(respBody)) > maxResponseBytes {
		// A chat answer this large is not one the proxy should hold in
		// memory on a 512 MB board; the chain moves on to the next provider.
		return nil, fmt.Errorf("response exceeds %d MB", maxResponseBytes>>20)
	}

	if cfg.ID == "gemini" && resp.StatusCode == http.StatusOK {
		respBody = StripGeminiThoughts(respBody)
		// Remember tool-call thought signatures so later turns of the same
		// session can replay them even if the client strips extra_content.
		if n := captureGeminiThoughtSignatures(respBody, fc.Signatures); n > 0 {
			log.Printf("gemini: captured %d thought signature(s) from response", n)
		}
	}

	out := &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       respBody,
	}
	if resp.StatusCode == http.StatusOK {
		// One parse for the model that answered (OpenRouter fills it in) and
		// for whether there is any output.
		out.UsedModel, out.emptyBody = parseCompletion(respBody)
		out.bodyChecked = true
	}
	return out, nil
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

	// Gemini 3.x rejects replayed tool-call history that lacks thought
	// signatures (opencode and other OpenAI clients strip them). Re-inject
	// what earlier Gemini responses carried; see gemini_signatures.go.
	if providerID == "gemini" {
		fromCache, sentinel := injectGeminiThoughtSignatures(body, fc.Signatures)
		if fromCache+sentinel > 0 {
			log.Printf("gemini thought signatures: %d from cache, %d sentinel", fromCache, sentinel)
		}
	}
	return body
}

// maxResponseBytes caps a buffered upstream body (an answer or an error),
// like maxRequestBodyBytes caps the client's. A var so tests can lower it.
var maxResponseBytes int64 = 16 << 20

const (
	// maxModelsPerProvider bounds how many models of one provider a single
	// request tries after model-level errors (anything but auth/transport), so a large catalog
	// such as NVIDIA NIM cannot use up every attempt.
	maxModelsPerProvider = 3
	// openRouterModelsArrayLimit caps the native-fallback models[] array:
	// the OpenRouter free tier rejects more with 400 ("'models' array must
	// have 3 items or fewer"). Verified 2026-11-20.
	openRouterModelsArrayLimit = 3
	// transportModelCooldown cools a model whose request failed at the
	// transport level (timeout, conn refused) so one hung model costs its
	// timeout once per window instead of stalling every walk.
	transportModelCooldown = 5 * time.Minute
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
	_, empty := parseCompletion(body)
	return empty
}

// parseCompletion reads, in one pass over a buffered 200 body, the model
// that answered and whether the body is empty (see isEmptyCompletion).
func parseCompletion(body []byte) (model string, empty bool) {
	var c struct {
		Model   string `json:"model"`
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
		return "", true
	}
	for _, ch := range c.Choices {
		m := ch.Message
		if len(m.ToolCalls) > 0 || m.FunctionCall != nil {
			return c.Model, false
		}
		if strings.TrimSpace(ch.Text) != "" || hasContent(m.Content) {
			return c.Model, false
		}
	}
	return c.Model, true
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

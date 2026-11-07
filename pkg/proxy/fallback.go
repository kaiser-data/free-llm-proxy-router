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
	RateLimiter     *ratelimit.GlobalTracker
	GeminiTracker   *ratelimit.GeminiTracker
	ReliabilityTracker *reliability.Tracker
	HTTPClient      *http.Client
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
	defer cancel()

	freeEntries := fc.Catalog.FreeEntries()
	stratReq := strategy.Request{
		Messages:              req.Messages,
		EstimatedPromptTokens: estimateTokens(req.Messages),
		MaxTokens:             req.MaxTokens,
		Stream:                req.Stream,
		Model:                 req.Model,
	}
	est := stratReq.EstimatedPromptTokens

	ranked := fc.Strategy.Rank(stratReq, freeEntries, nil)
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
	if fc.findProvider("openrouter") != nil && !failedProviders["openrouter"] {
		log.Printf("fallback: trying openrouter/free ultimate fallback")
		res := fc.attempt(ctx, "openrouter", "openrouter/free", OpenRouterFreeRouterRequest(fc.buildBody("openrouter", req.Raw, "openrouter/free")), est)
		attempts += res.calls
		if res.err != nil {
			return nil, res.err
		}
		if res.outcome == outSuccess {
			return res.resp, nil
		}
	}

	if lastTruncated != nil {
		lastTruncated.Truncated = true
		return lastTruncated, nil
	}
	return nil, fmt.Errorf("all providers exhausted after %d attempts", attempts)
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
		if fc.RateLimiter.IsOnCooldown(cooldownKey(providerID, modelID, slot, len(keys))) {
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
		log.Printf("fallback: %s/%s rate limited on every key — skipping model", providerID, modelID)
		return attemptResult{outcome: outSkipModel}
	}
	return last
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
		if isEmptyCompletion(resp.Body) {
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
	case resp == nil, status == 401, status == 403:
		// Only transport and auth failures mean the provider itself is
		// unusable, so only they count against its reliability.
		if status != 0 {
			fc.RateLimiter.SetCooldown(providerID, time.Now().Add(10*time.Minute))
			log.Printf("fallback: %s auth error %d — cooldown 10min", providerID, status)
		} else {
			log.Printf("fallback: %s/%s transport error — skipping provider%s", providerID, modelID, upstreamErrSnippet(resp, err))
		}
		fc.ReliabilityTracker.Record(providerID, false)
		if fc.ReliabilityTracker.ShouldCooldown(providerID) {
			fc.RateLimiter.SetCooldown(providerID, time.Now().Add(30*time.Second))
			log.Printf("fallback: %s reliability below 50%% — short cooldown", providerID)
		}
		return attemptResult{resp: resp, outcome: outProviderFail, calls: calls}
	case status == 400, status == 413, status == 422:
		// The provider rejects this request's shape or size; its other models
		// most likely will too, so skip the provider for this request. Not a
		// health problem: no reliability penalty.
		//
		// A 400 can also be model-specific (Gemini: "This model only supports
		// Interactions API"), so the model sits out for a while and later
		// requests reach the provider's next model. 413/422 depend on the
		// request itself, so they set no cooldown.
		if status == http.StatusBadRequest {
			fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(badRequestCooldown))
		}
		log.Printf("fallback: %s/%s status %d — provider rejects this request, skipping provider%s", providerID, modelID, status, upstreamErrSnippet(resp, err))
		return attemptResult{resp: resp, outcome: outProviderFail, calls: calls}
	case status == 404:
		fc.RateLimiter.SetCooldown(providerID+"/"+modelID, time.Now().Add(missingModelCooldown))
		log.Printf("fallback: %s model %s not found — model cooldown %s", providerID, modelID, missingModelCooldown)
	case status != http.StatusTooManyRequests:
		// Free tiers often fail one model (quota, retired, overloaded) while
		// the provider's other models still answer.
		log.Printf("fallback: %s/%s status %d — trying next model%s", providerID, modelID, status, upstreamErrSnippet(resp, err))
	}
	return attemptResult{resp: resp, outcome: outModelFail, calls: calls}
}

func (fc *FallbackChain) callProvider(ctx context.Context, cfg config.ProviderConfig, body map[string]any) (*Response, error) {
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
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

	resp, err := fc.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

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
	// badRequestCooldown applies to a model that answered 400.
	badRequestCooldown = time.Hour
)

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

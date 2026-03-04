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
//  2. Groq with service_tier: auto
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
}

// Execute runs the fallback chain for the given request.
// Returns the first successful response or an error if all providers fail.
func (fc *FallbackChain) Execute(ctx context.Context, req Request) (*Response, error) {
	freeEntries := fc.Catalog.FreeEntries()
	stratReq := strategy.Request{
		Messages:              req.Messages,
		EstimatedPromptTokens: estimateTokens(req.Messages),
		MaxTokens:             req.MaxTokens,
		Stream:                req.Stream,
		Model:                 req.Model,
	}

	ranked := fc.Strategy.Rank(stratReq, freeEntries, nil)
	maxAttempts := fc.Cfg.Fallback.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	// Step 1: Try OpenRouter with models[] array (native fallback)
	if or := fc.findProvider("openrouter"); or != nil {
		openRouterModels := fc.collectProviderModels("openrouter", ranked)
		if len(openRouterModels) > 0 {
			body := BuildOpenRouterRequest(req.Raw, openRouterModels)
			resp, err := fc.callProvider(ctx, *or, body)
			if err == nil && resp.StatusCode == http.StatusOK {
				fc.ReliabilityTracker.Record("openrouter", true)
				return resp, nil
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
	attempt := 0
	failedProviders := make(map[string]bool)
	for _, r := range ranked {
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

		// Skip any provider currently on rate-limit cooldown.
		if fc.RateLimiter.IsOnCooldown(r.ProviderID) {
			log.Printf("fallback: %s on cooldown — skipping", r.ProviderID)
			failedProviders[r.ProviderID] = true
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
				time.Sleep(time.Duration(spacingMs) * time.Millisecond)
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

		body := fc.buildBody(r.ProviderID, req.Raw, r.ModelID)
		resp, err := fc.callProvider(ctx, *providerCfg, body)
		attempt++

		if err != nil || resp.StatusCode != http.StatusOK {
			// HuggingFace 503+loading: wait and retry SAME provider (cold start).
			if r.ProviderID == "huggingface" && resp != nil && resp.StatusCode == 503 {
				if waitMs := parseHFLoadingWait(resp.Body); waitMs > 0 {
					log.Printf("fallback: huggingface model loading, waiting %dms", waitMs)
					time.Sleep(time.Duration(waitMs) * time.Millisecond)
					resp, err = fc.callProvider(ctx, *providerCfg, body)
					if err == nil && resp.StatusCode == http.StatusOK {
						fc.ReliabilityTracker.Record(r.ProviderID, true)
						return resp, nil
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
					fc.RateLimiter.SetCooldown(r.ProviderID, until)
					log.Printf("fallback: %s rate limited — cooldown until %s", r.ProviderID, until.Format("15:04:05"))
				case 401, 403:
					fc.RateLimiter.SetCooldown(r.ProviderID, time.Now().Add(10*time.Minute))
					log.Printf("fallback: %s auth error %d — cooldown 10min", r.ProviderID, resp.StatusCode)
				case 404:
					fc.Catalog.MarkNeedsReverification(r.ProviderID, r.ModelID)
					fc.RateLimiter.SetCooldown(r.ProviderID, time.Now().Add(5*time.Minute))
					log.Printf("fallback: %s model %s not found — cooldown 5min", r.ProviderID, r.ModelID)
				}
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
		return resp, nil
	}

	// Step 5: Ultimate fallback — OpenRouter free router
	if or := fc.findProvider("openrouter"); or != nil {
		log.Printf("fallback: trying openrouter/free ultimate fallback")
		body := OpenRouterFreeRouterRequest(req.Raw)
		resp, err := fc.callProvider(ctx, *or, body)
		if err == nil && resp.StatusCode == http.StatusOK {
			fc.ReliabilityTracker.Record("openrouter", true)
			return resp, nil
		}
		fc.ReliabilityTracker.Record("openrouter", false)
	}

	return nil, fmt.Errorf("all providers exhausted after %d attempts", attempt)
}

func (fc *FallbackChain) callProvider(ctx context.Context, cfg config.ProviderConfig, body map[string]any) (*Response, error) {
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

func (fc *FallbackChain) buildBody(providerID string, raw map[string]any, modelID string) map[string]any {
	body := copyMap(raw)
	body["model"] = modelID
	delete(body, "stream") // always non-streaming to providers; fallback requires buffered JSON

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

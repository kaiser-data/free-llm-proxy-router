// Package ratelimit implements provider-specific rate limit extraction and tracking.
package ratelimit

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitInfo is the unified rate limit state extracted from a provider response.
type RateLimitInfo struct {
	// RetryAfterMs is how many milliseconds to wait before retrying.
	RetryAfterMs int64
	// ResetTimestamp is a Unix timestamp (seconds) when the limit resets (Gemini, Cerebras).
	ResetTimestamp int64
	// RemainingRequests is the count of remaining allowed requests in the current window.
	RemainingRequests int
	// RemainingTokens is the count of remaining tokens in the current window.
	RemainingTokens int
	// CerebrasTokResetMs is the fractional-seconds token-bucket reset time (Cerebras only).
	CerebrasTokResetMs int64
	// GeminiErrorType is "RESOURCE_EXHAUSTED" when Gemini signals a quota error.
	GeminiErrorType string
	// IsRPDExhausted is true when a daily quota is exhausted (requires midnight-PT wait).
	IsRPDExhausted bool
	// GroqResetRequests is the parsed Go-duration form of x-ratelimit-reset-requests.
	GroqResetRequests time.Duration
	// GroqResetTokens is the parsed Go-duration form of x-ratelimit-reset-tokens.
	GroqResetTokens time.Duration
}

// ExtractRateLimitInfo reads all known rate-limit headers from a response and
// returns a unified RateLimitInfo. Provider-specific logic is centralised here.
//
// Provider: OpenRouter
// Docs: https://openrouter.ai/docs/faq
// Last verified: 2026-02-28
// Free tier: 50 req/day (1000 with $10+ balance)
// Auth: Authorization: Bearer (standard)
// Special headers: HTTP-Referer, X-Title required; Retry-After on 429
//
// Provider: Groq
// Docs: https://console.groq.com/docs/rate-limits
// Last verified: 2026-02-28
// Free tier: all models, per-model RPM/TPM/TPD limits
// Auth: Authorization: Bearer (standard)
// Special headers: x-ratelimit-reset-requests / x-ratelimit-reset-tokens (Go duration strings on every response)
//
// Provider: Gemini
// Docs: https://ai.google.dev/gemini-api/docs/rate-limits
// Last verified: 2026-02-28
// Free tier: RPM/TPM/RPD/IPM per model; limits cut Dec 2025
// Auth: x-goog-api-key header (NO Bearer prefix)
// Special headers: X-RateLimit-Reset (unix ts), Retry-After; body has RESOURCE_EXHAUSTED
//
// Provider: Cerebras
// Docs: https://inference-docs.cerebras.ai/support/rate-limits
// Last verified: 2026-02-28
// Free tier: RPS-limited (not RPM); headers on every response
// Auth: Authorization: Bearer (standard)
// Special headers: x-ratelimit-reset-tokens (fractional float seconds), all others int
//
// Provider: Mistral
// Docs: https://docs.mistral.ai/api/
// Last verified: 2026-02-28
// Free tier: 2 RPM, 500K TPM, workspace-level limits
// Auth: Authorization: Bearer (standard)
// Special headers: Retry-After on 429
//
// Provider: Cohere
// Docs: https://docs.cohere.com/reference/rate-limits
// Last verified: 2026-02-28
// Free tier: 1000 req/month (Trial key), monthly reset
// Auth: Authorization: Bearer (standard)
// Special headers: Retry-After; required Cohere-Version header on all requests
//
// Provider: Together AI
// Docs: https://docs.together.ai/docs/rate-limits
// Last verified: 2026-02-28
// Free tier: $25 credit (credit-based, not traditional free tier)
// Auth: Authorization: Bearer (standard)
// Special headers: Retry-After on 429
//
// Provider: DeepSeek
// Docs: https://platform.deepseek.com/docs
// Last verified: 2026-02-28
// Free tier: ~$5 credit (credit-based)
// Auth: Authorization: Bearer (standard)
// Special headers: Retry-After on 429; reasoning_content field in response
//
// Provider: NVIDIA NIM
// Docs: https://docs.api.nvidia.com/nim/reference/limits
// Last verified: 2026-02-28
// Free tier: ~1000 credits on signup; 40 RPM hard limit
// Auth: Authorization: Bearer (standard)
// Special headers: Retry-After on 429
//
// Provider: HuggingFace
// Docs: https://huggingface.co/docs/api-inference/quicktour
// Last verified: 2026-02-28
// Free tier: 300 calls/hour/token; 503+loading = cold start (retry, don't skip)
// Auth: Authorization: Bearer (standard)
// Special headers: none; 503+body "loading" signals cold start
func ExtractRateLimitInfo(providerID string, headers http.Header, body []byte) RateLimitInfo {
	info := RateLimitInfo{
		RemainingRequests: -1,
		RemainingTokens:   -1,
	}

	// Universal: Retry-After (present in most providers on 429)
	if ra := headers.Get("Retry-After"); ra != "" {
		if secs, err := strconv.ParseInt(strings.TrimSpace(ra), 10, 64); err == nil {
			info.RetryAfterMs = secs * 1000
		}
	}

	// Universal: remaining counts when present
	if rem := headers.Get("X-RateLimit-Remaining"); rem != "" {
		if n, err := strconv.Atoi(rem); err == nil {
			info.RemainingRequests = n
		}
	}

	switch strings.ToLower(providerID) {
	case "gemini":
		extractGeminiHeaders(&info, headers, body)
	case "cerebras":
		extractCerebrasHeaders(&info, headers)
	case "groq":
		extractGroqHeaders(&info, headers)
	}

	return info
}

// extractGeminiHeaders processes Gemini-specific 429 indicators.
func extractGeminiHeaders(info *RateLimitInfo, headers http.Header, body []byte) {
	// Gemini error body: {"error": {"status": "RESOURCE_EXHAUSTED", "details": [...]}}
	if strings.Contains(string(body), "RESOURCE_EXHAUSTED") {
		info.GeminiErrorType = "RESOURCE_EXHAUSTED"
	}

	// Parse retryDelay from details array if present
	// "details": [{"@type": "...RetryInfo", "retryDelay": "30s"}]
	var errWrap struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errWrap) == nil {
		for _, d := range errWrap.Error.Details {
			if strings.Contains(d.Type, "RetryInfo") && d.RetryDelay != "" {
				if dur, err := time.ParseDuration(d.RetryDelay); err == nil {
					ms := dur.Milliseconds()
					if ms > info.RetryAfterMs {
						info.RetryAfterMs = ms
					}
				}
			}
		}
	}

	// X-RateLimit-Reset: unix timestamp
	if reset := headers.Get("X-RateLimit-Reset"); reset != "" {
		if ts, err := strconv.ParseInt(strings.TrimSpace(reset), 10, 64); err == nil {
			info.ResetTimestamp = ts
			nowMs := time.Now().UnixMilli()
			resetMs := ts * 1000
			if resetMs > nowMs {
				diff := resetMs - nowMs
				if diff > info.RetryAfterMs {
					info.RetryAfterMs = diff
				}
			}
		}
	}

	// If the reset is more than 60 minutes away, it's an RPD exhaustion.
	// Wait until midnight Pacific Time, not just 60s.
	if info.RetryAfterMs > 60*60*1000 {
		info.IsRPDExhausted = true
	}

	// Remaining request count
	if rem := headers.Get("X-RateLimit-Remaining"); rem != "" {
		if n, err := strconv.Atoi(rem); err == nil {
			info.RemainingRequests = n
		}
	}
}

// extractCerebrasHeaders processes Cerebras-specific headers.
func extractCerebrasHeaders(info *RateLimitInfo, headers http.Header) {
	// x-ratelimit-reset-tokens is a fractional float (seconds)
	if tokenReset := headers.Get("x-ratelimit-reset-tokens"); tokenReset != "" {
		if secs, err := strconv.ParseFloat(strings.TrimSpace(tokenReset), 64); err == nil {
			info.CerebrasTokResetMs = int64(secs * 1000)
		}
	}
	// Use token reset if larger than standard Retry-After
	if info.CerebrasTokResetMs > info.RetryAfterMs {
		info.RetryAfterMs = info.CerebrasTokResetMs
	}

	// Remaining requests (present on every response, not just 429)
	if rem := headers.Get("x-ratelimit-remaining-requests"); rem != "" {
		if n, err := strconv.Atoi(rem); err == nil {
			info.RemainingRequests = n
		}
	}
	if rem := headers.Get("x-ratelimit-remaining-tokens"); rem != "" {
		if n, err := strconv.Atoi(rem); err == nil {
			info.RemainingTokens = n
		}
	}

	// Reset timestamp
	if ts := headers.Get("x-ratelimit-reset-requests"); ts != "" {
		if secs, err := strconv.ParseFloat(strings.TrimSpace(ts), 64); err == nil {
			info.ResetTimestamp = time.Now().Unix() + int64(secs)
		}
	}
}

// extractGroqHeaders processes Groq-specific headers.
// Groq uses Go duration strings like "1m0s" or "6.566s" for reset values.
func extractGroqHeaders(info *RateLimitInfo, headers http.Header) {
	// x-ratelimit-reset-requests (Go duration string, e.g. "1m0s")
	if r := headers.Get("x-ratelimit-reset-requests"); r != "" {
		if dur, err := time.ParseDuration(r); err == nil {
			info.GroqResetRequests = dur
			if ms := dur.Milliseconds(); ms > info.RetryAfterMs {
				info.RetryAfterMs = ms
			}
		}
	}
	// x-ratelimit-reset-tokens
	if r := headers.Get("x-ratelimit-reset-tokens"); r != "" {
		if dur, err := time.ParseDuration(r); err == nil {
			info.GroqResetTokens = dur
			if ms := dur.Milliseconds(); ms > info.RetryAfterMs {
				info.RetryAfterMs = ms
			}
		}
	}

	if rem := headers.Get("x-ratelimit-remaining-requests"); rem != "" {
		if n, err := strconv.Atoi(rem); err == nil {
			info.RemainingRequests = n
		}
	}
	if rem := headers.Get("x-ratelimit-remaining-tokens"); rem != "" {
		if n, err := strconv.Atoi(rem); err == nil {
			info.RemainingTokens = n
		}
	}
}

// WaitDuration returns how long to wait before retrying a request to this provider.
func (r RateLimitInfo) WaitDuration() time.Duration {
	if r.IsRPDExhausted {
		return timeUntilMidnightPT()
	}
	if r.RetryAfterMs > 0 {
		// Add 10% jitter
		jitterMs := r.RetryAfterMs / 10
		if jitterMs > 0 {
			// Use deterministic jitter derived from current nanoseconds
			jitter := time.Duration(time.Now().UnixNano()%jitterMs) * time.Millisecond
			return time.Duration(r.RetryAfterMs)*time.Millisecond + jitter
		}
		return time.Duration(r.RetryAfterMs) * time.Millisecond
	}
	return defaultBackoff()
}

// timeUntilMidnightPT computes the duration until the next midnight in the
// America/Los_Angeles timezone (Pacific Time, which observes PST/PDT).
func timeUntilMidnightPT() time.Duration {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		// Fallback: UTC-8 (PST) if timezone data unavailable
		loc = time.FixedZone("PST", -8*60*60)
	}
	now := time.Now().In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
	return time.Until(midnight)
}

// defaultBackoff returns a conservative 5-second backoff for unknown cases.
func defaultBackoff() time.Duration {
	return 5 * time.Second
}

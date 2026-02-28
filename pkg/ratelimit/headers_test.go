package ratelimit

import (
	"net/http"
	"testing"
	"time"
)

// TestExtractRateLimitInfo_AllProviders tests header extraction for all provider paths.
func TestExtractRateLimitInfo_AllProviders(t *testing.T) {
	t.Run("universal_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "60")
		info := ExtractRateLimitInfo("openrouter", h, nil)
		if info.RetryAfterMs != 60_000 {
			t.Errorf("RetryAfterMs = %d, want 60000", info.RetryAfterMs)
		}
	})

	t.Run("gemini_resource_exhausted", func(t *testing.T) {
		body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","code":429}}`)
		h := http.Header{}
		h.Set("Retry-After", "30")
		info := ExtractRateLimitInfo("gemini", h, body)
		if info.GeminiErrorType != "RESOURCE_EXHAUSTED" {
			t.Errorf("GeminiErrorType = %q, want RESOURCE_EXHAUSTED", info.GeminiErrorType)
		}
	})

	t.Run("gemini_retry_info_in_details", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"code": 429,
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "30s"}
				]
			}
		}`)
		h := http.Header{}
		info := ExtractRateLimitInfo("gemini", h, body)
		if info.RetryAfterMs != 30_000 {
			t.Errorf("RetryAfterMs = %d, want 30000", info.RetryAfterMs)
		}
	})

	t.Run("gemini_reset_timestamp", func(t *testing.T) {
		future := time.Now().Add(2 * time.Minute).Unix()
		h := http.Header{}
		h.Set("X-RateLimit-Reset", itoa(future))
		info := ExtractRateLimitInfo("gemini", h, nil)
		if info.RetryAfterMs <= 0 {
			t.Errorf("RetryAfterMs = %d, want >0 for future reset", info.RetryAfterMs)
		}
	})

	t.Run("gemini_rpd_exhausted", func(t *testing.T) {
		// Reset more than 60 min away = RPD exhaustion
		far := time.Now().Add(2 * time.Hour).Unix()
		h := http.Header{}
		h.Set("X-RateLimit-Reset", itoa(far))
		info := ExtractRateLimitInfo("gemini", h, nil)
		if !info.IsRPDExhausted {
			t.Errorf("IsRPDExhausted = false, want true for reset >60min away")
		}
	})

	t.Run("cerebras_fractional_token_reset", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "0.5")
		info := ExtractRateLimitInfo("cerebras", h, nil)
		if info.CerebrasTokResetMs != 500 {
			t.Errorf("CerebrasTokResetMs = %d, want 500", info.CerebrasTokResetMs)
		}
	})

	t.Run("cerebras_fractional_2300ms", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "2.3")
		info := ExtractRateLimitInfo("cerebras", h, nil)
		if info.CerebrasTokResetMs != 2300 {
			t.Errorf("CerebrasTokResetMs = %d, want 2300", info.CerebrasTokResetMs)
		}
	})

	t.Run("cerebras_remaining_requests", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-remaining-requests", "3")
		h.Set("x-ratelimit-remaining-tokens", "5000")
		info := ExtractRateLimitInfo("cerebras", h, nil)
		if info.RemainingRequests != 3 {
			t.Errorf("RemainingRequests = %d, want 3", info.RemainingRequests)
		}
		if info.RemainingTokens != 5000 {
			t.Errorf("RemainingTokens = %d, want 5000", info.RemainingTokens)
		}
	})

	t.Run("groq_go_duration_reset_requests", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-requests", "1m0s")
		info := ExtractRateLimitInfo("groq", h, nil)
		if info.GroqResetRequests != 60*time.Second {
			t.Errorf("GroqResetRequests = %v, want 1m0s", info.GroqResetRequests)
		}
		if info.RetryAfterMs != 60_000 {
			t.Errorf("RetryAfterMs = %d, want 60000", info.RetryAfterMs)
		}
	})

	t.Run("groq_fractional_reset_tokens", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "6.566s")
		info := ExtractRateLimitInfo("groq", h, nil)
		if info.GroqResetTokens.Milliseconds() != 6566 {
			t.Errorf("GroqResetTokens = %v, want 6.566s", info.GroqResetTokens)
		}
	})

	t.Run("mistral_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "120")
		info := ExtractRateLimitInfo("mistral", h, nil)
		if info.RetryAfterMs != 120_000 {
			t.Errorf("RetryAfterMs = %d, want 120000", info.RetryAfterMs)
		}
	})

	t.Run("cohere_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "5")
		info := ExtractRateLimitInfo("cohere", h, nil)
		if info.RetryAfterMs != 5_000 {
			t.Errorf("RetryAfterMs = %d, want 5000", info.RetryAfterMs)
		}
	})

	t.Run("together_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "10")
		info := ExtractRateLimitInfo("together", h, nil)
		if info.RetryAfterMs != 10_000 {
			t.Errorf("RetryAfterMs = %d, want 10000", info.RetryAfterMs)
		}
	})

	t.Run("deepseek_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "45")
		info := ExtractRateLimitInfo("deepseek", h, nil)
		if info.RetryAfterMs != 45_000 {
			t.Errorf("RetryAfterMs = %d, want 45000", info.RetryAfterMs)
		}
	})

	t.Run("nvidia_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "2")
		info := ExtractRateLimitInfo("nvidia-nim", h, nil)
		if info.RetryAfterMs != 2_000 {
			t.Errorf("RetryAfterMs = %d, want 2000", info.RetryAfterMs)
		}
	})

	t.Run("no_headers_default_backoff", func(t *testing.T) {
		info := ExtractRateLimitInfo("unknown-provider", http.Header{}, nil)
		dur := info.WaitDuration()
		// Default backoff should be positive
		if dur <= 0 {
			t.Errorf("WaitDuration() = %v, want >0", dur)
		}
	})
}

// itoa converts an int64 to a decimal string for test use.
func itoa(n int64) string {
	return itoa64(n)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// Package bench implements the benchmark runner.
package bench

import "time"

// BenchMetrics holds timing and quality metrics for a single inference call.
type BenchMetrics struct {
	ProviderID   string        `json:"provider_id"`
	ModelID      string        `json:"model_id"`
	StrategyName string        `json:"strategy_name"`
	PromptTokens int           `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	// ReasoningTokens is the count of reasoning tokens (DeepSeek R1).
	ReasoningTokens int        `json:"reasoning_tokens,omitempty"`
	TTFT            time.Duration `json:"ttft_ms"` // Time to First Token
	TotalDuration   time.Duration `json:"total_duration_ms"`
	Success         bool          `json:"success"`
	ErrorMsg        string        `json:"error,omitempty"`
	StatusCode      int           `json:"status_code,omitempty"`
}

// TokensPerSecond returns the output token throughput.
func (m *BenchMetrics) TokensPerSecond() float64 {
	if m.TotalDuration == 0 || m.CompletionTokens == 0 {
		return 0
	}
	secs := m.TotalDuration.Seconds()
	return float64(m.CompletionTokens) / secs
}

package bench

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

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/config"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/strategy"
)

// BenchPrompt is a single benchmark prompt.
type BenchPrompt struct {
	Name    string
	Message string
}

// DefaultPrompts are the standard benchmark prompts.
var DefaultPrompts = []BenchPrompt{
	{Name: "simple_qa", Message: "What is the capital of France?"},
	{Name: "math", Message: "What is 23 * 47?"},
	{Name: "code", Message: "Write a Go function that reverses a string."},
	{Name: "reasoning", Message: "If a train travels at 60mph for 2 hours, how far does it go? Show your reasoning."},
}

// Runner runs benchmarks across all strategies and models.
type Runner struct {
	Cfg        *config.Config
	Catalog    *catalog.Catalog
	Strategies []strategy.Strategy
	Prompts    []BenchPrompt
	Client     *http.Client
}

// Run executes the benchmark and returns all metrics.
func (r *Runner) Run(ctx context.Context) ([]BenchMetrics, error) {
	if len(r.Prompts) == 0 {
		r.Prompts = DefaultPrompts
	}

	var results []BenchMetrics
	entries := r.Catalog.FreeEntries()

	for _, strat := range r.Strategies {
		log.Printf("bench: running strategy %s", strat.Name())

		stratReq := strategy.Request{
			Messages:  []map[string]any{{"role": "user", "content": "benchmark"}},
			MaxTokens: 256,
		}
		ranked := strat.Rank(stratReq, entries, nil)
		if len(ranked) == 0 {
			log.Printf("bench: no models for strategy %s", strat.Name())
			continue
		}

		// Test top-3 models per strategy
		tested := 0
		for _, r2 := range ranked {
			if tested >= 3 {
				break
			}
			provCfg := r.findProvider(r2.ProviderID)
			if provCfg == nil {
				continue
			}

			for _, prompt := range r.Prompts {
				m := r.benchOne(ctx, *provCfg, r2.ModelID, strat.Name(), prompt)
				results = append(results, m)
			}
			tested++
		}
	}
	return results, nil
}

func (r *Runner) benchOne(ctx context.Context, cfg config.ProviderConfig, modelID, stratName string, prompt BenchPrompt) BenchMetrics {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"

	payload := map[string]any{
		"model":      modelID,
		"max_tokens": 256,
		"messages":   []map[string]any{{"role": "user", "content": prompt.Message}},
	}
	if cfg.NativeFeatures.ServiceTier != "" {
		payload["service_tier"] = cfg.NativeFeatures.ServiceTier
	}

	data, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return BenchMetrics{
			ProviderID: cfg.ID, ModelID: modelID, StrategyName: stratName,
			ErrorMsg: err.Error(),
		}
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
	resp, err := r.Client.Do(req)
	ttft := time.Since(start)
	if err != nil {
		return BenchMetrics{
			ProviderID: cfg.ID, ModelID: modelID, StrategyName: stratName,
			TTFT: ttft, ErrorMsg: err.Error(),
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	totalDur := time.Since(start)

	m := BenchMetrics{
		ProviderID:    cfg.ID,
		ModelID:       modelID,
		StrategyName:  stratName,
		TTFT:          ttft,
		TotalDuration: totalDur,
		StatusCode:    resp.StatusCode,
		Success:       resp.StatusCode == http.StatusOK,
	}

	if resp.StatusCode == http.StatusOK {
		var parsed struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			// DeepSeek R1 includes reasoning tokens
			Choices []struct {
				Message struct {
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &parsed) == nil {
			m.PromptTokens = parsed.Usage.PromptTokens
			m.CompletionTokens = parsed.Usage.CompletionTokens
			// Count reasoning tokens from DeepSeek R1
			if len(parsed.Choices) > 0 {
				rc := parsed.Choices[0].Message.ReasoningContent
				if rc != "" {
					m.ReasoningTokens = len(rc) / 4 // rough token estimate
				}
			}
		}
	} else {
		m.ErrorMsg = fmt.Sprintf("status %d: %s", resp.StatusCode, body)
	}
	return m
}

func (r *Runner) findProvider(id string) *config.ProviderConfig {
	for i := range r.Cfg.Providers {
		if r.Cfg.Providers[i].ID == id && r.Cfg.Providers[i].Enabled {
			return &r.Cfg.Providers[i]
		}
	}
	return nil
}

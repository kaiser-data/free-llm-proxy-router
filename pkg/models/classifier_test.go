package models

import (
	"testing"
)

// classifierCase holds a model ID and its expected classification.
type classifierCase struct {
	modelID  string
	wantBillions float64
	wantTier ModelTier
	isMoE    bool
}

// TestExtractParams verifies parameter extraction across 30+ model names.
func TestExtractParams(t *testing.T) {
	tests := []classifierCase{
		// Explicit B suffix
		{modelID: "llama-3.1-8b-instruct",       wantBillions: 8,    wantTier: TierSmall},
		{modelID: "llama-3.1-70b-instruct",      wantBillions: 70,   wantTier: TierBalanced},
		{modelID: "llama-3.2-1b-preview",         wantBillions: 1,    wantTier: TierTiny},
		{modelID: "llama-3.2-3b-preview",         wantBillions: 3,    wantTier: TierSmall},
		{modelID: "llama-3.3-70b-versatile",      wantBillions: 70,   wantTier: TierBalanced},
		{modelID: "gemma-2-9b-it",                wantBillions: 9,    wantTier: TierSmall},
		{modelID: "gemma-2-27b-it",               wantBillions: 27,   wantTier: TierBalanced},
		{modelID: "mistral-7b-instruct",          wantBillions: 7,    wantTier: TierSmall},
		{modelID: "deepseek-r1-distill-llama-70b", wantBillions: 70,  wantTier: TierBalanced},
		{modelID: "deepseek-r1-distill-qwen-14b", wantBillions: 14,   wantTier: TierBalanced},
		{modelID: "deepseek-r1-distill-qwen-7b",  wantBillions: 7,    wantTier: TierSmall},
		// "mini" keyword matches "phi-3-mini" → 3B → TierSmall
		{modelID: "phi-3-mini-4k-instruct",       wantBillions: 0,    wantTier: TierSmall}, // "mini" keyword → 3B
		{modelID: "phi-3.5-mini-instruct",        wantBillions: 0,    wantTier: TierSmall},
		{modelID: "SmolLM2-1.7B-Instruct",        wantBillions: 1.7,  wantTier: TierTiny},
		// SmolLM-360M: no B suffix, no numeric keyword → unknown → TierBalanced
		{modelID: "SmolLM-360M",                  wantBillions: 0,    wantTier: TierBalanced},
		{modelID: "gpt-oss-120B",                 wantBillions: 120,  wantTier: TierPerformance},
		{modelID: "qwen2.5-72b-instruct",         wantBillions: 72,   wantTier: TierBalanced},
		{modelID: "qwen2.5-7b-instruct",          wantBillions: 7,    wantTier: TierSmall},
		{modelID: "qwen2.5-3b-instruct",          wantBillions: 3,    wantTier: TierSmall},
		{modelID: "qwen2.5-0.5b-instruct",        wantBillions: 0.5,  wantTier: TierTiny},
		// MoE models
		{modelID: "mixtral-8x7b-32768",           wantTier: TierSmall, isMoE: true},
		{modelID: "mixtral-8x22b-instruct",       wantTier: TierBalanced, isMoE: true},
		// Large models
		{modelID: "llama-3.1-405b-instruct",      wantBillions: 405,  wantTier: TierPerformance},
		{modelID: "command-r-plus-104b",          wantBillions: 104,  wantTier: TierPerformance},
		// Gemini (no B suffix; "flash" has no keyword match — returns unknown → TierBalanced)
		{modelID: "gemini-2.0-flash",             wantBillions: 0,    wantTier: TierBalanced},
		{modelID: "gemini-2.0-flash-lite",        wantBillions: 0,    wantTier: TierBalanced},
		// Keyword fallback
		{modelID: "some-nano-model",              wantBillions: 0.5,  wantTier: TierTiny},
		{modelID: "another-tiny-model",           wantBillions: 1,    wantTier: TierTiny},
		{modelID: "model-small-v2",               wantBillions: 7,    wantTier: TierSmall},
		{modelID: "large-model-xl",               wantBillions: 70,   wantTier: TierBalanced},
		// Completely unknown
		{modelID: "unknown-provider-custom-model", wantBillions: 0,   wantTier: TierBalanced},
	}

	for _, tc := range tests {
		t.Run(tc.modelID, func(t *testing.T) {
			params := ExtractParams(tc.modelID)
			tier := ClassifyTier(params)

			if tc.wantTier != "" && tier != tc.wantTier {
				t.Errorf("ClassifyTier(%q) = %v, want %v (params=%+v)", tc.modelID, tier, tc.wantTier, params)
			}
			if !tc.isMoE && tc.wantBillions > 0 && params.Billions != tc.wantBillions {
				t.Errorf("ExtractParams(%q).Billions = %v, want %v", tc.modelID, params.Billions, tc.wantBillions)
			}
		})
	}
}

// TestClassifyTierBoundaries checks exact tier boundaries.
func TestClassifyTierBoundaries(t *testing.T) {
	tests := []struct {
		effective float64
		want      ModelTier
	}{
		{0, TierBalanced},    // unknown
		{0.5, TierTiny},
		{2.9, TierTiny},
		{3.0, TierSmall},
		{13.9, TierSmall},
		{14.0, TierBalanced},
		{79.9, TierBalanced},
		{80.0, TierPerformance},
		{405.0, TierPerformance},
	}
	for _, tc := range tests {
		params := ParamScale{Effective: tc.effective, Billions: tc.effective}
		got := ClassifyTier(params)
		if got != tc.want {
			t.Errorf("ClassifyTier(%.1fB) = %v, want %v", tc.effective, got, tc.want)
		}
	}
}

// TestMoEExtraction verifies MoE parameter extraction.
func TestMoEExtraction(t *testing.T) {
	tests := []struct {
		modelID      string
		wantBillions float64
		wantN        float64 // number of experts
	}{
		{"mixtral-8x7b-32768", 7, 8},
		{"mixtral-8x22b-instruct-v0.1", 22, 8},
		{"10x22b-model", 22, 10},
	}
	for _, tc := range tests {
		t.Run(tc.modelID, func(t *testing.T) {
			params := ExtractParams(tc.modelID)
			if params.Billions != tc.wantBillions {
				t.Errorf("ExtractParams(%q).Billions = %v, want %v", tc.modelID, params.Billions, tc.wantBillions)
			}
			wantEffective := tc.wantN * tc.wantBillions * 0.23
			// Use approximate comparison for floating point
			diff := params.Effective - wantEffective
			if diff > 0.01 || diff < -0.01 {
				t.Errorf("ExtractParams(%q).Effective = %v, want ~%v", tc.modelID, params.Effective, wantEffective)
			}
			if params.IsEstimated {
				t.Errorf("ExtractParams(%q).IsEstimated = true, want false (MoE)", tc.modelID)
			}
		})
	}
}

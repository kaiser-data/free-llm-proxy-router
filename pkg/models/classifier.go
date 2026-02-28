package models

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// moeRegex matches "NxMb" patterns (e.g. "mixtral-8x7b", "10x22b").
	moeRegex = regexp.MustCompile(`(\d+(?:\.\d+)?)x(\d+(?:\.\d+)?)b`)

	// paramRegex matches "Nb" or "NB" patterns (e.g. "8b", "70B", "1.7b", "120B").
	paramRegex = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*b(?:\b|_)`)

	// keyword table for when no numeric pattern is found
	keywordParams = []struct {
		kw string
		b  float64
	}{
		{"nano", 0.5},
		{"tiny", 1.0},
		{"mini", 3.0},
		{"small", 7.0},
		{"medium", 13.0},
		{"large", 70.0},
		{"xl", 70.0},
		{"xxl", 180.0},
	}
)

// ExtractParams parses the parameter count from a raw model ID string.
// It is agnostic — it works on model names it has never seen before.
// No model names are hardcoded; classification is purely numeric.
func ExtractParams(modelID string) ParamScale {
	lower := strings.ToLower(modelID)

	// Pattern 1: NxMb — Mixture of Experts (e.g. "mixtral-8x7b")
	if m := moeRegex.FindStringSubmatch(lower); len(m) == 3 {
		n, _ := strconv.ParseFloat(m[1], 64)
		mB, _ := strconv.ParseFloat(m[2], 64)
		// Empirical active-param ratio for MoE: ~23% of total experts fire per token
		effective := n * mB * 0.23
		return ParamScale{Billions: mB, Effective: effective, IsEstimated: false}
	}

	// Pattern 2: explicit Nb / NB (e.g. "llama-3.1-8b", "GPT-oss-120B", "SmolLM2-1.7B")
	if m := paramRegex.FindStringSubmatch(lower); len(m) >= 2 {
		b, _ := strconv.ParseFloat(m[1], 64)
		return ParamScale{Billions: b, Effective: b, IsEstimated: false}
	}

	// Pattern 3: keyword fallback — only when no number is found.
	// Use word-boundary matching to avoid false hits (e.g. "gemini" containing "mini").
	// Mark IsEstimated: true.
	for _, kw := range keywordParams {
		if matchesWordBoundary(lower, kw.kw) {
			return ParamScale{Billions: kw.b, Effective: kw.b, IsEstimated: true}
		}
	}

	// Unknown — return zero, caller should treat as Balanced.
	return ParamScale{IsEstimated: true}
}

// matchesWordBoundary checks if kw appears as a whole word (surrounded by non-alpha chars)
// in s. This prevents "mini" from matching "gemini".
func matchesWordBoundary(s, kw string) bool {
	idx := 0
	for {
		pos := strings.Index(s[idx:], kw)
		if pos < 0 {
			return false
		}
		abs := idx + pos
		// Check left boundary
		if abs > 0 && isAlphaChar(s[abs-1]) {
			idx = abs + 1
			continue
		}
		// Check right boundary
		end := abs + len(kw)
		if end < len(s) && isAlphaChar(s[end]) {
			idx = abs + 1
			continue
		}
		return true
	}
}

func isAlphaChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ClassifyTier assigns a ModelTier based solely on numeric effective parameter count.
// No model names are hardcoded here.
func ClassifyTier(params ParamScale) ModelTier {
	effective := params.Effective
	if effective == 0 {
		effective = params.Billions
	}
	switch {
	case effective == 0:
		return TierBalanced // unknown — default to middle
	case effective < 3:
		return TierTiny
	case effective < 14:
		return TierSmall
	case effective < 80:
		return TierBalanced
	default:
		return TierPerformance
	}
}

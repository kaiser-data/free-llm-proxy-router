package strategy

import (
	"sort"
	"strings"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
)

// contextWindowThreshold is the minimum context window (tokens) to be considered
// long-context capable.
const contextWindowThreshold = 32_768

// StrategyLongContext prefers models with large context windows (>32K tokens).
// Gemini 2.0 Flash (1M context) is the top pick — for requests that need the
// space. Small prompts are ranked quality-first instead; see longContextScore.
type StrategyLongContext struct{}

func (s *StrategyLongContext) Name() string { return "long_context" }

func (s *StrategyLongContext) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	// need is the context the request will occupy: prompt plus the reply
	// budget. A model with a smaller window is a guaranteed upstream 400.
	need := req.EstimatedPromptTokens + req.MaxTokens
	// A small prompt (input only, under Groq's ~7k/min free input limit)
	// fits every free model: rank by quality and save the big-context
	// models for the requests only they can hold.
	small := req.EstimatedPromptTokens > 0 && req.EstimatedPromptTokens <= smallPromptThreshold
	ranked := make([]RankedEntry, 0, len(entries))
	for i, e := range entries {
		if !e.IsFree {
			continue
		}
		score := longContextScore(e, enriched, need, small)
		ranked = append(ranked, RankedEntry{
			Entry:      &entries[i],
			ProviderID: e.ProviderID,
			ModelID:    e.ModelID,
			Score:      score,
		})
	}
	// Stable and fully deterministic: context tiers commonly tie (several
	// 1M-context Gemini models at once), and with an unstable sort which
	// model actually served a request was luck of the draw. Equal scores
	// fall back to a stable provider/model order.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score < ranked[j].Score
		}
		if ranked[i].ProviderID != ranked[j].ProviderID {
			return ranked[i].ProviderID < ranked[j].ProviderID
		}
		return ranked[i].ModelID < ranked[j].ModelID
	})
	return ranked
}

func longContextScore(e catalog.CatalogEntry, enriched *models.EnrichedCatalog, need int, small bool) float64 {
	// Use enriched context window if available
	contextWindow := e.ContextWindow
	if contextWindow == 0 && enriched != nil {
		for _, em := range enriched.Models {
			for _, inst := range em.Instances {
				if inst.ProviderID == e.ProviderID && inst.ModelID == e.ModelID {
					contextWindow = inst.ContextWindow
					break
				}
			}
		}
	}

	tier := 10.0 // small context window — last resort
	switch {
	case contextWindow >= 1_000_000:
		tier = 1.0 // 1M+ context: top pick
	case contextWindow >= 128_000:
		tier = 2.0
	case contextWindow >= contextWindowThreshold:
		tier = 3.0
	case contextWindow == 0:
		tier = 5.0 // unknown context window — deprioritise
		for _, c := range models.DetectCapabilities(e.ModelID) {
			if c == models.CapLongContext {
				tier = 2.5
				break
			}
		}
	}

	// A window that cannot hold prompt + reply ranks below every model
	// that fits, whatever its tier: the upstream would reject the request.
	if need > 0 && contextWindow > 0 && contextWindow < need {
		return tier + doesNotFitPenalty
	}

	if small {
		// Small request: quality first (see smallPromptQuality), then the
		// window — smaller windows first, so 1M-context models are saved
		// for the requests only they can hold. The quality term dominates:
		// a tiny model still ranks last.
		return smallPromptQuality(e.ModelID)*10 + windowTerm(contextWindow)
	}

	// Better models first within a tier; quality can never cross tiers
	// because the adjustment is clamped below half the smallest tier gap.
	return tier + qualityAdjustment(e.ModelID)
}

// qualityTieBreakSpan bounds qualityAdjustment below half the smallest gap
// between context tiers (2.0 → 2.5), so quality reorders models within a
// tier but can never outrank a model with a better context tier.
const qualityTieBreakSpan = 0.2

// qualityAdjustment breaks ties between models in the same context tier so
// the fallback chain tries stronger models before weaker ones: the full
// variant of a family ahead of its "lite" sibling, code-capable models
// ahead of generic chat models, and mid-sized models ahead of tiny ones.
// Like the rest of the classifiers it works purely on name patterns — no
// model names are hardcoded.
func qualityAdjustment(modelID string) float64 {
	adj := 0.0
	if strings.Contains(strings.ToLower(modelID), "lite") {
		adj += 0.15 // the "lite" variant is a family's weakest member
	}
	for _, c := range models.DetectCapabilities(modelID) {
		if c == models.CapCode {
			adj -= 0.1 // code-tuned models answer coding prompts better
		}
	}
	switch models.ClassifyTier(models.ExtractParams(modelID)) {
	case models.TierTiny:
		adj += 0.15
	case models.TierSmall:
		adj += 0.05
	case models.TierPerformance:
		adj -= 0.05
	}
	if adj > qualityTieBreakSpan {
		return qualityTieBreakSpan
	}
	if adj < -qualityTieBreakSpan {
		return -qualityTieBreakSpan
	}
	return adj
}

// smallPromptThreshold is the input-token estimate below which a request is
// small: every free model can hold it, so ranking goes quality-first and
// big-context models are saved for the requests only they can serve. It
// sits safely under Groq's ~7k input-token-per-minute free limit, so small
// requests can be routed to Groq's smaller-context models.
const smallPromptThreshold = 4096

// doesNotFitPenalty pushes a model whose window cannot hold prompt + reply
// below every model that fits, including small-context last-resort models.
const doesNotFitPenalty = 50.0

// windowTerm normalises a context window to a small ranking term. Unknown
// windows get a neutral middle value.
func windowTerm(contextWindow int) float64 {
	if contextWindow <= 0 {
		return 0.5
	}
	return float64(contextWindow) / 1_000_000
}

// smallPromptQuality ranks how strongly a model answers a small prompt
// (lower = stronger): code-tuned mid/large/unknown-size models first, then
// mid/large general models, then small and "lite" models, tiny models last.
// Purely name-pattern based, like the rest of the classifiers.
func smallPromptQuality(modelID string) float64 {
	rank := 1.0
	for _, c := range models.DetectCapabilities(modelID) {
		if c == models.CapCode {
			rank = 0 // code-tuned models lead unless tiny or small (tier switch below)
		}
	}
	switch models.ClassifyTier(models.ExtractParams(modelID)) {
	case models.TierTiny:
		rank = 4 // a 2-3B model is the weakest answer even for a tiny prompt
	case models.TierSmall:
		rank = 2 // small models answer, but mid-sized ones answer better
	case models.TierPerformance:
		rank = 0 // the biggest models are first-class too
	}
	if strings.Contains(strings.ToLower(modelID), "lite") {
		rank++ // the lite variant of a family is its weakest member
	}
	return rank
}

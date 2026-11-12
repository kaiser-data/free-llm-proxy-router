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
// Gemini 2.0 Flash (1M context) is the top pick.
type StrategyLongContext struct{}

func (s *StrategyLongContext) Name() string { return "long_context" }

func (s *StrategyLongContext) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	ranked := make([]RankedEntry, 0, len(entries))
	for i, e := range entries {
		if !e.IsFree {
			continue
		}
		score := longContextScore(e, enriched)
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

func longContextScore(e catalog.CatalogEntry, enriched *models.EnrichedCatalog) float64 {
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

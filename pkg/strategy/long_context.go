package strategy

import (
	"sort"

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
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		score := longContextScore(e, enriched)
		ranked = append(ranked, RankedEntry{
			Entry:      e,
			ProviderID: e.ProviderID,
			ModelID:    e.ModelID,
			Score:      score,
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Score < ranked[j].Score
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

	if contextWindow >= 1_000_000 {
		return 1.0 // 1M+ context: top pick
	}
	if contextWindow >= 128_000 {
		return 2.0
	}
	if contextWindow >= contextWindowThreshold {
		return 3.0
	}
	if contextWindow == 0 {
		// Check capability flags
		caps := models.DetectCapabilities(e.ModelID)
		for _, c := range caps {
			if c == models.CapLongContext {
				return 2.5
			}
		}
		return 5.0 // unknown context window — deprioritise
	}
	return 10.0 // small context window — last resort
}

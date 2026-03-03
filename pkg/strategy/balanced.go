package strategy

import (
	"sort"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
)

// StrategyBalanced picks models in the 14-79B effective-parameter range.
// It spreads load evenly across providers rather than always hitting one.
type StrategyBalanced struct{}

func (s *StrategyBalanced) Name() string { return "balanced" }

func (s *StrategyBalanced) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		params := models.ExtractParams(e.ModelID)
		tier := models.ClassifyTier(params)
		score := balancedScore(e, tier)
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

// balancedScore gives the lowest score to Balanced-tier models, then small, then performance.
func balancedScore(e catalog.CatalogEntry, tier models.ModelTier) float64 {
	base := map[models.ModelTier]float64{
		models.TierBalanced:    1.0,
		models.TierSmall:       2.0,
		models.TierPerformance: 3.0,
		models.TierTiny:        4.0,
	}[tier]
	return base
}

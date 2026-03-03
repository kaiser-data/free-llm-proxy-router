package strategy

import (
	"sort"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
)

// StrategyEconomical minimises credit/quota consumption by preferring
// true free-tier providers over credit-based ones, and smaller models within
// each tier.
type StrategyEconomical struct{}

func (s *StrategyEconomical) Name() string { return "economical" }

func (s *StrategyEconomical) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		score := economicalScore(e)
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

func economicalScore(e catalog.CatalogEntry) float64 {
	base := 0.0
	// Credit-based = more expensive in the long run
	if e.TierType == "credit" {
		base += 100.0
	}
	// Within each tier, prefer smaller models (use less quota/tokens)
	params := models.ExtractParams(e.ModelID)
	size := params.Effective
	if size == 0 {
		size = params.Billions
	}
	return base + size
}

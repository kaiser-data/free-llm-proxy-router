package strategy

import (
	"sort"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

// StrategyPerformance prefers the largest available models (highest Effective params).
type StrategyPerformance struct{}

func (s *StrategyPerformance) Name() string { return "performance" }

func (s *StrategyPerformance) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		params := models.ExtractParams(e.ModelID)
		effective := params.Effective
		if effective == 0 {
			effective = params.Billions
		}
		ranked = append(ranked, RankedEntry{
			Entry:      e,
			ProviderID: e.ProviderID,
			ModelID:    e.ModelID,
			// Lower score = higher priority; invert effective params
			Score: -effective,
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Score < ranked[j].Score
	})
	return ranked
}

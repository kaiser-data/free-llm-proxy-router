package strategy

import (
	"sort"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
)

// StrategyReliable sorts models by their historical EMA success rate,
// preferring providers with the most consistent track record.
type StrategyReliable struct {
	Tracker *reliability.Tracker
}

func (s *StrategyReliable) Name() string { return "reliable" }

func (s *StrategyReliable) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		rate := 1.0 // default
		if s.Tracker != nil {
			rate = s.Tracker.SuccessRate(e.ProviderID)
		}
		// Invert: lower score = higher priority
		score := 1.0 - rate
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

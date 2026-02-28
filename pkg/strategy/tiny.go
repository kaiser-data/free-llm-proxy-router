package strategy

import (
	"sort"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

// StrategyTiny selects models under 3B effective parameters.
// Ideal for tasks where latency and quota preservation matter more than quality.
type StrategyTiny struct{}

func (s *StrategyTiny) Name() string { return "tiny" }

func (s *StrategyTiny) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
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

		var score float64
		if effective > 0 && effective < 3 {
			score = effective // within tiny: smaller = better
		} else if effective == 0 {
			score = 2.0 // unknown size — place with tiny
		} else {
			score = effective + 10 // not tiny — deprioritise
		}
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

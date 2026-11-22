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

// successRates reads each provider's success rate once per ranking. The rate
// is per provider, so taking the tracker's lock once per model (thousands
// per request) bought nothing. A nil tracker reports every provider as 1.0.
func successRates(t *reliability.Tracker) func(providerID string) float64 {
	if t == nil {
		return func(string) float64 { return 1.0 }
	}
	rates := map[string]float64{}
	return func(providerID string) float64 {
		r, ok := rates[providerID]
		if !ok {
			r = t.SuccessRate(providerID)
			rates[providerID] = r
		}
		return r
	}
}

func (s *StrategyReliable) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	ranked := make([]RankedEntry, 0, len(entries))
	successRate := successRates(s.Tracker)
	for i, e := range entries {
		if !e.IsFree {
			continue
		}
		rate := successRate(e.ProviderID)
		// Invert: lower score = higher priority
		score := 1.0 - rate
		ranked = append(ranked, RankedEntry{
			Entry:      &entries[i],
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

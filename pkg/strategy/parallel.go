package strategy

import (
	"sort"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
)

// StrategyParallel spreads a request across providers: it ranks the fastest
// model of each provider (see speedScore), fastest providers first, and keeps
// the top FanOut (default 3). The fallback chain tries them in turn, so a
// provider that fails is followed by a different provider, not by another of
// its own models. Calls are sequential: firing them concurrently would spend
// several free quotas on every request.
type StrategyParallel struct {
	FanOut int
}

func (s *StrategyParallel) Name() string { return "parallel" }

func (s *StrategyParallel) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	// One entry per provider: its fastest free model. Ties keep catalog order.
	var ranked []RankedEntry
	best := map[string]int{} // provider → index in ranked
	for i := range entries {
		e := &entries[i]
		if !e.IsFree {
			continue
		}
		score := speedScore(e)
		if j, ok := best[e.ProviderID]; ok {
			if score < ranked[j].Score {
				ranked[j] = RankedEntry{Entry: e, ProviderID: e.ProviderID, ModelID: e.ModelID, Score: score}
			}
			continue
		}
		best[e.ProviderID] = len(ranked)
		ranked = append(ranked, RankedEntry{Entry: e, ProviderID: e.ProviderID, ModelID: e.ModelID, Score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Score < ranked[j].Score
	})
	fanOut := s.FanOut
	if fanOut <= 0 {
		fanOut = 3
	}
	if len(ranked) > fanOut {
		ranked = ranked[:fanOut]
	}
	return ranked
}

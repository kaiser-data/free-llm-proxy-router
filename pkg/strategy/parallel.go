package strategy

import (
	"sort"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

// StrategyParallel fans out requests across multiple providers and returns the
// fastest successful response. The Rank() method returns a list of candidates
// which the proxy layer uses to send concurrent requests.
//
// FanOut controls how many parallel requests to send (default: 3).
type StrategyParallel struct {
	FanOut int
}

func (s *StrategyParallel) Name() string { return "parallel" }

func (s *StrategyParallel) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	// Spread across providers first, then within providers by speed
	var ranked []RankedEntry
	seenProvider := map[string]bool{}
	// First pass: one entry per provider (fastest per provider)
	for _, e := range entries {
		if !e.IsFree || seenProvider[e.ProviderID] {
			continue
		}
		score, ok := providerSpeedScore[e.ProviderID]
		if !ok {
			score = 7.0
		}
		ranked = append(ranked, RankedEntry{
			Entry:      e,
			ProviderID: e.ProviderID,
			ModelID:    e.ModelID,
			Score:      score,
		})
		seenProvider[e.ProviderID] = true
	}
	sort.Slice(ranked, func(i, j int) bool {
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

// providerSpeedScore is also defined in speed.go — use the same values.
// Go does not allow duplicate var declarations in the same package,
// so we access the map from speed.go directly (same package).
// This function is a compile-time guard.
var _ = providerSpeedScore // ensure it's accessible
var _ = models.ExtractParams

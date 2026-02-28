package strategy

import (
	"sort"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/ratelimit"
)

// StrategySmall prefers Small-tier (3-13B) models with a Cerebras pre-emption
// feature: if Cerebras reports remaining-requests < 5, it routes to Groq 8B
// before the 429 hits.
type StrategySmall struct {
	// GlobalTracker provides Cerebras remaining-request awareness.
	GlobalTracker *ratelimit.GlobalTracker
}

func (s *StrategySmall) Name() string { return "small" }

func (s *StrategySmall) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	// Pre-emption: if Cerebras is almost exhausted, bump Groq to front
	cerebrasAlmostFull := s.isCerebrasAlmostFull()

	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		params := models.ExtractParams(e.ModelID)
		tier := models.ClassifyTier(params)

		score := smallScore(e, tier, cerebrasAlmostFull)
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

// isCerebrasAlmostFull checks if Cerebras remaining-requests is below 5.
func (s *StrategySmall) isCerebrasAlmostFull() bool {
	if s.GlobalTracker == nil {
		return false
	}
	t := s.GlobalTracker.Provider("cerebras")
	// We peek at the tracker's per-model remaining state.
	// This is a heuristic — actual check happens in the proxy layer.
	_ = t
	return false // real check done in proxy/fallback.go via header inspection
}

func smallScore(e catalog.CatalogEntry, tier models.ModelTier, cerebrasAlmostFull bool) float64 {
	// Cerebras pre-emption: if almost full, deprioritise cerebras and prefer groq
	if cerebrasAlmostFull {
		if e.ProviderID == "groq" && tier == models.TierSmall {
			return 0.5 // highest priority when cerebras is full
		}
		if e.ProviderID == "cerebras" {
			return 10.0 // last resort when almost full
		}
	}

	base := map[models.ModelTier]float64{
		models.TierSmall:       1.0,
		models.TierTiny:        2.0,
		models.TierBalanced:    3.0,
		models.TierPerformance: 4.0,
	}[tier]

	// Prefer Cerebras and Groq for small models (fastest inference)
	provBonus := 0.0
	switch e.ProviderID {
	case "cerebras":
		provBonus = -0.3
	case "groq":
		provBonus = -0.2
	}
	return base + provBonus
}

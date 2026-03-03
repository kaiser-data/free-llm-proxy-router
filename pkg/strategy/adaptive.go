package strategy

import (
	"sort"
	"strings"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
)

// StrategyAdaptive combines reliability, speed, and model tier signals.
// When OpenRouter is available, it builds a models[] array for native fallback.
//
// OpenRouter native fallback: the proxy layer reads the top-N OpenRouter entries
// and includes them in a models[] array, letting OpenRouter handle intra-provider
// fallback automatically.
type StrategyAdaptive struct {
	Tracker   *reliability.Tracker
	// OpenRouterNativeFallbackN is how many free models to include in models[] array.
	OpenRouterNativeFallbackN int
}

func (s *StrategyAdaptive) Name() string { return "adaptive" }

func (s *StrategyAdaptive) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		score := s.adaptiveScore(e, enriched)
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

// OpenRouterModels returns the top-N OpenRouter free models for native fallback.
// The proxy uses these in a models[] array so OpenRouter handles intra-provider routing.
func (s *StrategyAdaptive) OpenRouterModels(entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []string {
	n := s.OpenRouterNativeFallbackN
	if n <= 0 {
		n = 5
	}
	ranked := s.Rank(Request{}, entries, enriched)
	var out []string
	for _, r := range ranked {
		if strings.ToLower(r.ProviderID) == "openrouter" {
			out = append(out, r.ModelID)
			if len(out) >= n {
				break
			}
		}
	}
	return out
}

func (s *StrategyAdaptive) adaptiveScore(e catalog.CatalogEntry, enriched *models.EnrichedCatalog) float64 {
	// Reliability component (0.0 - 1.0, inverted: 0 = perfect)
	reliability := 0.0
	if s.Tracker != nil {
		reliability = 1.0 - s.Tracker.SuccessRate(e.ProviderID)
	}

	// Speed component (lower = faster provider)
	speedScore, ok := providerSpeedScore[e.ProviderID]
	if !ok {
		speedScore = 7.0
	}

	// Parameter size (prefer balanced models 14-79B by default)
	params := models.ExtractParams(e.ModelID)
	tier := models.ClassifyTier(params)
	tierScore := map[models.ModelTier]float64{
		models.TierBalanced:    1.0,
		models.TierSmall:       1.5,
		models.TierPerformance: 2.0,
		models.TierTiny:        2.5,
	}[tier]

	// Weighted combination
	return reliability*3.0 + speedScore*0.5 + tierScore*1.0
}

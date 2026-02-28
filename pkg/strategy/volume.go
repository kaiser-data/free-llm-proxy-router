package strategy

import (
	"sort"
	"strings"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

// StrategyVolume maximises requests-per-day by preferring providers with the
// highest RPD. It prefers Gemini Flash-Lite (highest RPD: 1000/day as of Dec 2025)
// and rotates across Gemini models to avoid hitting per-model RPD limits.
//
// When a model's RPD is exhausted, this strategy automatically rotates to the
// next-highest-RPD model within the same provider before crossing providers.
type StrategyVolume struct {
	// GeminiPreferredModel is the model to prefer for volume (highest RPD).
	// Default: "gemini-2.0-flash-lite"
	GeminiPreferredModel string
}

func (s *StrategyVolume) Name() string { return "volume" }

func (s *StrategyVolume) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	preferred := s.GeminiPreferredModel
	if preferred == "" {
		preferred = "gemini-2.0-flash-lite"
	}

	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		score := volumeScore(e, preferred)
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

// volumeScore assigns a priority for a volume-maximising strategy.
// Lower score = higher priority.
func volumeScore(e catalog.CatalogEntry, geminiPreferred string) float64 {
	lower := strings.ToLower(e.ModelID)
	provLower := strings.ToLower(e.ProviderID)

	// Tier 1: The preferred Gemini model (highest RPD)
	if provLower == "gemini" && strings.Contains(lower, strings.ToLower(geminiPreferred)) {
		return 1.0
	}
	// Tier 2: Other Gemini Flash models (high RPD)
	if provLower == "gemini" && strings.Contains(lower, "flash") {
		return 2.0
	}
	// Tier 3: Any Gemini model
	if provLower == "gemini" {
		return 3.0
	}
	// Tier 4: Groq (fast, good throughput)
	if provLower == "groq" {
		return 4.0
	}
	// Tier 5: OpenRouter (daily limit 50/day free without balance)
	if provLower == "openrouter" {
		return 5.0
	}
	// Tier 6: Everything else
	params := models.ExtractParams(e.ModelID)
	size := params.Effective
	if size == 0 {
		size = params.Billions
	}
	// Within remaining providers, prefer smaller models (lighter on quotas)
	return 6.0 + size/1000
}

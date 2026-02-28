package strategy

import (
	"sort"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

// providerSpeedScore gives a relative speed score per provider (lower = faster).
// Cerebras is the fastest (~2600 tok/s), then Groq (~750 tok/s), then others.
var providerSpeedScore = map[string]float64{
	"cerebras":   1.0,
	"groq":       2.0,
	"gemini":     3.0,
	"openrouter": 4.0,
	"mistral":    5.0,
	"together":   5.0,
	"deepseek":   6.0,
	"cohere":     6.0,
	"nvidia-nim": 5.0,
	"huggingface": 8.0,
}

// StrategySpeed prefers providers known for the lowest latency,
// then selects the smallest model within that provider (smallest = fastest).
type StrategySpeed struct{}

func (s *StrategySpeed) Name() string { return "speed" }

func (s *StrategySpeed) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		provScore, ok := providerSpeedScore[e.ProviderID]
		if !ok {
			provScore = 7.0 // unknown provider = slower by default
		}
		params := models.ExtractParams(e.ModelID)
		size := params.Effective
		if size == 0 {
			size = params.Billions
		}
		if size == 0 {
			size = 100 // unknown = treat as large for speed ranking
		}
		// Composite: provider speed * 1000 + model size (both ascending = faster)
		score := provScore*1000 + size
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

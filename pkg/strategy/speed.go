package strategy

import (
	"sort"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
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

// speedScore is lower for faster answers: provider speed first (×1000), then
// model size (smaller = faster). Shared by speed and parallel.
func speedScore(e *catalog.CatalogEntry) float64 {
	provScore, ok := providerSpeedScore[e.ProviderID]
	if !ok {
		provScore = 7.0 // unknown provider = slower by default
	}
	params := e.ParamScale()
	size := params.Effective
	if size == 0 {
		size = params.Billions
	}
	if size == 0 {
		size = 100 // unknown = treat as large for speed ranking
	}
	return provScore*1000 + size
}

func (s *StrategySpeed) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	ranked := make([]RankedEntry, 0, len(entries))
	for i, e := range entries {
		if !e.IsFree {
			continue
		}
		score := speedScore(&entries[i])
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

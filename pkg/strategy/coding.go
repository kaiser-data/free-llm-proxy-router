package strategy

import (
	"sort"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

// StrategyCoding prefers models known to have strong code capabilities.
type StrategyCoding struct{}

func (s *StrategyCoding) Name() string { return "coding" }

func (s *StrategyCoding) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		score := codingScore(e, enriched)
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

func codingScore(e catalog.CatalogEntry, enriched *models.EnrichedCatalog) float64 {
	// Check enriched capabilities first
	if enriched != nil {
		for _, em := range enriched.Models {
			for _, inst := range em.Instances {
				if inst.ProviderID == e.ProviderID && inst.ModelID == e.ModelID {
					for _, cap := range em.Capabilities {
						if cap == models.CapCode {
							return 1.0 // explicitly flagged as code-capable
						}
					}
				}
			}
		}
	}

	// Fall back to inference from model name (using DetectCapabilities)
	caps := models.DetectCapabilities(e.ModelID)
	for _, c := range caps {
		if c == models.CapCode {
			return 1.5
		}
	}

	// Use parameter size as a proxy for code quality
	params := models.ExtractParams(e.ModelID)
	effective := params.Effective
	if effective == 0 {
		effective = params.Billions
	}
	return 2.0 + (1000.0 / (effective + 1))
}

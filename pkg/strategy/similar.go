package strategy

import (
	"sort"
	"strings"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

// StrategySimilar prefers models from the same family as a target model.
// The target family is specified in the config (proxy.similar.model_family).
type StrategySimilar struct {
	// TargetFamily is the family ID to prefer (e.g. "llama-3.1-70b").
	TargetFamily string
}

func (s *StrategySimilar) Name() string { return "similar" }

func (s *StrategySimilar) Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry {
	var ranked []RankedEntry
	for _, e := range entries {
		if !e.IsFree {
			continue
		}
		score := similarScore(e, s.TargetFamily, enriched)
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

func similarScore(e catalog.CatalogEntry, targetFamily string, enriched *models.EnrichedCatalog) float64 {
	if targetFamily == "" {
		return 5.0 // no target — neutral rank
	}
	lower := strings.ToLower(targetFamily)

	// Check enriched family
	if enriched != nil {
		for _, em := range enriched.Models {
			if !strings.EqualFold(em.Family, targetFamily) {
				continue
			}
			for _, inst := range em.Instances {
				if inst.ProviderID == e.ProviderID && inst.ModelID == e.ModelID {
					return 1.0 // exact family match
				}
			}
		}
	}

	// Fallback: substring family match in model ID
	if strings.Contains(strings.ToLower(e.ModelID), lower) {
		return 1.5
	}

	// Detected family match
	detected := models.DetectFamily(e.ModelID)
	if strings.EqualFold(detected, targetFamily) {
		return 1.2
	}

	return 5.0 // no match
}

package scan

import (
	"strings"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
)

// CorrelatedModel groups entries from different providers that share the same
// underlying model family/weights.
type CorrelatedModel struct {
	Family   string
	Entries  []catalog.CatalogEntry
}

// Correlate groups catalog entries by their detected family.
// Models in the same group share the same underlying architecture and can be
// used as equivalents in the strategy layer.
func Correlate(entries []catalog.CatalogEntry) []CorrelatedModel {
	groups := map[string][]catalog.CatalogEntry{}
	for _, e := range entries {
		family := models.DetectFamily(e.ModelID)
		if family == "" {
			// Fall back to a loose name match (strip version, provider prefix)
			family = looseFamilyKey(e.ModelID)
		}
		groups[family] = append(groups[family], e)
	}

	var result []CorrelatedModel
	for family, ents := range groups {
		if len(ents) == 0 {
			continue
		}
		result = append(result, CorrelatedModel{
			Family:  family,
			Entries: ents,
		})
	}
	return result
}

// looseFamilyKey extracts a rough family key from a model ID by stripping
// version numbers, provider prefixes, and common suffixes.
func looseFamilyKey(modelID string) string {
	lower := strings.ToLower(modelID)
	// Strip provider prefix (e.g. "meta-llama/", "google/", "mistralai/")
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		lower = lower[idx+1:]
	}
	// Strip common version patterns: -3.1-, -v1, -001, etc.
	for _, pat := range []string{"-instruct", "-chat", ":free", ":nitro", "-latest"} {
		lower = strings.ReplaceAll(lower, pat, "")
	}
	return lower
}

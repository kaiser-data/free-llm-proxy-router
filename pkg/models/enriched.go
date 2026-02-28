package models

import "time"

// ModelInstance represents one provider's offering of a model.
type ModelInstance struct {
	ProviderID    string  `json:"provider_id"`
	ModelID       string  `json:"model_id"`
	ContextWindow int     `json:"context_window,omitempty"`
	IsFree        bool    `json:"is_free"`
	TierType      string  `json:"tier_type,omitempty"` // "free" | "credit"
}

// EnrichedModel is a model with tier, parameter count, capabilities,
// and cross-provider correlation information.
type EnrichedModel struct {
	Family       string          `json:"family"`
	Tier         ModelTier       `json:"tier"`
	Params       ParamScale      `json:"params"`
	Capabilities []Capability    `json:"capabilities,omitempty"`
	Instances    []ModelInstance `json:"instances"`

	// CanonicalID is the normalised model identifier used for cross-provider matching.
	CanonicalID string `json:"canonical_id"`
}

// EnrichedCatalog is the full set of enriched models persisted to enriched.json.
type EnrichedCatalog struct {
	Version   int             `json:"version"`
	UpdatedAt time.Time       `json:"updated_at"`
	Models    []EnrichedModel `json:"models"`
}

// FindByProvider returns all instances for a given provider ID.
func (ec *EnrichedCatalog) FindByProvider(providerID string) []EnrichedModel {
	var out []EnrichedModel
	for _, m := range ec.Models {
		for _, inst := range m.Instances {
			if inst.ProviderID == providerID {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// FindByTier returns all enriched models at the given tier.
func (ec *EnrichedCatalog) FindByTier(tier ModelTier) []EnrichedModel {
	var out []EnrichedModel
	for _, m := range ec.Models {
		if m.Tier == tier {
			out = append(out, m)
		}
	}
	return out
}

// FindByCapability returns all enriched models that have the given capability.
func (ec *EnrichedCatalog) FindByCapability(cap Capability) []EnrichedModel {
	var out []EnrichedModel
	for _, m := range ec.Models {
		for _, c := range m.Capabilities {
			if c == cap {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

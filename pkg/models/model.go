// Package models defines types and classification logic for LLM models.
package models

// ModelTier classifies a model by approximate capability/size.
type ModelTier string

const (
	TierTiny        ModelTier = "tiny"        // <3B effective params
	TierSmall       ModelTier = "small"        // 3-13B
	TierBalanced    ModelTier = "balanced"     // 14-79B (or unknown)
	TierPerformance ModelTier = "performance"  // 80B+
)

// Capability describes an optional model feature.
type Capability string

const (
	CapVision      Capability = "vision"
	CapCode        Capability = "code"
	CapReasoning   Capability = "reasoning"
	CapLongContext Capability = "long_context" // >32K token context
	CapAudio       Capability = "audio"
	CapJSON        Capability = "json_mode"
	CapFunctionCall Capability = "function_calling"
)

// ParamScale holds the extracted parameter count for a model.
type ParamScale struct {
	// Billions is the total declared parameter count.
	Billions float64
	// Effective is the active parameter count (equal to Billions for dense;
	// reduced for MoE via an empirical ratio).
	Effective float64
	// IsEstimated is true when the count was inferred from a keyword, not a number.
	IsEstimated bool
}

// ModelFamily groups models that share the same underlying architecture/weights.
type ModelFamily struct {
	ID          string   `yaml:"id"`
	Names       []string `yaml:"names"`        // substrings that match this family
	Description string   `yaml:"description"`
}

// KnownCapabilityPattern is a pattern used to detect a capability from a model name.
type KnownCapabilityPattern struct {
	Capability Capability `yaml:"capability"`
	Keywords   []string   `yaml:"keywords"`
}

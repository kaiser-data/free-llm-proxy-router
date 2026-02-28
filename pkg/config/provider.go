package config

// ProviderConfig holds all configuration for a single provider.
type ProviderConfig struct {
	ID       string `mapstructure:"id"`
	Name     string `mapstructure:"name"`
	BaseURL  string `mapstructure:"base_url"`
	Enabled  bool   `mapstructure:"enabled"`
	Priority int    `mapstructure:"priority"`
	Timeout  string `mapstructure:"timeout"`

	// Auth: either api_key_env (env var name) or api_key (literal, use sparingly)
	APIKeyEnv  string `mapstructure:"api_key_env"`
	APIKey     string `mapstructure:"api_key"`
	AuthHeader string `mapstructure:"auth_header"` // default: "Authorization"
	AuthPrefix string `mapstructure:"auth_prefix"` // default: "Bearer "

	// ExtraHeaders are injected on every request to this provider.
	// Example: {"HTTP-Referer": "...", "X-Title": "...", "Cohere-Version": "..."}
	ExtraHeaders map[string]string `mapstructure:"extra_headers"`

	// TierType distinguishes traditional rate-limited free tiers ("free")
	// from credit-based systems ("credit").
	TierType string `mapstructure:"tier_type"` // "free" | "credit"

	// RateLimitNotes is a human-readable annotation; not used in logic.
	RateLimitNotes string `mapstructure:"rate_limit_notes"`

	// NativeFeatures describes provider-native capabilities to exploit.
	NativeFeatures NativeFeatures `mapstructure:"native_features"`

	// Discovery configures how free models are enumerated.
	Discovery DiscoveryConfig `mapstructure:"discovery"`

	// PreferredVolumeModel is used by StrategyVolume to pick highest-RPD model.
	PreferredVolumeModel string `mapstructure:"preferred_volume_model"`
}

// NativeFeatures lists provider-native request capabilities.
type NativeFeatures struct {
	// ServiceTier is sent as-is in the request body (Groq: "auto").
	ServiceTier string `mapstructure:"service_tier"`

	// NativeFallbackEnabled: if true, proxy builds models[] array for this provider.
	NativeFallbackEnabled bool `mapstructure:"enabled"`

	// FreeRouterModel is the model ID for ultimate fallback (OpenRouter: "openrouter/free").
	FreeRouterModel string `mapstructure:"free_router_model"`
}

// DiscoveryConfig controls how the scanner finds free models for a provider.
type DiscoveryConfig struct {
	// ModelsEndpoint is the path or full URL to list models.
	ModelsEndpoint string `mapstructure:"models_endpoint"`

	// AllFree marks all returned models as free (e.g. Groq, Cerebras).
	AllFree bool `mapstructure:"all_free"`

	// FreeDetectField is a dot-path field to check for free detection (OpenRouter).
	FreeDetectField string `mapstructure:"free_detect_field"`
	// FreeDetectValue is the expected value (e.g. "0").
	FreeDetectValue string `mapstructure:"free_detect_value"`

	// FreeMarkers are model ID suffixes that signal free models (e.g. ":free").
	FreeMarkers []string `mapstructure:"free_markers"`

	// ProbeForFree: if true, make a test call to verify uncertain free status.
	ProbeForFree bool `mapstructure:"probe_for_free"`

	// SkipModelsEndpoint: use an alternative discovery source (HuggingFace).
	SkipModelsEndpoint bool `mapstructure:"skip_models_endpoint"`

	// HubAPIFilters is passed to the HF Hub API for model filtering.
	HubAPIFilters string `mapstructure:"hub_api_filters"`
}

// ResolvedAuth returns the effective API key and auth header values.
// The env var takes precedence over the literal key.
func (p *ProviderConfig) ResolvedAuth() (header, value string) {
	key := p.APIKey
	if p.APIKeyEnv != "" {
		if v := lookupEnv(p.APIKeyEnv); v != "" {
			key = v
		}
	}
	h := p.AuthHeader
	if h == "" {
		h = "Authorization"
	}
	prefix := p.AuthPrefix
	if prefix == "" && h == "Authorization" {
		prefix = "Bearer "
	}
	return h, prefix + key
}

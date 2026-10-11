package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// envRefPattern matches "${VAR}" references in literal config values so an
// API key can be sourced from the environment without hardcoding it:
//
//	api_key: "${UNOROUTER_API_KEY}"
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvRefs replaces every "${NAME}" in s with the value of NAME,
// checking real environment variables first, then dotenv files (same lookup
// order as api_key_env). Unknown references expand to the empty string: a
// missing key yields "no auth header" rather than a literal "${NAME}"
// being sent upstream.
func expandEnvRefs(s string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	return envRefPattern.ReplaceAllStringFunc(s, func(ref string) string {
		return lookupEnv(ref[2 : len(ref)-1])
	})
}

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

	// RPM caps the per-model request rate the slot limiter (task L0) allows
	// for this provider: at most RPM reservations per sliding minute per
	// model id. 0 (default) disables slot limiting for the provider —
	// requests enter upstreams ungated and rely on upstream 429 handling
	// exactly as before. Set it to the provider's documented per-model
	// per-minute quota (UnoRouter: 1).
	RPM int `mapstructure:"rpm_limit"`
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

	// Refresh opts the provider into the proxy's live :free-model refresh
	// (task Q): the proxy calls GET /v1/models for it at boot and every
	// models.model_refresh_minutes, keeping the canonical groups in sync
	// with the provider's current free list. Off by default — a provider
	// must explicitly opt in (the scanner call is an upstream request and
	// some discovery sources are heavy).
	Refresh bool `mapstructure:"refresh"`
}

// ResolvedAPIKey returns the bare API key (no header name, no prefix).
// Useful for providers that pass the key as a query parameter (e.g. Gemini).
// Lookup order: real env var → dotenv file → literal api_key field (with
// "${VAR}" references expanded).
func (p *ProviderConfig) ResolvedAPIKey() string {
	if p.APIKeyEnv != "" {
		if v := lookupEnv(p.APIKeyEnv); v != "" {
			return v
		}
	}
	return expandEnvRefs(p.APIKey)
}

// ResolvedAuth returns the effective API key and auth header values.
// The env var takes precedence over the literal key (whose "${VAR}"
// references, if any, are expanded).
func (p *ProviderConfig) ResolvedAuth() (header, value string) {
	key := expandEnvRefs(p.APIKey)
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

// AllKeys returns all API keys available for this provider in priority order.
// It auto-discovers numbered variants of the base env var up to _9:
//
//	GROQ_API_KEY, GROQ_API_KEY_2, GROQ_API_KEY_3, ...
//
// Add GROQ_API_KEY_2=gsk_... to .env to pool keys across multiple accounts.
// The proxy rotates to the next slot automatically on 429.
func (p *ProviderConfig) AllKeys() []string {
	var keys []string
	if key := expandEnvRefs(p.APIKey); key != "" {
		keys = append(keys, key)
	}
	if p.APIKeyEnv != "" {
		if v := lookupEnv(p.APIKeyEnv); v != "" {
			keys = append(keys, v)
		}
		for i := 2; i <= 9; i++ {
			if v := lookupEnv(fmt.Sprintf("%s_%d", p.APIKeyEnv, i)); v != "" {
				keys = append(keys, v)
			}
		}
	}
	return keys
}

// NumKeys returns the number of API keys available for this provider.
func (p *ProviderConfig) NumKeys() int {
	return len(p.AllKeys())
}

// ResolvedTimeout parses the per-provider timeout field ("30s", "90s", …)
// into a Duration; 0 means unset or invalid, and the caller falls back to
// the global fallback.provider_timeout_seconds. The field was previously
// declared (and documented in configs/config.example.yaml) but never read —
// wiring it here makes it a working per-upstream override of the buffered
// attempt timeout.
func (p *ProviderConfig) ResolvedTimeout() time.Duration {
	if p.Timeout == "" {
		return 0
	}
	d, err := time.ParseDuration(p.Timeout)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

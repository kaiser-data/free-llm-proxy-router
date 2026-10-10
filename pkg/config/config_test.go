package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// TestModelsExposureDefaults verifies that a config without a models section
// gets the backward-compatible exposure defaults: both raw and canonical
// entries advertised.
func TestModelsExposureDefaults(t *testing.T) {
	path := writeTempConfig(t, "proxy:\n  port: 9999\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Models.ExposeRaw {
		t.Errorf("expose_raw default = false, want true")
	}
	if !cfg.Models.ExposeCanonical {
		t.Errorf("expose_canonical default = false, want true")
	}
}

// TestModelsExposureParsing verifies explicit exposure flags are honoured,
// including disabling raw exposure for canonical-only listing.
func TestModelsExposureParsing(t *testing.T) {
	path := writeTempConfig(t, "models:\n  expose_raw: false\n  expose_canonical: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models.ExposeRaw {
		t.Errorf("expose_raw = true, want false")
	}
	if !cfg.Models.ExposeCanonical {
		t.Errorf("expose_canonical = false, want true")
	}
}

// TestModelsExposureParsingRawOnly verifies raw-only exposure parsing.
func TestModelsExposureParsingRawOnly(t *testing.T) {
	path := writeTempConfig(t, "models:\n  expose_raw: true\n  expose_canonical: false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Models.ExposeRaw {
		t.Errorf("expose_raw = false, want true")
	}
	if cfg.Models.ExposeCanonical {
		t.Errorf("expose_canonical = true, want false")
	}
}

// TestUpstreamsSynonym verifies the provider list may be spelled
// "upstreams" in the config file (the shape used in the UnoRouter docs):
//
//	upstreams:
//	  - id: unorouter
//	    base_url: https://api.unorouter.com/v1
//	    api_key: ${UNOROUTER_API_KEY}
func TestUpstreamsSynonym(t *testing.T) {
	path := writeTempConfig(t, "upstreams:\n  - id: unorouter\n    base_url: https://api.unorouter.com/v1\n    api_key_env: UNOROUTER_API_KEY\n    enabled: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %+v, want exactly the one unorouter upstream", cfg.Providers)
	}
	p := cfg.Providers[0]
	if p.ID != "unorouter" || p.BaseURL != "https://api.unorouter.com/v1" || !p.Enabled {
		t.Errorf("provider = %+v, want id=unorouter base_url=https://api.unorouter.com/v1 enabled=true", p)
	}
}

// TestUpstreamsProvidersWinsWhenBoth: when both spellings appear, the
// canonical "providers" key wins.
func TestUpstreamsProvidersWinsWhenBoth(t *testing.T) {
	path := writeTempConfig(t, "providers:\n  - id: groq\n    base_url: https://api.groq.com/openai/v1\n    enabled: true\nupstreams:\n  - id: unorouter\n    base_url: https://api.unorouter.com/v1\n    enabled: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "groq" {
		t.Errorf("providers = %+v, want the providers-key entry to win", cfg.Providers)
	}
}

// TestAPIKeyEnvRefExpansion verifies api_key: "${VAR}" resolves from the
// environment (real env first, then dotenv) — the key itself is never
// hardcoded in the config.
func TestAPIKeyEnvRefExpansion(t *testing.T) {
	t.Setenv("UNOROUTER_API_KEY", "expanded-test-key")
	path := writeTempConfig(t, "providers:\n  - id: unorouter\n    base_url: https://api.unorouter.com/v1\n    api_key: \"${UNOROUTER_API_KEY}\"\n    enabled: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Providers[0]
	if got := p.ResolvedAPIKey(); got != "expanded-test-key" {
		t.Errorf("ResolvedAPIKey = %q, want expanded-test-key", got)
	}
	header, value := p.ResolvedAuth()
	if header != "Authorization" || value != "Bearer expanded-test-key" {
		t.Errorf("ResolvedAuth = (%q, %q), want (Authorization, Bearer expanded-test-key)", header, value)
	}
	// Keys pooled via numbered env variants still resolve.
	if keys := p.AllKeys(); len(keys) != 1 || keys[0] != "expanded-test-key" {
		t.Errorf("AllKeys = %v, want [expanded-test-key]", keys)
	}
	// An unknown reference expands to empty — no literal "${VAR}" leaks.
	p.APIKey = "${DEFINITELY_NOT_SET_ANYWHERE_XYZ}"
	if got := p.ResolvedAPIKey(); got != "" {
		t.Errorf("unknown env ref = %q, want empty", got)
	}
}
// TestPublicAliasParsing verifies models.public_alias is loaded and defaults
// to empty (feature off) when unset.
func TestPublicAliasParsing(t *testing.T) {
	path := writeTempConfig(t, "models:\n  public_alias: \"kiwi-auto\"\n  expose_raw: false\n  expose_canonical: false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models.PublicAlias != "kiwi-auto" {
		t.Errorf("public_alias = %q, want kiwi-auto", cfg.Models.PublicAlias)
	}
	if cfg.Models.ExposeRaw || cfg.Models.ExposeCanonical {
		t.Errorf("exposure flags = %v/%v, want false/false", cfg.Models.ExposeRaw, cfg.Models.ExposeCanonical)
	}

	// Default: unset.
	defPath := writeTempConfig(t, "proxy:\n  port: 9999\n")
	defCfg, err := Load(defPath)
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if defCfg.Models.PublicAlias != "" {
		t.Errorf("public_alias default = %q, want empty", defCfg.Models.PublicAlias)
	}
}

// TestTimeoutDefaults verifies the backward-compatible timeout defaults: the
// previously hardcoded 120s values become config defaults, and the stream
// timeout inherits the provider timeout when unset (0).
func TestTimeoutDefaults(t *testing.T) {
	path := writeTempConfig(t, "proxy:\n  port: 9999\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Fallback.ProviderTimeoutSeconds != 120 {
		t.Errorf("provider_timeout_seconds default = %d, want 120", cfg.Fallback.ProviderTimeoutSeconds)
	}
	if cfg.Fallback.StreamTimeoutSeconds != 0 {
		t.Errorf("stream_timeout_seconds default = %d, want 0 (inherit)", cfg.Fallback.StreamTimeoutSeconds)
	}
	if cfg.Proxy.WriteTimeoutSeconds != 120 {
		t.Errorf("write_timeout_seconds default = %d, want 120", cfg.Proxy.WriteTimeoutSeconds)
	}
}

// TestTimeoutConfigParsing verifies explicit timeout values are honoured.
func TestTimeoutConfigParsing(t *testing.T) {
	path := writeTempConfig(t, "fallback:\n  provider_timeout_seconds: 45\n  stream_timeout_seconds: 600\nproxy:\n  write_timeout_seconds: 700\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Fallback.ProviderTimeoutSeconds != 45 {
		t.Errorf("provider_timeout_seconds = %d, want 45", cfg.Fallback.ProviderTimeoutSeconds)
	}
	if cfg.Fallback.StreamTimeoutSeconds != 600 {
		t.Errorf("stream_timeout_seconds = %d, want 600", cfg.Fallback.StreamTimeoutSeconds)
	}
	if cfg.Proxy.WriteTimeoutSeconds != 700 {
		t.Errorf("write_timeout_seconds = %d, want 700", cfg.Proxy.WriteTimeoutSeconds)
	}
}

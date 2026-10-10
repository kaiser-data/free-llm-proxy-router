// Package config loads and watches the free-llm-proxy-router configuration file.
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// Config is the top-level configuration struct.
type Config struct {
	Proxy     ProxyConfig      `mapstructure:"proxy"`
	Catalog   CatalogConfig    `mapstructure:"catalog"`
	Refresh   RefreshConfig    `mapstructure:"refresh"`
	Fallback  FallbackConfig   `mapstructure:"fallback"`
	Models    ModelsConfig     `mapstructure:"models"`
	Providers []ProviderConfig `mapstructure:"providers"`
}

// ProxyConfig holds proxy server settings.
type ProxyConfig struct {
	Port      int    `mapstructure:"port"`
	AuthToken string `mapstructure:"auth_token"`
	CacheTTL  int    `mapstructure:"cache_ttl"`
	LogLevel  string `mapstructure:"log_level"`
	Strategy  string `mapstructure:"strategy"`

	// WriteTimeoutSeconds caps the http.Server write timeout — the hard
	// ceiling on one request's total server-side time, including every
	// fallback attempt. Previously hardcoded at 120s. Operators raising
	// the per-attempt upstream timeouts (fallback.provider_timeout_seconds)
	// must raise this too, or the server deadline cuts the response first.
	// Default 120.
	WriteTimeoutSeconds int `mapstructure:"write_timeout_seconds"`

	// StrategyOverrides allows per-strategy config tweaks.
	StrategyOverrides map[string]map[string]any `mapstructure:"strategy_overrides"`

	// Similar holds model_family for the "similar" strategy.
	Similar map[string]string `mapstructure:"similar"`

	// Agents maps profile names to per-agent routing configuration.
	// An agent sets model: "<profile-name>" and the proxy resolves it here.
	Agents map[string]AgentProfile `mapstructure:"agents"`
}

// AgentProfile defines routing behaviour for a named agent.
// Agents identify themselves by sending model: "<profile-name>" in their request.
//
//	strategy   — which routing strategy to use (adaptive, speed, coding, …)
//	model      — pin to a specific model (e.g. "groq/llama-3.3-70b-versatile");
//	             takes precedence over strategy when set
//	defaults   — parameter defaults applied when the client did not set them
//	             (temperature, max_tokens, top_p, stop, …)
//	overrides  — parameters always applied, overriding whatever the client sent
type AgentProfile struct {
	Strategy  string         `mapstructure:"strategy"`
	Model     string         `mapstructure:"model"`
	Defaults  map[string]any `mapstructure:"defaults"`
	Overrides map[string]any `mapstructure:"overrides"`
}

// CatalogConfig holds paths and staleness settings for the local model catalog.
type CatalogConfig struct {
	Path            string        `mapstructure:"path"`
	EnrichedPath    string        `mapstructure:"enriched_path"`
	MaxAgeHours     int           `mapstructure:"max_age_hours"`
	AutoScanOnStart bool          `mapstructure:"auto_scan_on_start"`
	GitSync         GitSyncConfig `mapstructure:"git_sync"`
}

// GitSyncConfig controls catalog synchronisation via git or a raw remote URL.
// See pkg/catalog/gitsync.go for full documentation.
type GitSyncConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	// Scanner role (one machine only):
	RepoPath      string `mapstructure:"repo_path"`
	CatalogInRepo string `mapstructure:"catalog_in_repo"`
	AutoPush      bool   `mapstructure:"auto_push"`
	ScanInterval  string `mapstructure:"scan_interval"`
	// Replica role (all other machines):
	RemoteURL    string `mapstructure:"remote_url"`
	PullInterval string `mapstructure:"pull_interval"`
}

// ModelsConfig configures canonical model aliases.
//
// A canonical name ("qwen3") maps to the raw upstream model IDs that serve
// it across providers. Requests naming a canonical model try the listed
// upstreams in order and use the first free success. Response bodies are
// returned verbatim; the X-Used-Model header reports the canonical name,
// while the raw upstream identity stays in the X-Free-Router-Upstream-*
// headers. Raw model IDs keep working unchanged.
type ModelsConfig struct {
	// Aliases maps canonical names to explicit lists of upstream model IDs.
	// An entry replaces any auto-derived group with the same name.
	Aliases map[string][]string `mapstructure:"aliases"`

	// Canonicalization tunes automatic alias derivation from the catalog.
	Canonicalization CanonicalizationConfig `mapstructure:"canonicalization"`

	// ExposeRaw controls whether raw upstream model IDs are advertised in
	// GET /v1/models. Raw IDs always remain requestable and keep driving
	// routing internally; this flag only filters the client-visible
	// listing. Defaults to true (backward compatible).
	ExposeRaw bool `mapstructure:"expose_raw"`

	// ExposeCanonical controls whether canonical alias names are advertised
	// in GET /v1/models. Requires models.canonicalization.enabled to take
	// effect. Defaults to true.
	ExposeCanonical bool `mapstructure:"expose_canonical"`

	// PublicAlias, when set, turns ONE model name into the entire pool of
	// discovered free chat-capable models (e.g. "kiwi-auto"). The pool is
	// rebuilt from the live catalog on every refresh, so candidates come and
	// go without any client configuration change. In this single-model mode
	// GET /v1/models advertises ONLY this name — expose_raw/expose_canonical
	// apply to non-pool configurations. Raw IDs ("provider/model:free") and
	// canonical family names stay requestable and routable for advanced/raw
	// use; they are just not listed. The real candidate set behind the alias
	// is inspectable at GET /debug/pool.
	PublicAlias string `mapstructure:"public_alias"`
}

// CanonicalizationConfig controls auto-derivation of canonical model groups.
type CanonicalizationConfig struct {
	// Enabled turns on auto-grouping of catalog models into canonical
	// families (e.g. every free qwen* entry joins the "qwen3" group).
	Enabled bool `mapstructure:"enabled"`

	// FreeOnly restricts auto-grouping to entries flagged as free tier.
	FreeOnly bool `mapstructure:"free_only"`
}

// RefreshConfig holds the LLM-powered refresh settings.
type RefreshConfig struct {
	Model      string `mapstructure:"model"`
	Provider   string `mapstructure:"provider"`
	Schedule   string `mapstructure:"schedule"`
	PromptFile string `mapstructure:"prompt_file"`
	WebSearch  bool   `mapstructure:"web_search"`
	OutputMerge string `mapstructure:"output_merge"`
}

// FallbackConfig controls retry and fallback behaviour.
type FallbackConfig struct {
	RetryOn429               bool `mapstructure:"retry_on_429"`
	RetryOn5xx               bool `mapstructure:"retry_on_5xx"`
	MaxAttempts              int  `mapstructure:"max_attempts"`
	CerebrasRequestSpacingMs int  `mapstructure:"cerebras_request_spacing_ms"`

	// ProviderTimeoutSeconds caps each buffered upstream attempt
	// (connection + full response read) on the direct, alias, and strategy
	// routes. Previously hardcoded at 120s. Providers may override it
	// per-upstream via providers[].timeout. Default 120.
	ProviderTimeoutSeconds int `mapstructure:"provider_timeout_seconds"`

	// StreamTimeoutSeconds caps the whole lifetime of a streaming upstream
	// attempt (SSE pass-through). The default 0 inherits
	// provider_timeout_seconds, matching the previous behavior where one
	// 120s client timeout silently bounded both buffered calls and streams.
	// Raise it for long generations; a per-attempt cap stays mandatory
	// because an idle upstream would otherwise pin the handler forever.
	StreamTimeoutSeconds int `mapstructure:"stream_timeout_seconds"`
}

// mu guards the current config pointer for concurrent hot-reload.
var mu sync.RWMutex
var current *Config

// Load reads the configuration from the given file path.
// If path is empty, it searches for config.yaml in the current directory
// and in ~/.free-llm-proxy-router/.
//
// Before returning, Load looks for .env and .secrets files in standard
// locations and populates them into the dotenv map so that ResolvedAuth()
// can find API keys without requiring them in the real environment.
func Load(path string) (*Config, error) {
	v := viper.New()

	setDefaults(v)

	configDir := ""
	if path != "" {
		v.SetConfigFile(path)
		configDir = filepath.Dir(path)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath(expandHome("~/.free-llm-proxy-router"))
		v.AddConfigPath("configs")
	}

	// Load .env / .secrets files before AutomaticEnv so real env vars win.
	loadDotenv(defaultEnvPaths(configDir)...)

	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		// Config file is optional — use defaults if not found.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	// "upstreams" is accepted as a synonym for "providers" — the provider
	// list is the proxy's upstream table and both spellings appear in
	// examples. "providers" wins when both keys are present.
	if len(cfg.Providers) == 0 {
		var upstreams []ProviderConfig
		if err := v.UnmarshalKey("upstreams", &upstreams); err == nil && len(upstreams) > 0 {
			cfg.Providers = upstreams
		}
	}

	mu.Lock()
	current = &cfg
	mu.Unlock()

	return &cfg, nil
}

// Get returns the currently-loaded config (safe for concurrent use).
func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Watch starts watching the config file for changes and calls onChange whenever
// the file changes.  The returned stop function cancels the watcher.
func Watch(path string, onChange func(*Config)) (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	target := path
	if target == "" {
		target = "config.yaml"
	}
	if err := watcher.Add(filepath.Dir(target)); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watching %s: %w", target, err)
	}

	go func() {
		debounce := time.NewTimer(0)
		<-debounce.C // drain initial tick

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name != target && filepath.Base(event.Name) != filepath.Base(target) {
					continue
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					debounce.Reset(200 * time.Millisecond)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("config watcher error: %v", err)
			case <-debounce.C:
				cfg, err := Load(path)
				if err != nil {
					log.Printf("config reload error: %v", err)
					continue
				}
				log.Printf("config reloaded from %s", path)
				onChange(cfg)
			}
		}
	}()

	return func() { watcher.Close() }, nil
}

// setDefaults installs sensible defaults into a viper instance.
func setDefaults(v *viper.Viper) {
	v.SetDefault("proxy.port", 8080)
	v.SetDefault("proxy.cache_ttl", 300)
	v.SetDefault("proxy.log_level", "info")
	v.SetDefault("proxy.strategy", "adaptive")
	v.SetDefault("proxy.write_timeout_seconds", 120)
	v.SetDefault("catalog.path", expandHome("~/.free-llm-proxy-router/catalog.json"))
	v.SetDefault("catalog.enriched_path", expandHome("~/.free-llm-proxy-router/enriched.json"))
	v.SetDefault("catalog.max_age_hours", 24)
	v.SetDefault("fallback.retry_on_429", true)
	v.SetDefault("fallback.retry_on_5xx", true)
	v.SetDefault("fallback.max_attempts", 5)
	v.SetDefault("fallback.provider_timeout_seconds", 120)
	v.SetDefault("fallback.stream_timeout_seconds", 0) // 0 = inherit provider_timeout_seconds
	v.SetDefault("models.canonicalization.enabled", true)
	v.SetDefault("models.canonicalization.free_only", true)
	v.SetDefault("models.expose_raw", true)
	v.SetDefault("models.expose_canonical", true)
	v.SetDefault("models.public_alias", "")
	v.SetDefault("fallback.cerebras_request_spacing_ms", 100)
	v.SetDefault("refresh.schedule", "weekly")
	v.SetDefault("refresh.output_merge", "conservative")
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// lookupEnv returns the value for key, checking real env vars first,
// then falling back to any value loaded from .env / .secrets files.
func lookupEnv(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dotenvLookup(key)
}

// Package config loads and watches the picoclaw-free-llm configuration file.
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
	Proxy   ProxyConfig    `mapstructure:"proxy"`
	Catalog CatalogConfig  `mapstructure:"catalog"`
	Refresh RefreshConfig  `mapstructure:"refresh"`
	Fallback FallbackConfig `mapstructure:"fallback"`
	Providers []ProviderConfig `mapstructure:"providers"`
}

// ProxyConfig holds proxy server settings.
type ProxyConfig struct {
	Port      int    `mapstructure:"port"`
	AuthToken string `mapstructure:"auth_token"`
	CacheTTL  int    `mapstructure:"cache_ttl"`
	LogLevel  string `mapstructure:"log_level"`
	Strategy  string `mapstructure:"strategy"`

	// StrategyOverrides allows per-strategy config tweaks.
	StrategyOverrides map[string]map[string]any `mapstructure:"strategy_overrides"`

	// Similar holds model_family for the "similar" strategy.
	Similar map[string]string `mapstructure:"similar"`
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
	RepoPath      string `mapstructure:"repo_path"`
	CatalogInRepo string `mapstructure:"catalog_in_repo"`
	AutoPush      bool   `mapstructure:"auto_push"`
	PullInterval  string `mapstructure:"pull_interval"`
	RemoteURL     string `mapstructure:"remote_url"`
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
	RetryOn429              bool `mapstructure:"retry_on_429"`
	RetryOn5xx              bool `mapstructure:"retry_on_5xx"`
	MaxAttempts             int  `mapstructure:"max_attempts"`
	CerebrasRequestSpacingMs int  `mapstructure:"cerebras_request_spacing_ms"`
}

// mu guards the current config pointer for concurrent hot-reload.
var mu sync.RWMutex
var current *Config

// Load reads the configuration from the given file path.
// If path is empty, it searches for config.yaml in the current directory
// and in ~/.picoclaw-free-llm/.
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
		v.AddConfigPath(expandHome("~/.picoclaw-free-llm"))
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
	v.SetDefault("catalog.path", expandHome("~/.picoclaw-free-llm/catalog.json"))
	v.SetDefault("catalog.enriched_path", expandHome("~/.picoclaw-free-llm/enriched.json"))
	v.SetDefault("catalog.max_age_hours", 24)
	v.SetDefault("fallback.retry_on_429", true)
	v.SetDefault("fallback.retry_on_5xx", true)
	v.SetDefault("fallback.max_attempts", 5)
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

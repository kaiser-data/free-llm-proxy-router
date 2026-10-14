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

	// WriteTimeoutSeconds is RETIRED (task S): it used to set the
	// http.Server's WriteTimeout — a total ceiling that cut flowing streams
	// and queued requests. The server-level WriteTimeout is now disabled;
	// deadlines are per-request via http.ResponseController:
	// proxy.write_idle_timeout_seconds after each successful write/flush,
	// capped by proxy.max_request_seconds. The field is kept so old configs
	// load unchanged; the boot logs a notice when it is set.
	WriteTimeoutSeconds int `mapstructure:"write_timeout_seconds"`

	// WriteIdleTimeoutSeconds is the post-write idle window (task S):
	// after every successful response write/flush the write deadline moves
	// to now + this value, so a stream that keeps flowing is never cut by
	// a fixed total timeout, while a stalled client (not reading) is cut
	// after the idle window. Default 30.
	WriteIdleTimeoutSeconds int `mapstructure:"write_idle_timeout_seconds"`

	// MaxRequestSeconds is the request's total write ceiling (task S): the
	// initial per-request write deadline. 0 (default) computes it
	// automatically as queue_wait + attempt ceiling + stream ceiling +
	// write_idle. An explicit value below the slot-queue floor
	// (queue_wait + attempt) triggers a boot warning.
	MaxRequestSeconds int `mapstructure:"max_request_seconds"`

	// SlotPersistIntervalSeconds is how often the RPM slot state is saved
	// to ~/.free-llm-proxy-router/slots.json while the proxy runs (T-7
	// hardening): a hard crash (SIGKILL/OOM/power loss) then loses at most
	// one interval of quota protection instead of everything since boot.
	// The graceful-shutdown save stays the final, complete write. Viper
	// default 10; a negative value disables the periodic save.
	SlotPersistIntervalSeconds int `mapstructure:"slot_persist_interval_seconds"`

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
	Enabled bool `mapstructure:"enabled"`
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

	// ModelRefreshMinutes is the refresh interval for the live :free-model
	// discovery (task Q): the proxy calls GET /v1/models for every enabled
	// provider that opted in (providers[].discovery.refresh) at boot and
	// then every this many minutes. A failed refresh keeps the existing
	// list — groups are never cleared by a refresh error. 0 disables the
	// live refresh entirely. Default 20.
	ModelRefreshMinutes int `mapstructure:"model_refresh_minutes"`
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
	Model       string `mapstructure:"model"`
	Provider    string `mapstructure:"provider"`
	Schedule    string `mapstructure:"schedule"`
	PromptFile  string `mapstructure:"prompt_file"`
	WebSearch   bool   `mapstructure:"web_search"`
	OutputMerge string `mapstructure:"output_merge"`
}

// ContextTruncationConfig trims oversized chat histories before they
// leave for fragile free upstreams (round 10): a sliding window keeps the
// system messages and the most recent turns; a still-oversized single
// message is trimmed head+tail with an explicit marker. Tool-calling
// requests are never truncated (dropped context breaks tool-call pairing
// upstream).
type ContextTruncationConfig struct {
	// Enabled turns payload trimming on. Default true.
	Enabled bool `mapstructure:"enabled"`

	// PromptTokenCeiling is the estimated prompt (4 chars ≈ 1 token)
	// above which the history is trimmed. Default 8000.
	PromptTokenCeiling int `mapstructure:"prompt_token_ceiling"`

	// KeepRecentTurns is how many recent non-system messages the sliding
	// window always keeps (plus every system message). Default 6.
	KeepRecentTurns int `mapstructure:"keep_recent_turns"`
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

	// RetriesPerCandidate bounds the same-candidate retry: when a candidate
	// fails with a retryable failure (transport error, 408, 404, and 5xx /
	// 429 gated by the flags above), the SAME provider/model is retried up
	// to this many extra times with exponential backoff before the walk
	// drops the whole provider for the request. Each retry consumes the
	// max_attempts budget. 0 restores the old fail-once-then-skip
	// behavior. Default 1.
	RetriesPerCandidate int `mapstructure:"retries_per_candidate"`

	// RetryAfterSeconds is the Retry-After hint (seconds) sent with the 503
	// exhaustion response when every candidate failed — the value clients
	// are asked to wait before retrying. Default 5.
	RetryAfterSeconds int `mapstructure:"retry_after_seconds"`

	// MaxFallbackHops caps how many distinct candidates a single request
	// may enter (each ranked-walk entry plus the OpenRouter steps count as
	// one hop) — a hard guard against unbounded walking when max_attempts
	// and retries_per_candidate are cranked up. Reaching the ceiling aborts
	// the walk immediately and the request gets the 503 exhaustion
	// response. Default 8: above the default max_attempts=5, so it never
	// binds unless configured aggressively.
	MaxFallbackHops int `mapstructure:"max_fallback_hops"`

	// TotalDeadlineSeconds caps the whole request's wall-clock time
	// (connection + every attempt + every backoff sleep) across the entire
	// candidate walk. Keep it below proxy.write_timeout_seconds (default
	// 120) so a budget overrun ends in a clean 503 exhaustion response
	// instead of the write timeout killing the connection mid-response.
	// 0 disables the cap. Default 90.
	TotalDeadlineSeconds int `mapstructure:"total_deadline_seconds"`

	// ModelBlocklist removes upstream model IDs from ranking entirely —
	// they are never selected for any request (round 8: models like
	// "ling-3.0-flash-sante:free" kept returning HTTP 400s on the free
	// tier). Keys match CatalogEntry.ModelID exactly. Applied per
	// request at the gate, so a config hot-reload takes effect
	// immediately. Empty/nil disables.
	ModelBlocklist []string `mapstructure:"model_blocklist"`

	// Unknown400Policy decides what an UNRECOGNIZED 400 does (round 8
	// step 2) — the recognized 400 subclasses keep their verdicts
	// regardless (content-policy refusal → fail fast, context-length
	// overflow / unsupported modality → fallback). The only recognized
	// value is "cross_model_confirm": the ranked walk tries up to
	// unknown_400_confirm_n DISTINCT upstream models with the same
	// payload before declaring the payload broken and fail-fast
	// surfacing the upstream 400. Any other value — including empty and
	// "fail_fast" — keeps the historical behavior: the first unknown 400
	// surfaces immediately. Unknown values fail closed; pkg/config has
	// no normalize/validate pass.
	Unknown400Policy string `mapstructure:"unknown_400_policy"`

	// Unknown400ConfirmN is the distinct-model confirmation count for
	// unknown_400_policy=cross_model_confirm. Default 2: the walk moves
	// to another model after the first unknown 400 and fail-fasts when a
	// second distinct model fails the same way. <=0 disables the policy
	// (fail_fast).
	Unknown400ConfirmN int `mapstructure:"unknown_400_confirm_n"`

	// Unknown400DemotionSeconds parks a model that answered an
	// unrecognized 400 OUT of every walk for this many seconds (round 8
	// steps 3–4, cross-request demotion). The registry is in-memory,
	// bounded at 64 entries (the oldest demotion is handed back FIFO
	// when full), and entries re-enter ranking automatically when the
	// TTL lapses. When every candidate is demoted the walk falls back to
	// them rather than starving. 0 disables demotion. Default 600.
	Unknown400DemotionSeconds int `mapstructure:"unknown_400_demotion_seconds"`

	// FirstTokenTimeoutSeconds bounds how long a streaming attempt may sit
	// on a 200 response without producing a usable first event (time to
	// first token). The attempt is dropped and the retry ladder runs —
	// nothing has been written to the client yet (pre-commit hold).
	// Default 30.
	FirstTokenTimeoutSeconds int `mapstructure:"first_token_timeout_seconds"`

	// FirstTokenTimeouts overrides FirstTokenTimeoutSeconds per provider
	// (key = provider ID) or provider/model (key = "provider/model") —
	// reasoning models routinely think longer than the global pre-commit
	// hold (task J). Values are seconds; keys are matched exactly, the
	// provider ID as configured and the upstream model ID as routed.
	// Lookup order: "provider/model" → "provider" → FirstTokenTimeoutSeconds
	// (default 30). Zero/negative values are ignored.
	FirstTokenTimeouts map[string]int `mapstructure:"first_token_timeouts"`

	// TTFTSlowThresholdSeconds marks an upstream attempt SLOW when its
	// measured time-to-first-token reached this many seconds (round 10).
	// Slow observations feed the per-model latency EMA and the
	// consecutive-slow circuit breaker; the EMA also deprioritizes slow
	// candidates to the BACK of the ranking (never dropped). Default 10.
	TTFTSlowThresholdSeconds int `mapstructure:"ttft_slow_threshold_seconds"`

	// TTFTBreakerTrips opens the per-model slow-TTFT circuit breaker
	// after this many CONSECUTIVE slow observations (TTFT ≥
	// ttft_slow_threshold_seconds, or a TTFT-watchdog abort): the model
	// joins the cross-request demotion registry (same registry and TTL
	// as unknown_400_demotion_seconds) and every later request skips it
	// at ranking until the TTL lapses. Any fast observation resets the
	// streak. Default 3.
	TTFTBreakerTrips int `mapstructure:"ttft_breaker_trips"`

	// TTFTBreakerMinSamples is how many TTFT samples a candidate needs
	// before the EMA-based deprioritization trusts its measurement.
	// Default 3.
	TTFTBreakerMinSamples int `mapstructure:"ttft_breaker_min_samples"`

	// ContextTruncation trims oversized chat histories before they leave
	// for fragile free upstreams (round 10). See ContextTruncationConfig.
	ContextTruncation ContextTruncationConfig `mapstructure:"context_truncation"`

	// StreamIdleTimeoutSeconds bounds the silent gap between chunks on a
	// committed stream (after the first token). Exceeding it ends the
	// stream with an OpenAI-style SSE error event + [DONE] — never a
	// silently truncated completion. Default 30.
	StreamIdleTimeoutSeconds int `mapstructure:"stream_idle_timeout_seconds"`

	// QueueWaitSeconds bounds how long a request may wait in its model
	// group's FIFO slot queue (task L0) when every eligible upstream lacked
	// an RPM slot. The wait is woken by a single timer at the earliest
	// next_available — no polling. 0 disables queueing: a slotless request
	// gets the exhaustion 503 immediately. Default 90.
	QueueWaitSeconds int `mapstructure:"queue_wait_seconds"`

	// QueueMaxWaiters caps the number of concurrently waiting requests per
	// slot queue; a request arriving on a full queue gets the exhaustion
	// 503 (with Retry-After) immediately instead of waiting. Default 64.
	QueueMaxWaiters int `mapstructure:"queue_max_waiters"`

	// StreamKeepaliveAfterSeconds starts the SSE keepalive on a WAITING
	// streaming request (queue wait or pre-commit hold): once the wait
	// passes this many seconds the proxy sends the 200 SSE header and
	// ": keepalive\n\n" comment events so agents and LBs do not cut the
	// idle connection. 0 disables keepalive. Default 5.
	StreamKeepaliveAfterSeconds int `mapstructure:"stream_keepalive_after_seconds"`

	// StreamKeepaliveIntervalSeconds is the spacing between ": keepalive\n\n"
	// comments once the keepalive has started. Default 15.
	StreamKeepaliveIntervalSeconds int `mapstructure:"stream_keepalive_interval_seconds"`

	// LoopUntilDeadline turns on the loop-until-deadline walk (round 11,
	// task AA): the request's total deadline (fallback.total_deadline_seconds)
	// becomes the PRIMARY stop of the walk — candidates that failed
	// transiently (empty completion, timeout, 5xx, 429, transport) re-enter
	// the loop when their per-request park TTL lapses instead of being cut
	// off for the rest of the request. max_fallback_hops stays as an
	// anti-corruption ceiling (counted across ALL rounds) and a small
	// cooldown (round_cooldown_ms) separates rounds so the loop never spins.
	// Requires a deadline: with total_deadline_seconds=0 (disabled) the flag
	// is ignored and the single-walk behavior applies (fail-closed — an
	// undying loop would burn quota forever). Invariant kept: the client
	// never receives an empty completion — only a REAL answer or the clear
	// 503 exhaustion error.
	LoopUntilDeadline bool `mapstructure:"loop_until_deadline"`

	// RoundCooldownMs is the pause between loop rounds (round 11): the
	// small cooldown that runs when a full walk round failed transiently
	// so the loop waits for upstreams to recover instead of spinning. It
	// is also the per-request park TTL of a transiently failed candidate —
	// the candidate re-enters the ranking when it lapses. 0/negative
	// restores the default 1000 ms.
	RoundCooldownMs int `mapstructure:"round_cooldown_ms"`

	// CommitOnReasoning decides whether a REASONING-ONLY upstream result
	// (reasoning_content / reasoning present, no content, no tool_calls)
	// may be committed to the client (round 11, task W). Default false: a
	// reasoning-only result is NOT a usable event — it counts as empty and
	// the walk falls back; reasoning deltas still signal "still working" so
	// the TTFT/idle watchdogs do not cut a thinking model. true delivers
	// the reasoning-only result verbatim instead of failing the attempt.
	CommitOnReasoning bool `mapstructure:"commit_on_reasoning"`

	// LastResort, when configured, is the final tier of the loop (round
	// 11): once the request's remaining time drops to/below when seconds
	// and there is still no answer, the walk tries provider/model exactly
	// once per round — through the SAME emptiness rules (an empty result
	// fails and the loop continues). Every engagement logs a WARN line and
	// bumps the last_resort_uses counter (visible at GET /debug/latency) so
	// the cost is observable. An unset provider skips the tier entirely.
	LastResort LastResortConfig `mapstructure:"last_resort"`
}

// LastResortConfig is the optional last-resort tier of the
// loop-until-deadline walk (round 11). See FallbackConfig.LastResort.
type LastResortConfig struct {
	// Provider is the provider ID of the last-resort upstream. Empty
	// disables the tier (the default).
	Provider string `mapstructure:"provider"`
	// Model is the upstream model ID to call on the last-resort provider.
	Model string `mapstructure:"model"`
	// WhenSeconds engages the tier when remaining_seconds <= this value.
	// 0/negative disables the tier.
	WhenSeconds int `mapstructure:"when"`
}

// LookupFirstTokenTimeout resolves the per-provider/model override for the
// streaming first-token timeout (fallback.first_token_timeouts, task J):
// the exact "provider/model" key first, then the provider-wide "provider"
// key, else 0 — the caller falls back to the global
// first_token_timeout_seconds. Zero/negative map values are ignored.
func LookupFirstTokenTimeout(m map[string]int, providerID, modelID string) int {
	if len(m) == 0 {
		return 0
	}
	if modelID != "" {
		if v, ok := m[providerID+"/"+modelID]; ok && v > 0 {
			return v
		}
	}
	if v, ok := m[providerID]; ok && v > 0 {
		return v
	}
	return 0
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
	v.SetDefault("proxy.write_timeout_seconds", 120) // retired no-op (task S)
	v.SetDefault("proxy.write_idle_timeout_seconds", 30)
	v.SetDefault("proxy.max_request_seconds", 0) // 0 = automatic ceiling (task S)
	v.SetDefault("proxy.slot_persist_interval_seconds", 10)
	v.SetDefault("catalog.path", expandHome("~/.free-llm-proxy-router/catalog.json"))
	v.SetDefault("catalog.enriched_path", expandHome("~/.free-llm-proxy-router/enriched.json"))
	v.SetDefault("catalog.max_age_hours", 24)
	v.SetDefault("fallback.retry_on_429", true)
	v.SetDefault("fallback.retry_on_5xx", true)
	v.SetDefault("fallback.max_attempts", 5)
	v.SetDefault("fallback.provider_timeout_seconds", 120)
	v.SetDefault("fallback.stream_timeout_seconds", 0) // 0 = inherit provider_timeout_seconds
	v.SetDefault("fallback.retries_per_candidate", 1)
	v.SetDefault("fallback.retry_after_seconds", 5)
	v.SetDefault("fallback.max_fallback_hops", 8)
	v.SetDefault("fallback.total_deadline_seconds", 90)
	v.SetDefault("fallback.first_token_timeout_seconds", 30)
	v.SetDefault("fallback.stream_idle_timeout_seconds", 30)
	v.SetDefault("fallback.queue_wait_seconds", 90)
	v.SetDefault("fallback.queue_max_waiters", 64)
	v.SetDefault("fallback.stream_keepalive_after_seconds", 5)
	v.SetDefault("fallback.stream_keepalive_interval_seconds", 15)
	v.SetDefault("fallback.unknown_400_policy", "cross_model_confirm")
	v.SetDefault("fallback.unknown_400_confirm_n", 2)
	v.SetDefault("fallback.unknown_400_demotion_seconds", 600)
	v.SetDefault("fallback.ttft_slow_threshold_seconds", 10)
	v.SetDefault("fallback.ttft_breaker_trips", 3)
	v.SetDefault("fallback.ttft_breaker_min_samples", 3)
	v.SetDefault("fallback.loop_until_deadline", false)
	v.SetDefault("fallback.round_cooldown_ms", 1000)
	v.SetDefault("fallback.commit_on_reasoning", false)
	v.SetDefault("fallback.last_resort.provider", "")
	v.SetDefault("fallback.last_resort.model", "")
	v.SetDefault("fallback.last_resort.when", 0)
	v.SetDefault("fallback.context_truncation.enabled", true)
	v.SetDefault("fallback.context_truncation.prompt_token_ceiling", 8000)
	v.SetDefault("fallback.context_truncation.keep_recent_turns", 6)
	v.SetDefault("models.model_refresh_minutes", 20)
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

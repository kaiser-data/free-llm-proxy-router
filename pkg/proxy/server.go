package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// Server is the OpenAI-compatible proxy server.
type Server struct {
	conf            atomic.Pointer[configState]
	catalog         atomic.Pointer[catalogSnapshot]
	strategyReg     *strategy.Registry
	rateLimiter     *ratelimit.GlobalTracker
	geminiTracker   *ratelimit.GeminiTracker
	reliabilityTracker *reliability.Tracker
	cache           *ResponseCache
	httpServer      *http.Server
}

// configState pairs a config with the filters built from it. They are stored
// and loaded together, so a request never sees one reload's provider
// settings with another reload's filters.
type configState struct {
	cfg     *config.Config
	filters strategy.Filters
}

// config returns the running configuration.
func (s *Server) config() *config.Config {
	if st := s.conf.Load(); st != nil {
		return st.cfg
	}
	return nil
}

// catalogSnapshot pairs a catalog with its free chat entries. FreeEntries
// filters every entry through IsChatModel and copies the result; doing that
// once per catalog load instead of once per request matters with thousands
// of entries. Both fields are read-only once stored.
type catalogSnapshot struct {
	cat  *catalog.Catalog
	free []catalog.CatalogEntry
}

func newCatalogSnapshot(cat *catalog.Catalog) *catalogSnapshot {
	if cat == nil {
		cat = &catalog.Catalog{}
	}
	free := cat.FreeEntries() // a fresh slice: safe to fill before sharing
	catalog.PrecomputeParams(free)
	if free == nil {
		free = []catalog.CatalogEntry{} // non-nil: FallbackChain.Free nil means "compute"
	}
	return &catalogSnapshot{cat: cat, free: free}
}

// upstreamClient is shared by every upstream call so connections are reused.
// http.DefaultTransport keeps only 2 idle connections per host, so concurrent
// requests to one provider kept re-doing the TLS handshake — slow on the small
// ARM boards this proxy targets.
var upstreamClient = newUpstreamClient()

func newUpstreamClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 16
	// No Client.Timeout: it would also bound reading the body and cut long
	// streams. Each call is bounded by the provider timeout (until the first
	// content chunk) and the request deadline instead.
	return &http.Client{Transport: t}
}

// NewServer creates a new proxy Server.
func NewServer(
	cfg *config.Config,
	cat *catalog.Catalog,
	stratReg *strategy.Registry,
	rateLimiter *ratelimit.GlobalTracker,
	geminiTracker *ratelimit.GeminiTracker,
	reliabilityTracker *reliability.Tracker,
) (*Server, error) {
	s := &Server{
		strategyReg:        stratReg,
		rateLimiter:        rateLimiter,
		geminiTracker:      geminiTracker,
		reliabilityTracker: reliabilityTracker,
	}
	if err := s.UpdateConfig(cfg); err != nil {
		return nil, err
	}
	s.UpdateCatalog(cat)
	s.cache = NewResponseCache(cfg.Proxy.CacheTTL)
	return s, nil
}

// UpdateConfig hot-reloads the configuration. A config whose filters do not
// build is rejected and the current one stays in force.
func (s *Server) UpdateConfig(cfg *config.Config) error {
	filters, err := strategy.BuildFilters(cfg.Proxy.Filters)
	if err != nil {
		return err
	}
	s.conf.Store(&configState{cfg: cfg, filters: filters})

	// Pre-resolve DNS for all enabled providers so the first request
	// doesn't pay a cold DNS lookup. Runs async to avoid blocking startup/reload.
	go preResolveProviders(cfg)
	return nil
}

// preResolveProviders fires off DNS lookups for all enabled provider BaseURLs.
// Best-effort: failures are logged but don't block the proxy.
func preResolveProviders(cfg *config.Config) {
	seen := make(map[string]bool)
	for _, p := range cfg.Providers {
		if !p.Enabled || p.BaseURL == "" {
			continue
		}
		host := extractHost(p.BaseURL)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		go func(h string) {
			if _, err := net.LookupHost(h); err != nil {
				log.Printf("dns pre-resolve: %s: %v", h, err)
			} else {
				log.Printf("dns pre-resolve: %s ok", h)
			}
		}(host)
	}
}

// extractHost returns the host:port from a URL like "https://api.openai.com/v1".
func extractHost(baseURL string) string {
	// Strip scheme
	u := baseURL
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	// Strip path
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

// UpdateCatalog hot-reloads the model catalog.
func (s *Server) UpdateCatalog(cat *catalog.Catalog) {
	s.catalog.Store(newCatalogSnapshot(cat))
}

// Start begins listening on the configured port.
func (s *Server) Start(ctx context.Context) error {
	cfg := s.config()
	mux := http.NewServeMux()

	// OpenAI-compatible routes
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/completions", s.handleCompletions)
	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	var handler http.Handler = mux
	if logRequests(cfg.Proxy.LogLevel) {
		handler = loggingMiddleware(handler)
	}
	handler = recoveryMiddleware(handler)
	handler = authMiddleware(cfg.Proxy.AuthToken, handler)

	s.httpServer = &http.Server{
		Addr:         net.JoinHostPort(cfg.Proxy.Host, strconv.Itoa(cfg.Proxy.Port)),
		Handler:      handler,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout(cfg.Fallback.RequestTimeout),
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("free-llm-proxy listening on %s (strategy: %s)", s.httpServer.Addr, cfg.Proxy.Strategy)
	if cfg.Proxy.AuthToken == "" && !isLoopbackHost(cfg.Proxy.Host) {
		log.Printf("warning: listening on %q without proxy.auth_token — anyone who can reach this port can spend your provider quotas", cfg.Proxy.Host)
	}

	// ListenAndServe returns as soon as Shutdown starts; wait for Shutdown to
	// drain in-flight requests (a streaming agent call, say) before returning,
	// or main exits and cuts them off.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := s.httpServer.Shutdown(shutCtx); err != nil {
			log.Printf("proxy: shutdown: %v", err)
		}
	}()

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-drained
	return nil
}

// shutdownGrace is how long in-flight requests get to finish on SIGTERM.
const shutdownGrace = 30 * time.Second

// writeTimeout bounds writing one response. It must outlast the request
// deadline (a streamed answer is written until the end), so it follows
// fallback.request_timeout; with no request deadline there is none.
//
// The write deadline starts when the request headers are read, the request
// deadline only after the body (up to ReadTimeout), hence both plus a margin.
// Without a request deadline it is still bounded, so a client that stops
// reading cannot hold a handler and its upstream connection forever.
func writeTimeout(requestTimeout time.Duration) time.Duration {
	if requestTimeout <= 0 {
		return noDeadlineWriteTimeout
	}
	return requestTimeout + readTimeout + 15*time.Second
}

const (
	readTimeout            = 30 * time.Second
	noDeadlineWriteTimeout = 15 * time.Minute
)

// logRequests reports whether proxy.log_level asks for one line per request.
// "warn" and "error" drop them, which spares an SD card on small boards.
func logRequests(level string) bool {
	switch strings.ToLower(level) {
	case "warn", "warning", "error":
		return false
	}
	return true
}

// isLoopbackHost reports whether a listen host only accepts local connections.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// maxRequestBodyBytes caps chat request bodies. Generous enough for long
// contexts and a few base64 images, small enough that one client can't make
// the proxy buffer an unbounded body in memory.
const maxRequestBodyBytes = 16 << 20

// handleChatCompletions is the main OpenAI-compat chat endpoint.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
				fmt.Sprintf("request body exceeds %d MB", maxRequestBodyBytes>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "failed to read request body")
		return
	}
	r.Body.Close()

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body is not valid JSON")
		return
	}

	req := Request{Raw: raw}
	if m, ok := raw["model"].(string); ok {
		req.Model = m
	}
	// Normalize the newer OpenAI spelling before agent-profile defaults run,
	// so a profile's max_tokens default cannot override the client's
	// explicit max_completion_tokens budget.
	req.readFields()

	conf := s.conf.Load() // one config + filters for the whole request
	cfg := conf.cfg
	snap := s.catalog.Load()

	// One deadline for the whole request: the direct route and the strategy
	// chain it may fall back to share it.
	if d := cfg.Fallback.RequestTimeout; d > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		r = r.WithContext(ctx)
	}

	// Resolve agent profile: if model field matches a named profile, apply defaults/overrides.
	if profile, ok := cfg.Proxy.Agents[req.Model]; ok {
		// Apply defaults — only when client did not set the key.
		for k, v := range profile.Defaults {
			if _, already := raw[k]; !already {
				raw[k] = v
			}
		}
		// Apply overrides — always win over client values.
		for k, v := range profile.Overrides {
			raw[k] = v
		}
		// Re-read what defaults/overrides may have changed: a profile that
		// sets stream or messages must change routing (SSE or buffered,
		// token estimate, filters), not only the body sent upstream.
		req.readFields()
		// Redirect routing: specific model takes precedence over strategy.
		// A pinned model that has left the free catalog falls back to the
		// profile's strategy rather than failing every request.
		if profile.Model != "" && !catalogHasModel(snap.cat, profile.Model) {
			log.Printf("agent profile %q: pinned model %q not in free catalog — using strategy instead", req.Model, profile.Model)
			profile.Model = ""
		}
		if profile.Model != "" {
			req.Model = profile.Model
			raw["model"] = profile.Model
		} else if profile.Strategy != "" {
			req.Model = profile.Strategy
			raw["model"] = profile.Strategy
		} else {
			// No model/strategy in profile — use proxy default strategy.
			req.Model = ""
			raw["model"] = ""
		}
		log.Printf("agent profile resolved: model=%q strategy=%q", profile.Model, profile.Strategy)
	}

	// Check cache for non-streaming requests that ask for a repeatable answer.
	useCache := !req.Stream && cacheable(raw)
	if useCache {
		if cached := s.cache.Get(raw); cached != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			return
		}
	}

	// Route: model field may be a strategy name, a specific model ID, or "auto"/"".
	if req.Model != "" && req.Model != "auto" {
		if _, errLookup := s.strategyReg.Get(req.Model); errLookup != nil {
			// Not a known strategy — treat as a specific model ID.
			s.serveDirectModel(w, r, conf, snap, req, raw)
			return
		}
		// It is a named strategy — fall through to strategy chain.
	}

	s.executeStrategyChain(w, r, conf, snap, req, raw)
}

// readFields sets Messages, Stream and MaxTokens from r.Raw (normalizing
// max_completion_tokens into max_tokens first). Model is left alone: an agent
// profile resolves it separately.
func (r *Request) readFields() {
	r.Messages = r.Messages[:0]
	if msgs, ok := r.Raw["messages"].([]any); ok {
		for _, msg := range msgs {
			if m, ok := msg.(map[string]any); ok {
				r.Messages = append(r.Messages, m)
			}
		}
	}
	r.Stream, _ = r.Raw["stream"].(bool)
	normalizeMaxTokens(r.Raw)
	r.MaxTokens = 0
	if mt, ok := toInt(r.Raw["max_tokens"]); ok {
		r.MaxTokens = mt
	}
}

// serveDirectModel routes a request with a specific model ID to the right provider.
// Accepts both "model-id" and "provider/model-id" formats.
// If all direct-provider attempts fail, falls back to the full strategy chain.
func (s *Server) serveDirectModel(w http.ResponseWriter, r *http.Request, conf *configState, snap *catalogSnapshot, req Request, raw map[string]any) {
	cfg := conf.cfg
	cat := snap.cat
	// Parse optional "provider/model" prefix.
	// Pass 1 (full exact match) handles IDs like "meta-llama/llama-3.3-70b-instruct:free"
	// where "/" is part of the model ID. Pass 2 handles "groq/llama-3.3-70b-versatile".
	targetProvider := ""
	targetModel := req.Model
	if idx := strings.IndexByte(req.Model, '/'); idx >= 0 {
		targetProvider = req.Model[:idx]
		targetModel = req.Model[idx+1:]
	}

	chain := &FallbackChain{
		Cfg:                cfg,
		Strategy:           nil,
		Catalog:            cat,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         upstreamClient,
		Stream:             req.Stream,
	}

	found := false
	est := estimateTokens(req.Messages)
	for _, e := range cat.Entries {
		if !matchEntry(e, req.Model, targetProvider, targetModel) {
			continue
		}
		found = true
		// Same path as the strategy chain: cooldowns, key slots, quotas,
		// buildBody (max_tokens_cap, service_tier) and accounting.
		res := chain.attempt(r.Context(), e.ProviderID, e.ModelID, chain.buildBody(e.ProviderID, raw, e.ModelID), est)
		if res.err != nil {
			break // request deadline or client gone; the chain below answers 503
		}
		if res.outcome == outSuccess {
			resp := res.resp
			if resp.stream != nil {
				writeUpstreamStream(w, resp)
				return
			}
			if req.Stream {
				writeCompletionAsSSE(w, resp.Body)
				return
			}
			copyUpstreamHeaders(w.Header(), resp.Header)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(resp.Body)
			return
		}
		// Any other outcome — try the next catalog entry.
	}
	if !found {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("model %q not found in free catalog", req.Model))
		return
	}
	// Model found but all direct attempts failed — fall back to strategy chain.
	log.Printf("direct route: all providers failed for %q — falling back to strategy chain", req.Model)
	req.Model = ""
	raw["model"] = ""
	s.executeStrategyChain(w, r, conf, snap, req, raw)
}

// catalogHasModel reports whether a model ID ("model" or "provider/model") is
// in the free catalog.
func catalogHasModel(cat *catalog.Catalog, model string) bool {
	targetProvider, targetModel := "", model
	if idx := strings.IndexByte(model, '/'); idx >= 0 {
		targetProvider, targetModel = model[:idx], model[idx+1:]
	}
	for _, e := range cat.Entries {
		if matchEntry(e, model, targetProvider, targetModel) {
			return true
		}
	}
	return false
}

// matchEntry reports whether a catalog entry matches the requested model.
// Pass 1: full exact string match (handles OpenRouter IDs with embedded slash).
// Pass 2: explicit provider-prefix routing (e.g. "groq/llama-3.3-70b-versatile").
func matchEntry(e catalog.CatalogEntry, reqModel, targetProvider, targetModel string) bool {
	if !e.IsFree {
		return false
	}
	if e.ModelID == reqModel {
		return true
	}
	if targetProvider != "" && e.ProviderID == targetProvider && e.ModelID == targetModel {
		return true
	}
	return false
}

// executeStrategyChain selects the appropriate strategy and runs the provider fallback chain.
func (s *Server) executeStrategyChain(w http.ResponseWriter, r *http.Request, conf *configState, snap *catalogSnapshot, req Request, raw map[string]any) {
	cfg := conf.cfg
	stratName := cfg.Proxy.Strategy
	if req.Model != "" && req.Model != "auto" {
		if _, err := s.strategyReg.Get(req.Model); err == nil {
			stratName = req.Model
		}
	}

	strat, err := s.strategyReg.Get(stratName)
	if err != nil {
		log.Printf("unknown strategy %q, falling back to adaptive", stratName)
		strat, _ = s.strategyReg.Get("adaptive")
	}

	chain := &FallbackChain{
		Cfg:                cfg,
		Strategy:           strat,
		Catalog:            snap.cat,
		Free:               snap.free,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         upstreamClient,
		Filters:            conf.filters,
		Stream:             req.Stream,
	}

	// Streaming requests go upstream as SSE; the chain still falls back on
	// anything that fails before the first content chunk (see peekStream).
	resp, err := chain.Execute(r.Context(), req)
	if errors.Is(err, strategy.ErrNoCandidates) {
		// No model can take this request (too long, needs tools): a retry
		// would fail the same way, so this is the client's 400, not a 503.
		log.Printf("fallback chain: %v", err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "no_model_fits_request", err.Error())
		return
	}
	if err != nil {
		log.Printf("fallback chain exhausted: %v", err)
		// A retryable error, not a fake completion: SDKs retry 503s and honour
		// Retry-After, whereas a 200 would be taken as a real answer.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(s.rateLimiter.EarliestRecovery())))
		w.Header().Set("X-Proxy-Exhausted", "true")
		writeError(w, http.StatusServiceUnavailable, "server_error", "all_providers_exhausted",
			"All free providers are rate-limited or failing. Retry after the Retry-After interval.")
		return
	}

	if resp.stream != nil {
		writeUpstreamStream(w, resp)
		return
	}
	if req.Stream && resp.StatusCode == http.StatusOK {
		if resp.UsedModel != "" {
			w.Header().Set("X-Used-Model", resp.UsedModel)
		}
		writeCompletionAsSSE(w, resp.Body)
		return
	}

	if !req.Stream && resp.StatusCode == http.StatusOK && !resp.Truncated && cacheable(raw) {
		s.cache.Set(raw, resp.Body)
	}

	copyUpstreamHeaders(w.Header(), resp.Header)
	if resp.UsedModel != "" {
		w.Header().Set("X-Used-Model", resp.UsedModel)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Body)
}

// upstreamOnlyHeaders describe the upstream connection or the upstream body as
// it was on the wire. The proxy re-frames the body (and may rewrite it, e.g.
// StripGeminiThoughts), so forwarding them would corrupt the response.
var upstreamOnlyHeaders = map[string]bool{
	"Content-Length":    true,
	"Content-Encoding":  true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Keep-Alive":        true,
	"Set-Cookie":        true,
}

// copyUpstreamHeaders forwards upstream response headers (rate-limit info,
// request IDs) except framing, hop-by-hop and cookie headers.
func copyUpstreamHeaders(dst, src http.Header) {
	for k, vals := range src {
		if upstreamOnlyHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// handleModels lists what a client can put in "model": "auto", the routing
// strategies, the configured agent profiles, then every free catalog model
// (deduplicated by ID; owned_by is the first provider serving it). UIs like
// Open WebUI and Continue only offer IDs that appear here.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	snap := s.catalog.Load()
	cfg := s.config()
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	models := []modelEntry{}
	seen := map[string]bool{}
	add := func(id, owner string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, modelEntry{ID: id, Object: "model", OwnedBy: owner})
	}

	add("auto", "free-llm-proxy-router")
	if s.strategyReg != nil {
		names := s.strategyReg.Names()
		sort.Strings(names)
		for _, n := range names {
			add(n, "free-llm-proxy-router")
		}
	}
	if cfg != nil {
		agents := make([]string, 0, len(cfg.Proxy.Agents))
		for n := range cfg.Proxy.Agents {
			agents = append(agents, n)
		}
		sort.Strings(agents)
		for _, n := range agents {
			add(n, "free-llm-proxy-router")
		}
	}
	if snap != nil {
		for _, e := range snap.free {
			add(e.ModelID, e.ProviderID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	})
}

// handleCompletions is a stub for text-completion requests.
// Most modern models only support chat; this returns a helpful error.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "invalid_request_error", "unsupported_endpoint",
		"legacy completions are not supported; use /v1/chat/completions")
}

// findProviderCfg returns the enabled provider with the given ID, or nil.
// Providers that need an API key but have none are treated as unavailable,
// so requests don't spend a round trip on a guaranteed 401.
func findProviderCfg(cfg *config.Config, providerID string) *config.ProviderConfig {
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == providerID && cfg.Providers[i].Enabled && !cfg.Providers[i].MissingKey() {
			return &cfg.Providers[i]
		}
	}
	return nil
}


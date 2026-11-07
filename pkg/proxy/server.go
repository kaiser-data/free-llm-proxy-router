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
	cfg             atomic.Pointer[config.Config]
	catalog         atomic.Pointer[catalog.Catalog]
	strategyReg     *strategy.Registry
	rateLimiter     *ratelimit.GlobalTracker
	geminiTracker   *ratelimit.GeminiTracker
	reliabilityTracker *reliability.Tracker
	cache           *ResponseCache
	streamProxy     *StreamProxy
	httpServer      *http.Server
}

// NewServer creates a new proxy Server.
func NewServer(
	cfg *config.Config,
	cat *catalog.Catalog,
	stratReg *strategy.Registry,
	rateLimiter *ratelimit.GlobalTracker,
	geminiTracker *ratelimit.GeminiTracker,
	reliabilityTracker *reliability.Tracker,
) *Server {
	s := &Server{
		strategyReg:        stratReg,
		rateLimiter:        rateLimiter,
		geminiTracker:      geminiTracker,
		reliabilityTracker: reliabilityTracker,
	}
	s.cfg.Store(cfg)
	s.catalog.Store(cat)
	s.cache = NewResponseCache(cfg.Proxy.CacheTTL)
	s.streamProxy = &StreamProxy{HTTPClient: &http.Client{Timeout: 120 * time.Second}}
	return s
}

// UpdateConfig hot-reloads the configuration.
func (s *Server) UpdateConfig(cfg *config.Config) {
	s.cfg.Store(cfg)
}

// UpdateCatalog hot-reloads the model catalog.
func (s *Server) UpdateCatalog(cat *catalog.Catalog) {
	s.catalog.Store(cat)
}

// Start begins listening on the configured port.
func (s *Server) Start(ctx context.Context) error {
	cfg := s.cfg.Load()
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
	handler = loggingMiddleware(handler)
	handler = recoveryMiddleware(handler)
	handler = authMiddleware(cfg.Proxy.AuthToken, handler)

	s.httpServer = &http.Server{
		Addr:         net.JoinHostPort(cfg.Proxy.Host, strconv.Itoa(cfg.Proxy.Port)),
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("free-llm-proxy listening on %s (strategy: %s)", s.httpServer.Addr, cfg.Proxy.Strategy)
	if cfg.Proxy.AuthToken == "" && !isLoopbackHost(cfg.Proxy.Host) {
		log.Printf("warning: listening on %q without proxy.auth_token — anyone who can reach this port can spend your provider quotas", cfg.Proxy.Host)
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.httpServer.Shutdown(shutCtx)
	}()

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
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
	if msgs, ok := raw["messages"].([]any); ok {
		for _, msg := range msgs {
			if m, ok := msg.(map[string]any); ok {
				req.Messages = append(req.Messages, m)
			}
		}
	}
	if stream, ok := raw["stream"].(bool); ok {
		req.Stream = stream
	}
	if mt, ok := toInt(raw["max_tokens"]); ok {
		req.MaxTokens = mt
	}

	cfg := s.cfg.Load()
	cat := s.catalog.Load()

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
		// Re-read max_tokens in case defaults/overrides changed it.
		if mt, ok := toInt(raw["max_tokens"]); ok {
			req.MaxTokens = mt
		}
		// Redirect routing: specific model takes precedence over strategy.
		// A pinned model that has left the free catalog falls back to the
		// profile's strategy rather than failing every request.
		if profile.Model != "" && !catalogHasModel(cat, profile.Model) {
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

	// Check cache for non-streaming requests
	if !req.Stream {
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
			s.serveDirectModel(w, r, cfg, cat, req, raw)
			return
		}
		// It is a named strategy — fall through to strategy chain.
	}

	s.executeStrategyChain(w, r, cfg, cat, req, raw)
}

// serveDirectModel routes a request with a specific model ID to the right provider.
// Accepts both "model-id" and "provider/model-id" formats.
// If all direct-provider attempts fail, falls back to the full strategy chain.
func (s *Server) serveDirectModel(w http.ResponseWriter, r *http.Request, cfg *config.Config, cat *catalog.Catalog, req Request, raw map[string]any) {
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
		HTTPClient:         &http.Client{Timeout: 120 * time.Second},
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
	s.executeStrategyChain(w, r, cfg, cat, req, raw)
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
func (s *Server) executeStrategyChain(w http.ResponseWriter, r *http.Request, cfg *config.Config, cat *catalog.Catalog, req Request, raw map[string]any) {
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
		Catalog:            cat,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         &http.Client{Timeout: 120 * time.Second},
	}

	// Always use the fallback chain regardless of stream flag.
	// Streaming is stripped from provider requests in buildBody so the chain
	// always receives buffered JSON — this is required for fallback to work.
	resp, err := chain.Execute(r.Context(), req)
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

	if req.Stream && resp.StatusCode == http.StatusOK {
		if resp.UsedModel != "" {
			w.Header().Set("X-Used-Model", resp.UsedModel)
		}
		writeCompletionAsSSE(w, resp.Body)
		return
	}

	if !req.Stream && resp.StatusCode == http.StatusOK && !resp.Truncated {
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
	cat := s.catalog.Load()
	cfg := s.cfg.Load()
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
	if cat != nil {
		for _, e := range cat.FreeEntries() {
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


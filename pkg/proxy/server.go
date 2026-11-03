package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
		Addr:         fmt.Sprintf(":%d", cfg.Proxy.Port),
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		// A request may legitimately stay pending while no provider is
		// available, so there is no write deadline; the client's own timeout
		// (or disconnect) ends the wait.
		WriteTimeout: 0,
		BaseContext:  func(net.Listener) context.Context { return ctx },
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("free-llm-proxy listening on :%d (strategy: %s)", cfg.Proxy.Port, cfg.Proxy.Strategy)

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

// handleChatCompletions is the main OpenAI-compat chat endpoint.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
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
	if mt, ok := raw["max_tokens"].(float64); ok {
		req.MaxTokens = int(mt)
	}

	cfg := s.cfg.Load()
	cat := s.catalog.Load()

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
		if mt, ok := raw["max_tokens"].(float64); ok {
			req.MaxTokens = int(mt)
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
	for _, e := range cat.Entries {
		if !matchEntry(e, req.Model, targetProvider, targetModel) {
			continue
		}
		found = true
		provCfg := findProviderCfg(cfg, e.ProviderID)
		if provCfg == nil {
			continue
		}
		body := copyMap(raw)
		body["model"] = e.ModelID
		delete(body, "stream")
		for _, f := range anthropicOnlyFields {
			delete(body, f)
		}
		resp, err := chain.callProvider(r.Context(), *provCfg, body)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if req.Stream {
				writeCompletionAsSSE(w, resp.Body)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(resp.Body)
			return
		}
		// Any error or non-2xx — try next catalog entry.
	}
	if !found {
		http.Error(w, fmt.Sprintf(`{"error":"model %q not found in free catalog"}`, req.Model), http.StatusNotFound)
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
	// Execute keeps retrying until a provider gives a real answer; it only
	// returns an error if the client went away or fallback.max_wait elapsed.
	resp, sseStarted, err := executePending(w, r, chain, req)
	if err != nil {
		if r.Context().Err() != nil {
			log.Printf("request abandoned while waiting for a provider: %v", err)
			return // client disconnected or proxy shutting down; nobody to answer
		}
		log.Printf("no provider answered: %v", err)
		// Never answer with a fake completion: report the failure as an error.
		if sseStarted {
			// Status 200 is already on the wire; end the stream with an error event.
			payload, _ := json.Marshal(proxyErrorBody(err))
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		writeProxyError(w, http.StatusServiceUnavailable, err)
		return
	}

	if req.Stream && resp.StatusCode == http.StatusOK {
		if sseStarted {
			writeSSECompletion(w, resp.Body, true)
			return
		}
		if resp.UsedModel != "" {
			w.Header().Set("X-Used-Model", resp.UsedModel)
		}
		writeCompletionAsSSE(w, resp.Body)
		return
	}

	if !req.Stream && resp.StatusCode == http.StatusOK {
		s.cache.Set(raw, resp.Body)
	}

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if resp.UsedModel != "" {
		w.Header().Set("X-Used-Model", resp.UsedModel)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Body)
}

// sseKeepaliveInterval is how often a pending streaming request emits an SSE
// comment so clients and intermediaries don't drop the idle connection.
const sseKeepaliveInterval = 15 * time.Second

// executePending runs the fallback chain for a request that may stay pending
// for a long time. For streaming clients it starts the SSE response after the
// first interval and sends comment lines (": keep-alive") until a provider
// answers; these carry no completion data. sseStarted reports whether SSE
// headers were already sent.
func executePending(w http.ResponseWriter, r *http.Request, chain *FallbackChain, req Request) (resp *Response, sseStarted bool, err error) {
	if !req.Stream {
		resp, err = chain.Execute(r.Context(), req)
		return resp, false, err
	}
	type result struct {
		resp *Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := chain.Execute(r.Context(), req)
		done <- result{resp, err}
	}()
	ticker := time.NewTicker(sseKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case res := <-done:
			return res.resp, sseStarted, res.err
		case <-ticker.C:
			if !sseStarted {
				beginSSE(w)
				sseStarted = true
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

// proxyErrorBody builds an OpenAI-style error object (not a completion).
func proxyErrorBody(err error) map[string]any {
	return map[string]any{"error": map[string]any{
		"message": err.Error(),
		"type":    "service_unavailable",
		"code":    "no_provider_available",
	}}
}

// writeProxyError sends a real HTTP error status with an OpenAI-style error body.
func writeProxyError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "30")
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(proxyErrorBody(err))
}

// handleModels returns the list of available free models.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cat := s.catalog.Load()
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
	}
	var models []modelEntry
	for _, e := range cat.FreeEntries() {
		models = append(models, modelEntry{
			ID:     e.ModelID,
			Object: "model",
		})
	}
	resp := map[string]any{
		"object": "list",
		"data":   models,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleCompletions is a stub for text-completion requests.
// Most modern models only support chat; this returns a helpful error.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	http.Error(w, `{"error":"use /v1/chat/completions — legacy completions not supported"}`, http.StatusNotImplemented)
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


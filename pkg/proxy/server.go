package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/config"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/ratelimit"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/reliability"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/strategy"
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
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("picoclaw-proxy listening on :%d (strategy: %s)", cfg.Proxy.Port, cfg.Proxy.Strategy)

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

	// Check cache for non-streaming requests
	if !req.Stream {
		if cached := s.cache.Get(raw); cached != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			return
		}
	}

	// Select strategy: model field may be a strategy name, a provider/model ID, or "auto"/"".
	stratName := cfg.Proxy.Strategy
	if req.Model != "" && req.Model != "auto" {
		// Check if the model field is a known strategy name.
		if _, errLookup := s.strategyReg.Get(req.Model); errLookup == nil {
			stratName = req.Model
		} else {
			// Specific model requested — route directly.
			s.serveDirectModel(w, r, cfg, req, raw)
			return
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

	if req.Stream {
		// For streaming, we need to use the first ranked model
		freeEntries := cat.FreeEntries()
		stratReq := strategy.Request{
			Messages:  req.Messages,
			MaxTokens: req.MaxTokens,
			Stream:    true,
			Model:     req.Model,
		}
		ranked := strat.Rank(stratReq, freeEntries, nil)
		if len(ranked) > 0 {
			provCfg := findProviderCfg(cfg, ranked[0].ProviderID)
			if provCfg != nil {
				streamBody := buildStreamBody(raw, ranked[0].ModelID, ranked[0].ProviderID)
				if err := s.streamProxy.Forward(r.Context(), w, *provCfg, streamBody); err != nil {
					log.Printf("stream error: %v", err)
					http.Error(w, `{"error":"stream failed"}`, http.StatusBadGateway)
				}
				return
			}
		}
	}

	resp, err := chain.Execute(r.Context(), req)
	if err != nil {
		log.Printf("fallback chain exhausted: %v", err)
		http.Error(w, `{"error":"all providers exhausted"}`, http.StatusServiceUnavailable)
		return
	}

	// Cache successful non-streaming responses
	if !req.Stream && resp.StatusCode == http.StatusOK {
		s.cache.Set(raw, resp.Body)
	}

	// Forward the response
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

// serveDirectModel routes a request with a specific model ID to the right provider.
// Accepts both "model-id" and "provider/model-id" formats.
func (s *Server) serveDirectModel(w http.ResponseWriter, r *http.Request, cfg *config.Config, req Request, raw map[string]any) {
	// Parse optional "provider/model" prefix
	targetProvider := ""
	targetModel := req.Model
	if idx := strings.IndexByte(req.Model, '/'); idx >= 0 {
		targetProvider = req.Model[:idx]
		targetModel = req.Model[idx+1:]
	}

	cat := s.catalog.Load()
	for _, e := range cat.Entries {
		if e.ModelID == targetModel && e.IsFree &&
			(targetProvider == "" || e.ProviderID == targetProvider) {
			provCfg := findProviderCfg(cfg, e.ProviderID)
			if provCfg == nil {
				continue
			}
			body := copyMap(raw)
			body["model"] = e.ModelID // strip provider prefix before sending to API
			chain := &FallbackChain{
				Cfg:                cfg,
				Strategy:           nil, // not used for direct routing
				Catalog:            cat,
				RateLimiter:        s.rateLimiter,
				GeminiTracker:      s.geminiTracker,
				ReliabilityTracker: s.reliabilityTracker,
				HTTPClient:         &http.Client{Timeout: 120 * time.Second},
			}
			resp, err := chain.callProvider(r.Context(), *provCfg, body)
			if err != nil || resp.StatusCode >= 500 {
				break // fall through to normal routing
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(resp.Body)
			return
		}
	}
	// Not found in catalog — return 404
	http.Error(w, fmt.Sprintf(`{"error":"model %q not found in free catalog"}`, req.Model), http.StatusNotFound)
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

func findProviderCfg(cfg *config.Config, providerID string) *config.ProviderConfig {
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == providerID && cfg.Providers[i].Enabled {
			return &cfg.Providers[i]
		}
	}
	return nil
}

func buildStreamBody(raw map[string]any, modelID, _ string) map[string]any {
	body := copyMap(raw)
	body["model"] = modelID
	body["stream"] = true
	return body
}

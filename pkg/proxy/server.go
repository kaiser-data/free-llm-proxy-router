package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/alias"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// Server is the OpenAI-compatible proxy server.
type Server struct {
	cfg                atomic.Pointer[config.Config]
	catalog            atomic.Pointer[catalog.Catalog]
	strategyReg        *strategy.Registry
	rateLimiter        *ratelimit.GlobalTracker
	geminiTracker      *ratelimit.GeminiTracker
	reliabilityTracker *reliability.Tracker
	cache              *ResponseCache
	httpServer         *http.Server

	// aliasRes holds the canonical model alias resolver. It is rebuilt on
	// every config or catalog reload. A nil result (aliasing disabled) means
	// requests keep using raw model IDs only.
	aliasRes atomic.Pointer[alias.Resolver]

	// chainHTTPClient, when set, replaces the default upstream HTTP client
	// used by the direct, alias, and strategy routes. Tests inject a
	// short-timeout client to exercise timeout classification without
	// slowing the suite.
	chainHTTPClient *http.Client
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
	s.aliasRes.Store(buildAliasResolver(cfg, cat))
	return s
}

// UpdateConfig hot-reloads the configuration.
func (s *Server) UpdateConfig(cfg *config.Config) {
	s.cfg.Store(cfg)
	// Rebuild the alias layer from the new config against the current
	// catalog (pure computation over in-memory data — cheap per reload).
	s.aliasRes.Store(buildAliasResolver(cfg, s.catalog.Load()))
}

// UpdateCatalog hot-reloads the model catalog.
func (s *Server) UpdateCatalog(cat *catalog.Catalog) {
	s.catalog.Store(cat)
	// Rebuild the alias layer so auto-derived canonical groups pick up
	// added or removed upstream models.
	s.aliasRes.Store(buildAliasResolver(s.cfg.Load(), cat))
}

// buildAliasResolver constructs the canonical model alias resolver from the
// current config and catalog. It returns nil when neither canonicalization
// nor a public pool alias is configured; callers treat a nil resolver as
// "no canonical aliasing".
//
// Raw model IDs are reserved: a canonical name that exactly matches a raw
// upstream ID is skipped, so the alias layer never shadows a model that
// clients already request directly. The same rule applies to the public
// pool alias (models.public_alias) — a collision there disables the pool
// rather than shadowing the raw model.
func buildAliasResolver(cfg *config.Config, cat *catalog.Catalog) *alias.Resolver {
	if cfg == nil {
		return nil
	}
	pool := strings.ToLower(strings.TrimSpace(cfg.Models.PublicAlias))
	if !cfg.Models.Canonicalization.Enabled && pool == "" {
		return nil
	}
	var entries []catalog.CatalogEntry
	reserved := map[string]bool{}
	if cat != nil {
		entries = cat.Entries
		for _, e := range entries {
			reserved[strings.ToLower(e.ModelID)] = true
		}
	}
	opts := alias.Options{
		FreeOnly:          cfg.Models.Canonicalization.FreeOnly,
		Reserved:          reserved,
		DisableAutoGroups: !cfg.Models.Canonicalization.Enabled,
	}
	if pool != "" {
		if reserved[pool] {
			log.Printf("config: models.public_alias %q collides with a raw model ID — pool alias disabled", cfg.Models.PublicAlias)
		} else {
			opts.PoolAlias = pool
		}
	}
	return alias.NewResolver(entries, cfg.Models.Aliases, opts)
}

// upstreamHTTPClient returns the HTTP client used for upstream calls from
// the direct, alias, and strategy routes. Tests may inject a short-timeout
// client via chainHTTPClient; production builds one from
// fallback.provider_timeout_seconds (default 120s — previously hardcoded).
func (s *Server) upstreamHTTPClient() *http.Client {
	if s.chainHTTPClient != nil {
		return s.chainHTTPClient
	}
	return &http.Client{Timeout: s.providerTimeout()}
}

// providerTimeout returns the buffered upstream attempt timeout from
// fallback.provider_timeout_seconds, guarding against zero/negative values
// from hand-written or hot-reloaded configs (the viper default is 120).
func (s *Server) providerTimeout() time.Duration {
	seconds := s.cfg.Load().Fallback.ProviderTimeoutSeconds
	if seconds <= 0 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}

// streamHTTPClient returns the HTTP client for streaming attempts. A
// positive fallback.stream_timeout_seconds overrides the buffered timeout
// (long generations need a bigger cap than buffered calls — NG6); the
// default (0) inherits provider_timeout_seconds, matching the previous
// single 120s client. chainHTTPClient injection (tests) still wins.
func (s *Server) streamHTTPClient() *http.Client {
	if s.chainHTTPClient != nil {
		return s.chainHTTPClient
	}
	if seconds := s.cfg.Load().Fallback.StreamTimeoutSeconds; seconds > 0 {
		return &http.Client{Timeout: time.Duration(seconds) * time.Second}
	}
	return &http.Client{Timeout: s.providerTimeout()}
}

// proxyWriteTimeout returns the http.Server write timeout from
// proxy.write_timeout_seconds (default 120). It is the hard ceiling on a
// request's total server-side time — including all fallback attempts — so
// operators raising the per-attempt timeouts must raise this too.
func (s *Server) proxyWriteTimeout() time.Duration {
	seconds := s.cfg.Load().Proxy.WriteTimeoutSeconds
	if seconds <= 0 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}

// Start begins listening on the configured port.
func (s *Server) Start(ctx context.Context) error {
	cfg := s.cfg.Load()
	mux := http.NewServeMux()

	// OpenAI-compatible routes
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/completions", s.handleCompletions)
	// Operator visibility into the public pool alias (models.public_alias):
	// which provider/model pairs currently back it. Model identities only —
	// no credentials. Protected by the auth middleware when configured.
	mux.HandleFunc("/debug/pool", s.handleDebugPool)
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
		WriteTimeout: s.proxyWriteTimeout(),
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
	// Keep the client's original model name for header reporting across
	// routing redirections (agent profiles, alias-route fallback clearing).
	req.ClientModel = req.Model
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
	// "raw:" prefix bypasses the alias layer entirely.
	rawOnly := false
	if rest, ok := strings.CutPrefix(req.Model, "raw:"); ok && rest != "" {
		rawOnly = true
		req.Model = rest
		raw["model"] = rest
	}

	// Parse optional "provider/model" prefix.
	// Pass 1 (full exact match) handles IDs like "meta-llama/llama-3.3-70b-instruct:free"
	// where "/" is part of the model ID. Pass 2 handles "groq/llama-3.3-70b-versatile".
	targetProvider := ""
	targetModel := req.Model
	if idx := strings.IndexByte(req.Model, '/'); idx >= 0 {
		targetProvider = req.Model[:idx]
		targetModel = req.Model[idx+1:]
	}

	var resolver *alias.Resolver
	if !rawOnly {
		resolver = s.aliasRes.Load()
	}
	chain := &FallbackChain{
		Cfg:                cfg,
		Strategy:           nil,
		Catalog:            cat,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         s.upstreamHTTPClient(),
		Resolver:           resolver,
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
		for _, f := range anthropicOnlyFields {
			delete(body, f)
		}
		if req.Stream {
			// Streaming request: same SSE pass-through and fallback
			// semantics as the alias route. canonicalModel is empty here —
			// the direct route reports no X-Used-Model on success, matching
			// its buffered behavior.
			if !s.streamOneCandidate(w, r, provCfg, body, "", req.Model, "direct route") {
				continue
			}
			return
		}
		delete(body, "stream") // buffered path: fallback requires complete JSON
		resp, err := chain.callProvider(r.Context(), *provCfg, body)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(resp.Body)
			return
		}
		// Non-retryable client/request errors (400, 401, 403, …) surface
		// immediately: no next catalog entry, no strategy chain, and no
		// masked success. Availability failures keep walking below.
		if classifyUpstreamFailure(resp, err) == nonRetryable {
			if resp != nil {
				log.Printf("direct route: %s/%s non-retryable status %d — surfacing to client", e.ProviderID, e.ModelID, resp.StatusCode)
				s.writeUpstreamError(w, req.Model, resp)
			} else {
				log.Printf("direct route: %s/%s request canceled — aborting walk", e.ProviderID, e.ModelID)
			}
			return
		}
		// Any retryable error or non-2xx — try next catalog entry.
	}
	if !found {
		if s.serveAliasModel(w, r, cfg, cat, req, raw, resolver) {
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"model %q not found in free catalog"}`, req.Model), http.StatusNotFound)
		return
	}
	// Model found but all direct attempts failed — fall back to strategy chain.
	log.Printf("direct route: all providers failed for %q — falling back to strategy chain", req.Model)
	req.Model = ""
	raw["model"] = ""
	s.executeStrategyChain(w, r, cfg, cat, req, raw)
}

// serveAliasModel routes a request for a canonical model name to its ordered
// upstream candidates. The first healthy upstream wins; if all fail, the
// request falls back to the default strategy chain. Returns false when the
// requested model is not a canonical alias, letting the caller keep its
// existing 404 behavior.
func (s *Server) serveAliasModel(w http.ResponseWriter, r *http.Request, cfg *config.Config, cat *catalog.Catalog, req Request, raw map[string]any, resolver *alias.Resolver) bool {
	ups := resolver.Upstreams(req.Model)
	if len(ups) == 0 {
		return false
	}
	if resolver.IsPool(req.Model) {
		log.Printf("pool alias: %q -> %d free candidate(s)", req.Model, len(ups))
		ups = s.rankPoolCandidates(cfg, req, resolver, cat, req.Model, ups)
	} else {
		log.Printf("canonical alias: %q -> %d upstream candidate(s)", req.Model, len(ups))
	}

	chain := &FallbackChain{
		Cfg:                cfg,
		Strategy:           nil,
		Catalog:            cat,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         s.upstreamHTTPClient(),
		Resolver:           resolver,
	}
	for _, u := range ups {
		provCfg := findProviderCfg(cfg, u.ProviderID)
		if provCfg == nil {
			continue
		}
		body := copyMap(raw)
		body["model"] = u.ModelID
		for _, f := range anthropicOnlyFields {
			delete(body, f)
		}
		if req.Stream {
			// Streaming request: keep stream:true in the body and pass the
			// upstream SSE through incrementally. streamOneCandidate only
			// walks to the next candidate while nothing has been written
			// downstream; once the stream starts the attempt is terminal.
			if !s.streamOneCandidate(w, r, provCfg, body, req.Model, req.Model, "alias route") {
				continue
			}
			return true
		}
		delete(body, "stream") // buffered path: fallback requires complete JSON
		resp, err := chain.callProvider(r.Context(), *provCfg, body)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Printf("alias route: %s/%s served %q", u.ProviderID, u.ModelID, req.Model)
			// Expose the raw upstream identity (documented contract) while
			// X-Used-Model reports the canonical name. Only the router-owned
			// headers are forwarded — upstream transport headers like
			// Content-Length/Content-Encoding describe the buffered body
			// callProvider read, not the bytes we are about to write.
			if v := resp.Header.Get("X-Free-Router-Upstream-Provider"); v != "" {
				w.Header().Set("X-Free-Router-Upstream-Provider", v)
			}
			if v := resp.Header.Get("X-Free-Router-Upstream-Model"); v != "" {
				w.Header().Set("X-Free-Router-Upstream-Model", v)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Used-Model", req.Model)
			w.WriteHeader(resp.StatusCode)
			w.Write(resp.Body)
			return true
		}
		// Client/request errors (400, 401, 403, …) must not walk more
		// candidates: no other upstream can succeed with the same payload,
		// and continuing would mask the real error behind an exhaustion
		// response. Surface the upstream response to the client as-is.
		if classifyUpstreamFailure(resp, err) == nonRetryable {
			if resp != nil {
				log.Printf("alias route: %s/%s non-retryable status %d — surfacing to client, no further candidates", u.ProviderID, u.ModelID, resp.StatusCode)
				s.writeUpstreamError(w, req.Model, resp)
			} else {
				log.Printf("alias route: %s/%s request canceled — aborting candidate walk", u.ProviderID, u.ModelID)
			}
			return true
		}
		// Retryable upstream failure — record the candidate and the
		// failure class, then try the next one (cooldown/retry already
		// handled by callProvider callers upstream of this point).
		if err != nil {
			log.Printf("alias route: %s/%s error: %v — trying next candidate", u.ProviderID, u.ModelID, err)
		} else {
			log.Printf("alias route: %s/%s status %d — trying next candidate", u.ProviderID, u.ModelID, resp.StatusCode)
		}
	}
	// All alias upstreams failed — fall back to the default strategy chain.
	log.Printf("alias route: all upstreams failed for %q — falling back to strategy chain", req.Model)
	req.Model = ""
	raw["model"] = ""
	s.executeStrategyChain(w, r, cfg, cat, req, raw)
	return true
}

// rankPoolCandidates orders the global free-pool alias candidates with the
// existing routing strategy machinery instead of raw catalog order — the
// pool must respect the same health/reliability/latency preferences as the
// strategy chain. No new routing engine is invented: the configured strategy
// (proxy.strategy) ranks the pool entries, and the walk order is the only
// output. On top of the strategy ranking, candidates whose recorded context
// window cannot hold the request (estimated prompt + requested completion)
// are demoted behind fitting ones rather than dropped — context metadata may
// be missing or stale, and the upstream still gets the final say. Retry,
// cooldown, 429 handling, and fallback semantics are untouched.
//
// When no strategy registry is available (tests) or the strategy yields no
// ranking, the original catalog order is kept.
func (s *Server) rankPoolCandidates(cfg *config.Config, req Request, resolver *alias.Resolver, cat *catalog.Catalog, name string, ups []alias.Upstream) []alias.Upstream {
	if len(ups) < 2 {
		return ups
	}

	stratReq := strategy.Request{
		Messages:              req.Messages,
		EstimatedPromptTokens: estimateTokens(req.Messages),
		MaxTokens:             req.MaxTokens,
		Stream:                req.Stream,
		Model:                 name,
	}

	rank := map[string]int{}
	if s.strategyReg != nil {
		stratName := cfg.Proxy.Strategy
		if stratName == "" {
			stratName = "adaptive"
		}
		if strat, err := s.strategyReg.Get(stratName); err == nil && strat != nil {
			entries := resolver.Candidates(cat, name)
			for i, r := range strat.Rank(stratReq, entries, nil) {
				rank[r.ProviderID+"/"+r.ModelID] = i
			}
		}
	}
	if len(rank) == 0 {
		return ups // no usable strategy — keep catalog order
	}

	// Context-fit demotion: needed tokens vs the candidate's recorded
	// context window (only when the metadata carries one).
	needed := stratReq.EstimatedPromptTokens
	if req.MaxTokens > 0 {
		needed += req.MaxTokens
	}
	type ordered struct {
		up   alias.Upstream
		rank int
		fits bool
	}
	list := make([]ordered, 0, len(ups))
	for i, u := range ups {
		o := ordered{up: u, rank: len(ups) + i, fits: true} // unranked → last, stable
		if r, ok := rank[u.Key()]; ok {
			o.rank = r
		}
		if e := cat.Find(u.ProviderID, u.ModelID); e != nil && e.ContextWindow > 0 && needed > e.ContextWindow {
			o.fits = false
		}
		list = append(list, o)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].fits != list[j].fits {
			return list[i].fits // fitting candidates first
		}
		return list[i].rank < list[j].rank
	})
	out := make([]alias.Upstream, 0, len(list))
	for _, o := range list {
		out = append(out, o.up)
	}
	return out
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
		HTTPClient:         s.upstreamHTTPClient(),
		Resolver:           s.aliasRes.Load(),
	}

	// Always use the fallback chain regardless of stream flag.
	// Streaming is stripped from provider requests in buildBody so the chain
	// always receives buffered JSON — this is required for fallback to work.
	resp, err := chain.Execute(r.Context(), req)
	if err != nil {
		log.Printf("fallback chain exhausted: %v", err)
		// Candidate exhaustion is an upstream availability failure: answer
		// 503 with a JSON error body + Retry-After so clients (and agents)
		// can tell a proxy outage apart from a real completion and know
		// when to come back. The previous 200 with a canned "temporarily
		// unavailable" completion masked the failure (gap NG1 in the
		// scenario-matrix report).
		s.writeExhaustionError(w, req.ClientModel, err)
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
		// Pool-alias requests keep the public identity: the client asked
		// for kiwi-auto and must keep seeing kiwi-auto in X-Used-Model even
		// when the strategy chain served the request after the alias walk
		// exhausted — the real upstream stays in the X-Free-Router-Upstream-*
		// headers.
		if res := s.aliasRes.Load(); res != nil && res.IsPool(req.ClientModel) {
			w.Header().Set("X-Used-Model", strings.ToLower(strings.TrimSpace(req.ClientModel)))
		} else {
			w.Header().Set("X-Used-Model", resp.UsedModel)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Body)
}

// handleModels returns the list of available free models. Client-visible
// exposure is configurable via models.expose_raw and models.expose_canonical
// (both default true — the previous behaviour of advertising raw IDs
// followed by canonical alias names). Disabling both is treated as a
// misconfiguration and falls back to exposing both — unless a public pool
// alias (models.public_alias) is configured, in which case the listing is
// exactly that one model. Raw IDs remain requestable and routable
// regardless of these flags — they only filter the listing.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	cat := s.catalog.Load()
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by,omitempty"`
	}
	// Public pool alias mode: ONE client-visible model name representing
	// the entire discovered free pool. Overrides the expose flags — raw
	// IDs and canonical families stay requestable and routable (advanced/
	// raw use), they just are not advertised.
	if res := s.aliasRes.Load(); res != nil {
		if pool := res.PoolAlias(); pool != "" {
			resp := map[string]any{
				"object": "list",
				"data": []modelEntry{{
					ID:      pool,
					Object:  "model",
					OwnedBy: "free-llm-proxy",
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
	}
	exposeRaw, exposeCanonical := cfg.Models.ExposeRaw, cfg.Models.ExposeCanonical
	if !exposeRaw && !exposeCanonical {
		exposeRaw, exposeCanonical = true, true
	}
	// Start non-nil so an empty catalog encodes as [] instead of null
	// (some OpenAI-compatible clients reject a null data array).
	models := make([]modelEntry, 0)
	listed := map[string]bool{}
	if exposeRaw {
		for _, e := range cat.FreeEntries() {
			listed[strings.ToLower(e.ModelID)] = true
			models = append(models, modelEntry{
				ID:     e.ModelID,
				Object: "model",
			})
		}
	}
	// Canonical names come from the live resolver. Raw model IDs are
	// reserved by the alias layer, so a name collision cannot happen; the
	// listed check is belt-and-braces.
	if exposeCanonical {
		if res := s.aliasRes.Load(); res != nil {
			for _, name := range res.Names() {
				if listed[name] {
					continue
				}
				listed[name] = true
				models = append(models, modelEntry{
					ID:     name,
					Object: "model",
				})
			}
		}
	}
	resp := map[string]any{
		"object": "list",
		"data":   models,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDebugPool exposes the real candidate pool behind the public alias
// for operators. Clients see only the pool alias name; this endpoint shows
// which provider/model pairs currently back it — provider IDs and model IDs
// only, never credentials (auth middleware still applies when an auth token
// is configured).
func (s *Server) handleDebugPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	res := s.aliasRes.Load()
	cat := s.catalog.Load()
	if res == nil || res.PoolAlias() == "" {
		http.Error(w, `{"error":"no pool alias active (models.public_alias unset, reserved-name collision, or no eligible free candidates)"}`, http.StatusNotFound)
		return
	}
	pool := res.PoolAlias()
	ups := res.Upstreams(pool)
	type candidate struct {
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		ContextWindow int    `json:"context_window,omitempty"`
	}
	list := make([]candidate, 0, len(ups))
	for _, u := range ups {
		c := candidate{Provider: u.ProviderID, Model: u.ModelID}
		if e := cat.Find(u.ProviderID, u.ModelID); e != nil {
			c.ContextWindow = e.ContextWindow
		}
		list = append(list, c)
	}
	resp := map[string]any{
		"pool":       pool,
		"candidates": len(list),
		"upstreams":  list,
	}
	if cat != nil && !cat.UpdatedAt.IsZero() {
		resp["catalog_updated_at"] = cat.UpdatedAt
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


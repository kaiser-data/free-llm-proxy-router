package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// newRequestID mints a 16-hex-char request id; the crypto fallback keeps
// startup working even if the entropy source fails.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// requestIDFor reuses a usable incoming X-Request-Id (printable ASCII, ≤64
// chars) or mints one, and pins it into the response headers so every route
// answers with it (task F).
func requestIDFor(w http.ResponseWriter, r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if len(id) > 64 {
		id = ""
	}
	for i := 0; id != "" && i < len(id); i++ {
		if c := id[i]; c < 0x21 || c > 0x7E {
			id = ""
			break
		}
	}
	if id == "" {
		id = newRequestID()
	}
	w.Header().Set("X-Request-Id", id)
	return id
}

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

	// slotLimit reserves RPM request slots per candidate (task L0); queues
	// parks slotless requests FIFO per model group until the earliest
	// next_available timer wakes them. Both are shared across all requests.
	slotLimit *SlotLimiter
	queues    *queueManager

	// demotions is the cross-request unknown-400 demotion registry (round
	// 8 steps 3–4) shared by every request's fallback chain; the TTL is
	// read from the live config so a hot-reload takes effect immediately.
	demotions *demotionRegistry

	// latencies is the cross-request slow-TTFT tracker (round 10) shared
	// by every request's fallback chain and the direct/alias streaming
	// routes: per-model TTFT EMA + consecutive-slow circuit breaker.
	// The breaker demotes into `demotions`; snapshots are exposed at
	// GET /debug/latency.
	latencies *latencyRegistry
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
	s.slotLimit = NewSlotLimiter()
	s.queues = newQueueManager(s.slotLimit)
	s.demotions = newDemotionRegistry()
	s.latencies = newLatencyRegistry()
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

// proxyWriteTimeout was REMOVED (task S): the retired
// proxy.write_timeout_seconds knob no longer sets anything — the
// server-level WriteTimeout is disabled and deadlines are per-request
// (write_idle_timeout_seconds + max_request_seconds, writedeadline.go).

// adjustedRequestConfig returns the config the request walks with. When
// slot queueing is on (task L0) it raises the total deadline to the queue
// floor — effective_total >= queue_wait + the per-attempt ceiling — so a
// queued request can wait its full budget and still get one attempt. The
// copy shares every slice; only TotalDeadlineSeconds is written.
func (s *Server) adjustedRequestConfig(cfg *config.Config) *config.Config {
	qw := cfg.Fallback.QueueWaitSeconds
	if qw <= 0 {
		return cfg
	}
	attempt := cfg.Fallback.ProviderTimeoutSeconds
	if attempt <= 0 {
		attempt = 120 // mirror the viper default for zeroed structs
	}
	for i := range cfg.Providers {
		if !cfg.Providers[i].Enabled {
			continue
		}
		if d := cfg.Providers[i].ResolvedTimeout(); int(d.Seconds()) > attempt {
			attempt = int(d.Seconds())
		}
	}
	floor := qw + attempt
	if cfg.Fallback.TotalDeadlineSeconds >= floor {
		return cfg
	}
	adj := *cfg
	adj.Fallback.TotalDeadlineSeconds = floor
	return &adj
}

// candidatesBlocked parks a request whose entire candidate list was
// skipped for lack of RPM slots (task L0): it waits FIFO in the model
// group's queue — woken by the earliest next_available timer — and returns
// whether the route may walk its candidates again. When the wait cannot
// continue the response is finished here: the exhaustion 503 + Retry-After
// (or the structured SSE error event when the keepalive header already
// went out); false with errClientGone means nobody is left to answer.
func (s *Server) candidatesBlocked(ctx context.Context, w http.ResponseWriter, group, clientModel, requestID string, keys []string) bool {
	c := s.cfg.Load()
	if s.queues == nil || c.Fallback.QueueWaitSeconds <= 0 {
		return false
	}
	earliest, _ := s.slotLimit.Earliest(keys)
	// The queue wait is a WAITING phase (task P): a streaming request
	// parks with the SSE keepalive armed.
	armKeepalive(w)
	served, err := s.queues.wait(ctx, group, keys, earliest, time.Duration(c.Fallback.QueueWaitSeconds)*time.Second, queueMaxWaiters(c), true)
	if served {
		log.Printf("slot queue: req=%s group %s handed its turn to %q", requestID, group, clientModel)
		return true
	}
	if errors.Is(err, errClientGone) {
		log.Printf("slot queue: req=%s group %s client gone — left the queue, no slot held", requestID, group)
		return false
	}
	s.writeExhaustionError(w, clientModel, err)
	return false
}

// queueMaxWaiters returns the effective per-queue waiter cap (viper
// default 64 for zeroed Config structs).
func queueMaxWaiters(c *config.Config) int {
	if s := c.Fallback.QueueMaxWaiters; s > 0 {
		return s
	}
	return 64
}

// wrapStream installs the SSE keepalive writer on a streaming request
// (task P). It returns the writer the route must use and the wrapper
// itself — nil when keepalive is disabled (after=0) or already installed.
// Non-streaming requests are never wrapped: they wait silently within the
// deadline and answer with their real status code.
func (s *Server) wrapStream(w http.ResponseWriter, requestID string) (http.ResponseWriter, *keepaliveWriter) {
	if keepaliveOf(w) != nil {
		return w, nil
	}
	c := s.cfg.Load()
	after := time.Duration(c.Fallback.StreamKeepaliveAfterSeconds) * time.Second
	if after <= 0 {
		return w, nil
	}
	interval := time.Duration(c.Fallback.StreamKeepaliveIntervalSeconds) * time.Second
	kw := newKeepaliveWriter(w, requestID, after, interval)
	return kw, kw
}

// finishStreamRoute ends a streaming route after the walk failed. Before
// any header: the exhaustion 503 + Retry-After (the status is still free).
// After the keepalive header went out (task P): the structured OpenAI-style
// SSE error event + data: [DONE] — the status can no longer change, and a
// fake success is out of the question either way.
func (s *Server) finishStreamRoute(kw *keepaliveWriter, w http.ResponseWriter, clientModel, requestID string, err error) {
	if kw != nil && kw.HeaderSent() {
		log.Printf("stream route: req=%s keepalive header already sent — ending with the SSE error event + [DONE]", requestID)
		flusher, canFlush := flusherOf(w)
		writeSSEErrorEventMsg(w, flusher, canFlush,
			fmt.Sprintf("request failed after the response started: %v", err),
			"upstream_failed_after_header")
		return
	}
	if errors.Is(err, errClientGone) {
		return // nobody left to answer
	}
	s.writeExhaustionError(w, clientModel, err)
}

// firstTokenTimeout returns the streaming pre-commit hold cap from
// fallback.first_token_timeout_seconds (default 30).
// firstTokenTimeoutFor resolves the per-provider/model TTFT override
// (fallback.first_token_timeouts, task J) against the loaded config,
// falling back to the global first_token_timeout_seconds.
func (s *Server) firstTokenTimeoutFor(providerID, modelID string) time.Duration {
	if seconds := config.LookupFirstTokenTimeout(s.cfg.Load().Fallback.FirstTokenTimeouts, providerID, modelID); seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return s.firstTokenTimeout()
}

func (s *Server) firstTokenTimeout() time.Duration {
	if seconds := s.cfg.Load().Fallback.FirstTokenTimeoutSeconds; seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 30 * time.Second
}

// streamIdleTimeout returns the post-commit per-read gap cap from
// fallback.stream_idle_timeout_seconds (default 30).
func (s *Server) streamIdleTimeout() time.Duration {
	if seconds := s.cfg.Load().Fallback.StreamIdleTimeoutSeconds; seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 30 * time.Second
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
	// Operator visibility into the slow-TTFT tracker (round 10): per
	// provider/model TTFT EMA, samples, consecutive-slow streaks and
	// breaker trips. Model identities and timings only — no credentials,
	// no prompt content. Protected by the auth middleware when configured.
	mux.HandleFunc("/debug/latency", s.handleDebugLatency)
	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	var handler http.Handler = mux
	handler = loggingMiddleware(handler)
	handler = recoveryMiddleware(handler)
	handler = authMiddleware(cfg.Proxy.AuthToken, handler)
	// Task S: outermost so EVERY response write path (auth errors, logs,
	// buffered bodies, SSE keepalive comments, stream chunks) carries the
	// idle-refreshing write deadline.
	handler = s.withWriteDeadlines(handler)

	// Task S: the server-level WriteTimeout is DISABLED — it was a total
	// ceiling that cut flowing streams mid-generation. Deadlines are
	// per-request: the outer withWriteDeadlines middleware sets each
	// request's initial ceiling (proxy.max_request_seconds) and refreshes
	// it (proxy.write_idle_timeout_seconds) after every successful write —
	// see writedeadline.go.
	// Round 15: ReadTimeout raised 30s→120s — a 90k-token request body
	// (~360KB) trickling through a slow WireGuard link must not trip the
	// read deadline; that kill is invisible server-side (no handler log)
	// and the client just sees a bare connection reset.
	//
	// Round 15: IdleTimeout raised 60s→900s — panel clients pool keep-alive
	// connections across the tunnel. When this server closed an idle conn
	// at 60s and the panel later wrote into it, the kernel answered RST and
	// the panel surfaced "read: connection reset by peer" (scanner_error)
	// with no router-side log at all — the request never reached the
	// handler. 15 min keeps pooled conns alive far longer than any realistic
	// reuse gap while still reaping truly dead connections.
	s.httpServer = &http.Server{
		Addr:        fmt.Sprintf(":%d", cfg.Proxy.Port),
		Handler:     handler,
		ReadTimeout: 120 * time.Second,
		IdleTimeout: 900 * time.Second,
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
	// Request id: honor the client's X-Request-Id or mint one — the response
	// header and every per-attempt log line carry it (task F).
	req.RequestID = requestIDFor(w, r)

	// SSE keepalive (task P): a streaming request that ends up WAITING —
	// slot queue or pre-commit hold — past stream_keepalive_after_seconds
	// gets the 200 header + ": keepalive" comments. One wrapper for the
	// whole request; every route writes through it.
	var kw *keepaliveWriter
	if req.Stream {
		w, kw = s.wrapStream(w, req.RequestID)
		if kw != nil {
			defer kw.stop()
		}
	}

	cfg := s.cfg.Load()
	cat := s.catalog.Load()
	// Slot-queue floor (task L0): when queueing is on, the request's total
	// deadline must not be lower than queue_wait + one attempt, otherwise a
	// queued request could never wait its budget and still get an attempt.
	cfg = s.adjustedRequestConfig(cfg)

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
		log.Printf("req=%s agent profile resolved: model=%q strategy=%q", req.RequestID, profile.Model, profile.Strategy)
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
	directDemand := demandOf(req.Raw, req.Messages, req.MaxTokens)
	chain := &FallbackChain{
		Cfg:                cfg,
		Strategy:           nil,
		Catalog:            cat,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         s.upstreamHTTPClient(),
		StreamHTTPClient:   s.streamHTTPClient(),
		Resolver:           resolver,
		Slots:              s.slotLimit,
		Queues:             s.queues,
		Demotions:          s.demotions,
		Latencies:          s.latencies,
		Gate:               &directDemand,
	}

	found := false
	// unknown400Seen is the per-request cross-model confirm state (round 8
	// step 2) for the direct route's catalog-entry walk — the policy walks
	// to the next entry until unknown_400_confirm_n distinct models failed
	// the same unrecognized way.
	directSeen400 := make(map[string]bool)
	for {
		entered, blockedKeys, gateSkipped := 0, []string{}, 0
		for _, e := range cat.Entries {
			if !matchEntry(e, req.Model, targetProvider, targetModel) {
				continue
			}
			found = true
			provCfg := findProviderCfg(cfg, e.ProviderID)
			if provCfg == nil {
				continue
			}
			// Capability gate (task Q): NG8-flagged, over-context, and
			// capability-mismatched entries are skipped before anything
			// waits — an all-skipped set answers immediately below.
			if !entryPassesGate(e, *chain.Gate) {
				gateSkipped++
				continue
			}
			// RPM slot gate (task L0): entering the candidate consumes one
			// atomic reservation; a slotless candidate is collected for the
			// group queue and costs no hop and no attempt.
			if !slotEnter(s.slotLimit, provCfg, e.ModelID, &blockedKeys) {
				continue
			}
			entered++
			body := copyMap(raw)
			body["model"] = e.ModelID
			for _, f := range anthropicOnlyFields {
				delete(body, f)
			}
			// Context truncation (round 10): the direct route speaks to
			// the same fragile free upstreams — trim before sending.
			applyContextTruncation(cfg, body)
			if req.Stream {
				// Streaming request: same SSE pass-through and fallback
				// semantics as the alias route. canonicalModel is empty here —
				// the direct route reports no X-Used-Model on success, matching
				// its buffered behavior.
				if !s.streamOneCandidate(w, r, provCfg, body, "", req.Model, "direct route", req.RequestID, chain, directSeen400) {
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
			// Route decision comes from ClassifyFailure like every other route
			// (task H — no route keeps its own failure table): fail-fast
			// client/request defects (400, 413, content-policy, …) surface
			// immediately with no masked success; 401/403 cool the provider down
			// and keep walking; every other retryable class (rate limits, 5xx,
			// transport) also walks to the next catalog entry. The unknown-400
			// branch consults the cross-model confirm policy (round 8 step 2).
			switch chain.failureDecisionWalk(resp, err, e.ProviderID, e.ModelID, directSeen400) {
			case FailFast:
				if resp != nil {
					log.Printf("direct route: req=%s %s/%s fail-fast status %d — surfacing to client", req.RequestID, e.ProviderID, e.ModelID, resp.StatusCode)
					s.writeUpstreamError(w, req.Model, resp)
				} else {
					log.Printf("direct route: req=%s %s/%s request canceled — aborting walk", req.RequestID, e.ProviderID, e.ModelID)
				}
				return
			default:
				if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
					noteAuthFailure(s.rateLimiter, e.ProviderID, e.ModelID, resp.StatusCode)
				}
				// Try the next catalog entry.
			}
		}
		// Route disposition when the round entered nothing:
		if !found {
			break // no direct entry matched — the caller falls to the alias route
		}
		if entered > 0 {
			break // entries were entered and failed — strategy chain as before
		}
		if len(blockedKeys) > 0 {
			// Matched candidates were skipped for quota: wait FIFO in the
			// group queue instead of failing immediately (task L0). A
			// handed turn walks again; otherwise the response is finished.
			if !s.candidatesBlocked(r.Context(), w, queueGroup(req), req.Model, req.RequestID, blockedKeys) {
				return
			}
			continue
		}
		if gateSkipped > 0 {
			// Round 9: a tools/vision-only mismatch is relaxed and the
			// entries are re-entered — the upstream verdict beats stale
			// metadata (an upstream tools-400 then walks on via
			// bad400Tools). Only a context/NG8-only mismatch answers the
			// immediate no-capable-candidate error (task Q).
			if chain.Gate.tools || chain.Gate.vision {
				*chain.Gate = relaxCapabilityDemand(*chain.Gate)
				continue
			}
			s.writeNoCapableCandidate(w, req.Model, *chain.Gate)
			return
		}
		break // matched only disabled providers — fall to the alias route
	}
	if !found {
		if s.serveAliasModel(w, r, cfg, cat, req, raw, resolver) {
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"model %q not found in free catalog"}`, req.Model), http.StatusNotFound)
		return
	}
	// Model found but all direct attempts failed — fall back to strategy chain.
	log.Printf("direct route: req=%s all providers failed for %q — falling back to strategy chain", req.RequestID, req.Model)
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
	// Capability gate BEFORE the queue (task Q): candidates the request
	// cannot use — NG8-flagged, too small a context window, missing
	// tools/vision — are dropped first. An empty surviving set fails
	// immediately with the clear error; it never waits in the slot queue.
	demand := demandOf(req.Raw, req.Messages, req.MaxTokens)
	ups = s.gateUpstreams(cat, req, ups)
	if len(ups) == 0 {
		log.Printf("alias route: req=%s %q — no candidate passes the capability gate (NG8/context/tools/vision)", req.RequestID, req.Model)
		s.writeNoCapableCandidate(w, req.Model, demand)
		return true
	}
	if resolver.IsPool(req.Model) {
		log.Printf("pool alias: req=%s %q -> %d free candidate(s)", req.RequestID, req.Model, len(ups))
		ups = s.rankPoolCandidates(cfg, req, resolver, cat, req.Model, ups)
	} else {
		log.Printf("canonical alias: req=%s %q -> %d upstream candidate(s)", req.RequestID, req.Model, len(ups))
	}

	chain := &FallbackChain{
		Cfg:                cfg,
		Strategy:           nil,
		Catalog:            cat,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         s.upstreamHTTPClient(),
		StreamHTTPClient:   s.streamHTTPClient(),
		Resolver:           resolver,
		Slots:              s.slotLimit,
		Queues:             s.queues,
		Demotions:          s.demotions,
		Latencies:          s.latencies,
		Gate:               &demand,
	}
	// unknown400Seen is the per-request cross-model confirm state (round 8
	// step 2) for the alias route's candidate walk.
	aliasSeen400 := make(map[string]bool)
	for {
		entered, blockedKeys := 0, []string{}
		// Unknown-400 demotion (round 8 steps 3–4): models an earlier
		// request proved unrecognized-400-broken leave the candidate list
		// until their TTL lapses; when every candidate is demoted they are
		// handed back (last chance) rather than starving the request.
		ups = chain.filterDemotedUpstreams(ups)
		// Slow-TTFT deprioritization (round 10): candidates whose
		// measured EMA TTFT sits at/above the slow threshold move to the
		// back of the candidate list (never dropped) — the same real-time
		// rule the ranked walk applies to its ranking.
		ups = chain.deprioritizeSlowUpstreams(ups)
		for _, u := range ups {
			provCfg := findProviderCfg(cfg, u.ProviderID)
			if provCfg == nil {
				continue
			}
			// RPM slot gate (task L0): entering the candidate consumes one
			// atomic reservation; a slotless candidate is collected for the
			// group queue and costs no hop and no attempt.
			if !slotEnter(s.slotLimit, provCfg, u.ModelID, &blockedKeys) {
				continue
			}
			entered++
			body := copyMap(raw)
			body["model"] = u.ModelID
			for _, f := range anthropicOnlyFields {
				delete(body, f)
			}
			// Context truncation (round 10): the alias/pool route speaks
			// to the same fragile free upstreams — trim before sending.
			applyContextTruncation(cfg, body)
			if req.Stream {
				// Streaming request: keep stream:true in the body and pass the
				// upstream SSE through incrementally. streamOneCandidate only
				// walks to the next candidate while nothing has been written
				// downstream; once the stream starts the attempt is terminal.
				if !s.streamOneCandidate(w, r, provCfg, body, req.Model, req.Model, "alias route", req.RequestID, chain, aliasSeen400) {
					continue
				}
				return true
			}
			delete(body, "stream") // buffered path: fallback requires complete JSON
			resp, err := chain.callProvider(r.Context(), *provCfg, body)
			if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Printf("alias route: req=%s %s/%s served %q", req.RequestID, u.ProviderID, u.ModelID, req.Model)
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
			// Route decision comes from ClassifyFailure like every other route
			// (task H): fail-fast client/request defects surface the upstream
			// response to the client verbatim; 401/403 cool the provider down
			// and walk on; other retryable classes walk to the next candidate.
			// The unknown-400 branch consults the cross-model confirm policy
			// (round 8 step 2).
			switch chain.failureDecisionWalk(resp, err, u.ProviderID, u.ModelID, aliasSeen400) {
			case FailFast:
				if resp != nil {
					log.Printf("alias route: req=%s %s/%s fail-fast status %d — surfacing to client, no further candidates", req.RequestID, u.ProviderID, u.ModelID, resp.StatusCode)
					s.writeUpstreamError(w, req.Model, resp)
				} else {
					log.Printf("alias route: req=%s %s/%s request canceled — aborting candidate walk", req.RequestID, u.ProviderID, u.ModelID)
				}
				return true
			default:
				if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
					noteAuthFailure(s.rateLimiter, u.ProviderID, u.ModelID, resp.StatusCode)
				}
				// Retryable failure — try the next candidate.
			}
			// Retryable upstream failure — record the candidate and the
			// failure class, then try the next one (cooldown/retry already
			// handled by callProvider callers upstream of this point).
			if err != nil {
				log.Printf("alias route: req=%s %s/%s error: %v — trying next candidate", req.RequestID, u.ProviderID, u.ModelID, err)
			} else {
				log.Printf("alias route: req=%s %s/%s status %d — trying next candidate", req.RequestID, u.ProviderID, u.ModelID, resp.StatusCode)
			}
		}
		// No upstream was entered this round but candidates were skipped
		// for quota: wait FIFO in the group queue instead of failing
		// immediately (task L0). A handed turn walks again; otherwise the
		// response is already finished (503 + Retry-After, or the SSE
		// error event when the keepalive header went out).
		if entered > 0 || len(blockedKeys) == 0 {
			break
		}
		if !s.candidatesBlocked(r.Context(), w, queueGroup(req), req.ClientModel, req.RequestID, blockedKeys) {
			return true
		}
	}
	// All alias upstreams failed — fall back to the default strategy chain.
	log.Printf("alias route: req=%s all upstreams failed for %q — falling back to strategy chain", req.RequestID, req.Model)
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

// gateUpstreams filters the alias/pool candidate list through the request's
// capability gate (task Q): NG8 exclusion, context fit, tools/vision — the
// same rule the ranked walk applies. Round 9: a tools/vision-only mismatch
// that empties the set is relaxed — the same two-pass rule gateEntries
// applies.
func (s *Server) gateUpstreams(cat *catalog.Catalog, req Request, ups []alias.Upstream) []alias.Upstream {
	d := demandOf(req.Raw, req.Messages, req.MaxTokens)
	keep := func(d capabilityDemand) []alias.Upstream {
		out := make([]alias.Upstream, 0, len(ups))
		for _, u := range ups {
			e := cat.Find(u.ProviderID, u.ModelID)
			if e == nil {
				continue // entry vanished from the catalog mid-flight
			}
			if entryPassesGate(*e, d) {
				out = append(out, u)
			}
		}
		return out
	}
	out := keep(d)
	if len(out) == 0 && (d.tools || d.vision) {
		out = keep(relaxCapabilityDemand(d))
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
		log.Printf("req=%s unknown strategy %q, falling back to adaptive", req.RequestID, stratName)
		strat, _ = s.strategyReg.Get("adaptive")
	}

	// Capability gate BEFORE the queue (task Q): a request no surviving
	// candidate can serve fails immediately with the clear error — it
	// never waits in the slot queue, because waiting cannot grow a context
	// window or add a missing capability.
	demand := demandOf(req.Raw, req.Messages, req.MaxTokens)
	if len(gateEntries(cat.FreeEntries(), demand, cfg.Fallback.ModelBlocklist)) == 0 {
		log.Printf("strategy chain: req=%s no candidate passes the capability gate for %q", req.RequestID, req.ClientModel)
		s.writeNoCapableCandidate(w, req.ClientModel, demand)
		return
	}

	chain := &FallbackChain{
		Cfg:                cfg,
		Strategy:           strat,
		Catalog:            cat,
		RateLimiter:        s.rateLimiter,
		GeminiTracker:      s.geminiTracker,
		ReliabilityTracker: s.reliabilityTracker,
		HTTPClient:         s.upstreamHTTPClient(),
		StreamHTTPClient:   s.streamHTTPClient(),
		Resolver:           s.aliasRes.Load(),
		Slots:              s.slotLimit,
		Queues:             s.queues,
		Demotions:          s.demotions,
		Latencies:          s.latencies,
		Gate:               &demand,
	}

	// Streaming requests run the SAME walk in streaming mode
	// (ExecuteStream): every attempt holds its upstream response until a
	// usable first event, so pre-commit failures retry/fall back silently
	// and the client sees real SSE (gap NG3) instead of a buffered JSON
	// completion served under application/json.
	if req.Stream {
		_, committed, err := chain.ExecuteStream(r.Context(), req, w)
		if err != nil {
			log.Printf("fallback chain exhausted (stream): req=%s %v", req.RequestID, err)
			// Before any header: 503 + Retry-After + OpenAI-style error
			// body (the pre-commit hold guarantees nothing was written).
			// After the keepalive header went out (task P): the structured
			// SSE error event + [DONE] — the status can no longer change.
			s.finishStreamRoute(keepaliveOf(w), w, req.ClientModel, req.RequestID, err)
			return
		}
		_ = committed // a committed stream already answered the client
		return
	}

	resp, err := chain.Execute(r.Context(), req)
	if err != nil {
		log.Printf("fallback chain exhausted: req=%s %v", req.RequestID, err)
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

// handleDebugLatency exposes the slow-TTFT tracker (round 10) for
// operators: every tracked provider/model pair's TTFT EMA, sample count,
// consecutive-slow streak, breaker trips and current demotion state, plus
// the effective thresholds. Model identities and timings only — never
// credentials or prompt content.
func (s *Server) handleDebugLatency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	fb := config.FallbackConfig{}
	if cfg := s.cfg.Load(); cfg != nil {
		fb = cfg.Fallback
	}
	ttl := time.Duration(fb.Unknown400DemotionSeconds) * time.Second
	now := time.Now()
	snap := s.latencies.snapshot()
	entries := make([]map[string]any, 0, len(snap))
	for _, st := range snap {
		provider, model := splitProviderModel(st.Key)
		e := map[string]any{
			"provider":      provider,
			"model":         model,
			"ttft_ema_ms":   st.TTFTemaMS,
			"last_ttft_ms":  st.LastTTFTMS,
			"samples":       st.Samples,
			"slow_streak":   st.SlowStreak,
			"breaker_trips": st.BreakerTrips,
			"deprioritized": s.latencyDeprioritized(st),
			"demoted":       s.demotions != nil && ttl > 0 && s.demotions.isDemoted(provider, model, now, ttl),
			"last_updated":  st.LastUpdated,
		}
		entries = append(entries, e)
	}
	resp := map[string]any{
		"upstreams": entries,
		"config": map[string]any{
			"ttft_slow_threshold_seconds": ttftSlowThresholdFrom(fb).Seconds(),
			"ttft_breaker_trips":          ttftBreakerTripsFrom(fb),
			"ttft_breaker_min_samples":    ttftBreakerMinSamplesFrom(fb),
			// Round 11: how many times the optional last-resort tier of
			// the deadline loop engaged process-wide — the cost counter.
			"last_resort_uses": lastResortUseCount(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// latencyDeprioritized mirrors isSlowCandidate for the debug snapshot:
// the pair currently ranks at the back of every walk.
func (s *Server) latencyDeprioritized(st latencyStats) bool {
	fb := config.FallbackConfig{}
	if cfg := s.cfg.Load(); cfg != nil {
		fb = cfg.Fallback
	}
	if st.Samples < int64(ttftBreakerMinSamplesFrom(fb)) {
		return false
	}
	return st.TTFTemaMS >= float64(ttftSlowThresholdFrom(fb).Milliseconds())
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

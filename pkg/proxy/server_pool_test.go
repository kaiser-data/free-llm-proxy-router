package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// newPoolServer builds a Server in single-model public mode:
// models.public_alias = "kiwi-auto" with raw/canonical exposure disabled.
// All catalog entries belong to one test provider ("unorouter") served by
// the given upstream URL. The strategy registry is nil — pool candidates
// keep catalog order, keeping these tests deterministic.
func newPoolServer(t *testing.T, upstreamURL string, entries []catalog.CatalogEntry) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{
		{ID: "unorouter", BaseURL: upstreamURL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cfg.Models.PublicAlias = "kiwi-auto"
	cfg.Models.ExposeRaw = false
	cfg.Models.ExposeCanonical = false
	return NewServer(cfg, &catalog.Catalog{Entries: entries}, nil, nil, nil, nil)
}

// poolCatalogEntries is the stand-in for a scanned UnoRouter free catalog:
// two free chat models, one paid, one embedding-only.
func poolCatalogEntries() []catalog.CatalogEntry {
	return []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen3:free", IsFree: true, ContextWindow: 40960},
		{ProviderID: "unorouter", ModelID: "deepseek-v3:free", IsFree: true, ContextWindow: 65536},
		{ProviderID: "unorouter", ModelID: "gpt-5", IsFree: false},
		{ProviderID: "unorouter", ModelID: "bge-m3:free", IsFree: true,
			Metadata: map[string]any{"supported_endpoint_types": []any{"embedding"}}},
	}
}

// servePoolDirect drives the production route (handleChatCompletions →
// serveDirectModel → serveAliasModel) for the given model name.
func servePoolDirect(s *Server, model string, stream bool) *httptest.ResponseRecorder {
	req := Request{
		Model:       model,
		ClientModel: model, // production: handleChatCompletions keeps the original name
		Messages:    []map[string]any{{"role": "user", "content": "hi"}},
		Stream:      stream,
	}
	raw := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if stream {
		raw["stream"] = true
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.serveDirectModel(rec, httpReq, s.cfg.Load(), s.catalog.Load(), req, raw)
	return rec
}

// handleModelsData fetches and decodes the /v1/models listing.
func handleModelsData(t *testing.T, s *Server) (ids []string, ownedBy map[string]string, code int) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /v1/models: %v — body: %s", err, rec.Body.String())
	}
	ownedBy = map[string]string{}
	for _, m := range body.Data {
		ids = append(ids, m.ID)
		ownedBy[m.ID] = m.OwnedBy
	}
	return ids, ownedBy, rec.Code
}

// TestPoolAliasResolvesAllFreeCandidates: kiwi-auto resolves to the whole
// eligible free chat pool — paid and non-chat models never enter.
func TestPoolAliasResolvesAllFreeCandidates(t *testing.T) {
	s := newPoolServer(t, "http://unused", poolCatalogEntries())
	ups := s.aliasRes.Load().Upstreams("kiwi-auto")
	if len(ups) != 2 {
		t.Fatalf("Upstreams(kiwi-auto) = %v, want the 2 free chat candidates", ups)
	}
	keys := map[string]bool{}
	for _, u := range ups {
		keys[u.Key()] = true
	}
	if !keys["unorouter/qwen3:free"] || !keys["unorouter/deepseek-v3:free"] {
		t.Errorf("pool missing free chat candidates: %v", keys)
	}
	if keys["unorouter/gpt-5"] {
		t.Error("paid candidate must not enter the pool")
	}
	if keys["unorouter/bge-m3:free"] {
		t.Error("embedding candidate must not enter the pool")
	}
	if !s.aliasRes.Load().IsPool("kiwi-auto") {
		t.Error("expected IsPool(kiwi-auto)")
	}
}

// TestHandleModelsPublicAliasOnly: production mode — /v1/models advertises
// exactly kiwi-auto; no raw model leak, paid models not listed.
func TestHandleModelsPublicAliasOnly(t *testing.T) {
	s := newPoolServer(t, "http://unused", poolCatalogEntries())
	ids, ownedBy, code := handleModelsData(t, s)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(ids) != 1 || ids[0] != "kiwi-auto" {
		t.Fatalf("data = %v, want exactly [kiwi-auto]", ids)
	}
	if ownedBy["kiwi-auto"] != "free-llm-proxy" {
		t.Errorf("owned_by = %q, want free-llm-proxy", ownedBy["kiwi-auto"])
	}
}

// TestHandleModelsPublicAliasOverridesExposeFlags: even with both expose
// flags true, a configured public alias keeps the listing single-model.
func TestHandleModelsPublicAliasOverridesExposeFlags(t *testing.T) {
	s := newPoolServer(t, "http://unused", poolCatalogEntries())
	cfg := *s.cfg.Load()
	cfg.Models.ExposeRaw = true
	cfg.Models.ExposeCanonical = true
	s.cfg.Store(&cfg)

	ids, _, _ := handleModelsData(t, s)
	if len(ids) != 1 || ids[0] != "kiwi-auto" {
		t.Errorf("data = %v, want exactly [kiwi-auto] even with expose flags true", ids)
	}
}

// TestHandleModelsBothFalseFallbackWithoutPool: regression — without a
// public alias, disabling both expose flags still falls back to listing
// both (existing misconfiguration behavior is preserved).
func TestHandleModelsBothFalseFallbackWithoutPool(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cfg.Models.ExposeRaw = false
	cfg.Models.ExposeCanonical = false
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen3:free", IsFree: true},
	}}
	s := NewServer(cfg, cat, nil, nil, nil, nil)

	ids, _, _ := handleModelsData(t, s)
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "qwen3:free") || !strings.Contains(joined, "qwen3") {
		t.Errorf("data = %v, want raw + canonical listing fallback", ids)
	}
	if strings.Contains(joined, "kiwi-auto") {
		t.Errorf("kiwi-auto must not be listed without a configured pool alias: %v", ids)
	}
}

// poolModelStatusHandler: upstream answering per requested model — failModel
// gets failStatus, every other model gets a valid 200 completion recording
// the served model. served collects the models that produced a 200.
func poolModelStatusHandler(failModel string, failStatus int, served *atomic.Value) http.HandlerFunc {
	var hits atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model == failModel {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failStatus)
			fmt.Fprintf(w, `{"error":{"message":"upstream rejected with %d"}}`, failStatus)
			return
		}
		var done []string
		if v, ok := served.Load().([]string); ok {
			done = v
		}
		served.Store(append(done, body.Model))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"ok","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"POOL_OK_%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, body.Model, strings.ToUpper(strings.ReplaceAll(body.Model, "-", "_")))
	}
}

// TestPoolAliasNonStreamFailover: retryable 429 on candidate A → candidate B
// serves; client sees 200 with the router-owned header contract.
func TestPoolAliasNonStreamFailover(t *testing.T) {
	var served atomic.Value
	upstream := httptest.NewServer(poolModelStatusHandler("qwen3:free", http.StatusTooManyRequests, &served))
	defer upstream.Close()
	s := newPoolServer(t, upstream.URL, poolCatalogEntries()[:2])

	rec := servePoolDirect(s, "kiwi-auto", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (retryable 429 must fall through to candidate B); body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Used-Model"); got != "kiwi-auto" {
		t.Errorf("X-Used-Model = %q, want kiwi-auto", got)
	}
	if got := rec.Header().Get("X-Free-Router-Upstream-Provider"); got != "unorouter" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want unorouter", got)
	}
	if got := rec.Header().Get("X-Free-Router-Upstream-Model"); got != "deepseek-v3:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want deepseek-v3:free (candidate B)", got)
	}
	if !strings.Contains(rec.Body.String(), "POOL_OK_DEEPSEEK_V3") {
		t.Errorf("body = %s, want candidate B completion", rec.Body.String())
	}
}

// TestPoolAliasNonRetryableSurfacesImmediately: upstream 400 is surfaced
// verbatim and the remaining pool is NOT walked.
func TestPoolAliasNonRetryableSurfacesImmediately(t *testing.T) {
	var served atomic.Value
	upstream := httptest.NewServer(poolModelStatusHandler("qwen3:free", http.StatusBadRequest, &served))
	defer upstream.Close()
	s := newPoolServer(t, upstream.URL, poolCatalogEntries()[:2])

	rec := servePoolDirect(s, "kiwi-auto", false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (non-retryable must surface as-is)", rec.Code)
	}
	if got := rec.Header().Get("X-Used-Model"); got != "kiwi-auto" {
		t.Errorf("X-Used-Model = %q, want kiwi-auto", got)
	}
	if v, ok := served.Load().([]string); ok && len(v) != 0 {
		t.Errorf("non-retryable must not serve via another candidate, got %v", v)
	}
}

// TestPoolAliasRawStillRoutableNotListed: raw IDs keep working when
// requested explicitly but are not advertised in /v1/models.
func TestPoolAliasRawStillRoutableNotListed(t *testing.T) {
	var served atomic.Value
	upstream := httptest.NewServer(poolModelStatusHandler("", 0, &served))
	defer upstream.Close()
	s := newPoolServer(t, upstream.URL, poolCatalogEntries())

	ids, _, _ := handleModelsData(t, s)
	if len(ids) != 1 || ids[0] != "kiwi-auto" {
		t.Fatalf("data = %v, want exactly [kiwi-auto] (no raw leak)", ids)
	}

	rec := servePoolDirect(s, "unorouter/qwen3:free", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw route status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if v, ok := served.Load().([]string); !ok || len(v) != 1 || v[0] != "qwen3:free" {
		t.Errorf("upstream served = %v, want [qwen3:free] (raw ID reached the provider)", v)
	}
}

// TestPoolAliasDynamicRefreshServer: a catalog hot-reload grows and shrinks
// the live pool without any config change — the dynamic-refresh contract.
func TestPoolAliasDynamicRefreshServer(t *testing.T) {
	s := newPoolServer(t, "http://unused", poolCatalogEntries()[:1])
	if ups := s.aliasRes.Load().Upstreams("kiwi-auto"); len(ups) != 1 {
		t.Fatalf("initial pool = %v, want 1 candidate", ups)
	}

	grown := &catalog.Catalog{Entries: poolCatalogEntries()[:2]}
	s.UpdateCatalog(grown)
	if ups := s.aliasRes.Load().Upstreams("kiwi-auto"); len(ups) != 2 {
		t.Errorf("pool after discovery = %d candidates, want 2", len(ups))
	}

	// Model removed / turned paid → pool shrinks back.
	paid := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen3:free", IsFree: false},
	}}
	s.UpdateCatalog(paid)
	if ups := s.aliasRes.Load().Upstreams("kiwi-auto"); len(ups) != 0 {
		t.Errorf("pool after removal = %d candidates, want 0", len(ups))
	}
}

// TestDebugPoolEndpoint: the operator view lists the real candidate set
// behind the public alias without exposing credentials.
func TestDebugPoolEndpoint(t *testing.T) {
	s := newPoolServer(t, "http://unused", poolCatalogEntries())

	rec := httptest.NewRecorder()
	s.handleDebugPool(rec, httptest.NewRequest(http.MethodGet, "/debug/pool", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Pool       string `json:"pool"`
		Candidates int    `json:"candidates"`
		Upstreams  []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"upstreams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /debug/pool: %v — body: %s", err, rec.Body.String())
	}
	if body.Pool != "kiwi-auto" || body.Candidates != 2 {
		t.Errorf("pool = %q candidates = %d, want kiwi-auto/2", body.Pool, body.Candidates)
	}
	if len(body.Upstreams) != 2 || body.Upstreams[0].Provider != "unorouter" || body.Upstreams[0].Model != "qwen3:free" {
		t.Errorf("upstreams = %+v, want unorouter/qwen3:free + unorouter/deepseek-v3:free", body.Upstreams)
	}
	if bodyStr := rec.Body.String(); strings.Contains(strings.ToLower(bodyStr), "key") || strings.Contains(bodyStr, "Bearer") {
		t.Error("/debug/pool must not expose credentials")
	}

	// No pool configured → 404 with explanation.
	cfg := &config.Config{}
	cfg.Models.Canonicalization.Enabled = true
	empty := NewServer(cfg, &catalog.Catalog{}, nil, nil, nil, nil)
	rec404 := httptest.NewRecorder()
	empty.handleDebugPool(rec404, httptest.NewRequest(http.MethodGet, "/debug/pool", nil))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("status without pool = %d, want 404", rec404.Code)
	}
}

// --- Streaming (real HTTP) ---------------------------------------------------

// poolProxyServer wraps serveDirectModel in a real HTTP server so clients
// observe genuine SSE delivery for pool-alias requests.
func poolProxyServer(s *Server) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		json.NewDecoder(r.Body).Decode(&raw)
		req := Request{Raw: raw}
		if m, ok := raw["model"].(string); ok {
			req.Model = m
		}
		req.ClientModel = req.Model // production parity
		if msgs, ok := raw["messages"].([]any); ok {
			for _, msg := range msgs {
				if m, ok := msg.(map[string]any); ok {
					req.Messages = append(req.Messages, m)
				}
			}
		}
		if st, ok := raw["stream"].(bool); ok {
			req.Stream = st
		}
		s.serveDirectModel(w, r, s.cfg.Load(), s.catalog.Load(), req, raw)
	}))
}

// TestStreamThroughMiddlewareIncremental: the PRODUCTION handler stack
// (logging → recovery → auth, exactly as Start() wires it) must preserve
// per-read SSE flushing. The logging middleware's responseWriter wrapper
// must forward http.Flusher — without it the stream buffers until the
// handler returns and clients lose streaming entirely. Regression for the
// middleware Flush bug found in live E2E.
func TestStreamThroughMiddlewareIncremental(t *testing.T) {
	firstWrite := make(chan struct{})
	proceed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sseEventOne)
		close(firstWrite)
		select {
		case <-proceed:
		case <-time.After(5 * time.Second):
		}
		writeSSE(w, sseEventTwo, sseDone)
	}))
	defer upstream.Close()

	s := newPoolServer(t, upstream.URL, poolCatalogEntries()[:1])
	// handleChatCompletions looks up strategy names in the registry — give it
	// a real one (the chain itself is never reached here: the single
	// candidate answers 200 immediately).
	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{{ID: "unorouter", BaseURL: upstream.URL, Enabled: true}}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cfg.Models.PublicAlias = "kiwi-auto"
	s = NewServer(cfg, &catalog.Catalog{Entries: poolCatalogEntries()[:1]},
		strategy.NewRegistry(nil, nil, "", "", 3, 5), nil, nil, nil)
	// Wrap the mux exactly like Start() does: logging → recovery → auth.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	var handler http.Handler = mux
	handler = loggingMiddleware(handler)
	handler = recoveryMiddleware(handler)
	handler = authMiddleware("", handler)
	proxySrv := httptest.NewServer(handler)
	defer proxySrv.Close()

	type received struct {
		text string
		at   time.Duration
	}
	got := make(chan received, 8)
	go func() {
		conn, err := net.Dial("tcp", proxySrv.Listener.Addr().String())
		if err != nil {
			close(got)
			return
		}
		defer conn.Close()
		body := `{"model":"kiwi-auto","messages":[{"role":"user","content":"hi"}],"stream":true}`
		conn.Write([]byte(fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: t\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)))
		start := time.Now()
		buf := make([]byte, 4096)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				got <- received{string(buf[:n]), time.Since(start)}
			}
			if rerr != nil {
				break
			}
		}
		close(got)
	}()

	var chunks []received
	for c := range got {
		chunks = append(chunks, c)
	}
	var firstEvent time.Duration
	var done time.Duration
	joined := ""
	for _, c := range chunks {
		joined += c.text
		if firstEvent == 0 && strings.Contains(joined, "data: "+sseEventOne) {
			firstEvent = c.at
		}
		if strings.Contains(c.text, "data: [DONE]") {
			done = c.at
		}
	}
	if firstEvent == 0 {
		t.Fatalf("stream did not deliver event one; raw response:\n%s", joined)
	}
	if firstEvent >= done {
		t.Fatalf("event one arrived at %v with DONE at %v — stream was buffered end-to-end (middleware must forward Flush)", firstEvent, done)
	}
}

// TestPoolAliasFallbackChainKeepsPoolIdentity: when the alias walk exhausts
// on retryable failures and the strategy chain serves the request, the
// client still sees X-Used-Model: kiwi-auto (the public identity), while the
// upstream identity headers carry the real provider/model.
func TestPoolAliasFallbackChainKeepsPoolIdentity(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		// 429 the alias walk (one call per candidate), then let the
		// strategy chain's re-walk succeed.
		if calls.Add(1) <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"ok","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"CHAIN_OK"},"finish_reason":"stop"}]}`, body.Model)
	}))
	defer upstream.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{
		{ID: "unorouter", BaseURL: upstream.URL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cfg.Models.PublicAlias = "kiwi-auto"
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen3:free", IsFree: true},
		{ProviderID: "unorouter", ModelID: "deepseek-v3:free", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 3, 5),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	rec := servePoolDirect(s, "kiwi-auto", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via fallback chain; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Used-Model"); got != "kiwi-auto" {
		t.Errorf("X-Used-Model = %q, want kiwi-auto (pool identity must survive the strategy-chain fallback)", got)
	}
	if got := rec.Header().Get("X-Free-Router-Upstream-Model"); got == "" {
		t.Error("X-Free-Router-Upstream-Model must reveal the real upstream")
	}
	if !strings.Contains(rec.Body.String(), "CHAIN_OK") {
		t.Errorf("body = %s, want the chain-served completion", rec.Body.String())
	}
}

// poolStreamPost posts a streaming kiwi-auto request to the proxy server.
func poolStreamPost(t *testing.T, url string) *http.Response {
	t.Helper()
	body := `{"model":"kiwi-auto","messages":[{"role":"user","content":"Write 10 short lines."}],"stream":true}`
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	return resp
}

// TestPoolAliasStreamingTrueSSE: model=kiwi-auto stream=true selects one real
// upstream candidate and pumps its SSE through verbatim.
func TestPoolAliasStreamingTrueSSE(t *testing.T) {
	rec := &streamUpstreamRecord{}
	upstream := httptest.NewServer(sseRecordingHandler(rec, sseEventOne, sseEventTwo, sseDone))
	defer upstream.Close()
	s := newPoolServer(t, upstream.URL, poolCatalogEntries()[:2])
	proxySrv := poolProxyServer(s)
	defer proxySrv.Close()

	resp := poolStreamPost(t, proxySrv.URL)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := resp.Header.Get("X-Used-Model"); got != "kiwi-auto" {
		t.Errorf("X-Used-Model = %q, want kiwi-auto", got)
	}
	if got := resp.Header.Get("X-Free-Router-Upstream-Provider"); got != "unorouter" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want unorouter", got)
	}
	if got := resp.Header.Get("X-Free-Router-Upstream-Model"); got != "qwen3:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want qwen3:free", got)
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	for _, want := range []string{"data: " + sseEventOne, "data: " + sseEventTwo, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q; body:\n%s", want, body)
		}
	}
	if rec.Hits() != 1 || rec.Model() != "qwen3:free" || !rec.Stream() {
		t.Errorf("upstream hits=%d model=%q stream=%v, want 1 hit for qwen3:free with stream=true", rec.Hits(), rec.Model(), rec.Stream())
	}
}

// TestPoolAliasStreamFailoverBeforeFirstByte: candidate A answers 503 (a
// stream attempt that produced no downstream byte) → candidate B's SSE is
// forwarded; exactly one upstream attempt each.
func TestPoolAliasStreamFailoverBeforeFirstByte(t *testing.T) {
	var hitsA atomic.Int32
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "qwen3:free" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hitsA.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"overloaded"}}`)
	})
	upA := httptest.NewServer(failA)
	defer upA.Close()
	upB := httptest.NewServer(sseRecordingHandler(&streamUpstreamRecord{}, sseEventB, sseDone))
	defer upB.Close()

	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{
		{ID: "unorouter", BaseURL: upA.URL, Enabled: true},
		{ID: "unorouter-b", BaseURL: upB.URL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cfg.Models.PublicAlias = "kiwi-auto"
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen3:free", IsFree: true},
		{ProviderID: "unorouter-b", ModelID: "deepseek-v3:free", IsFree: true},
	}}
	s := NewServer(cfg, cat, nil, nil, nil, nil)
	proxySrv := poolProxyServer(s)
	defer proxySrv.Close()

	resp := poolStreamPost(t, proxySrv.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := resp.Header.Get("X-Free-Router-Upstream-Model"); got != "deepseek-v3:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want deepseek-v3:free (candidate B)", got)
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if !strings.Contains(body, "B_ONLY_MARKER") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream body must carry candidate B's events + DONE; body:\n%s", body)
	}
	if hitsA.Load() != 1 {
		t.Errorf("candidate A hits = %d, want 1", hitsA.Load())
	}
}

// --- Strategy ranking + context fit -----------------------------------------

// TestPoolAliasStrategyRanking: the pool walk order follows the configured
// strategy — adaptive ranks the balanced-tier candidate ahead of the
// performance-tier one (context-fit demotion is covered by
// TestPoolAliasLargeContextRequestPrefersFittingCandidate).
func TestPoolAliasStrategyRanking(t *testing.T) {
	stratReg := strategy.NewRegistry(nil, nil, "", "", 3, 5)

	newSrv := func() *Server {
		cfg := &config.Config{}
		cfg.Proxy.Strategy = "adaptive"
		cfg.Providers = []config.ProviderConfig{
			{ID: "unorouter", BaseURL: "http://unused", Enabled: true},
		}
		cfg.Models.Canonicalization.Enabled = true
		cfg.Models.Canonicalization.FreeOnly = true
		cfg.Models.PublicAlias = "kiwi-auto"
		cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
			// Balanced tier — adaptive ranks this first for small requests.
			{ProviderID: "unorouter", ModelID: "qwen3-32b:free", IsFree: true, ContextWindow: 4096},
			// Performance tier — ranked second, but large context.
			{ProviderID: "unorouter", ModelID: "qwen3-235b-a22b:free", IsFree: true, ContextWindow: 32768},
		}}
		s := NewServer(cfg, cat, stratReg, nil, nil, nil)
		return s
	}

	// Small request: the strategy ranking decides — balanced-tier candidate.
	var servedSmall atomic.Value
	upSmall := httptest.NewServer(poolModelStatusHandler("", 0, &servedSmall))
	defer upSmall.Close()
	sSmall := newSrv()
	sSmall.cfg.Load().Providers[0].BaseURL = upSmall.URL
	rec := servePoolDirect(sSmall, "kiwi-auto", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("small request status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if v, _ := servedSmall.Load().([]string); len(v) != 1 || v[0] != "qwen3-32b:free" {
		t.Errorf("small request served = %v, want [qwen3-32b:free] (adaptive ranking)", v)
	}
}

// TestPoolAliasLargeContextRequestPrefersFittingCandidate: request context
// far beyond a small candidate's window never intentionally selects it while
// a larger-context free candidate exists (requirement: 100k request must not
// pick an 8k candidate).
func TestPoolAliasLargeContextRequestPrefersFittingCandidate(t *testing.T) {
	stratReg := strategy.NewRegistry(nil, nil, "", "", 3, 5)
	var served atomic.Value
	upstream := httptest.NewServer(poolModelStatusHandler("", 0, &served))
	defer upstream.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{
		{ID: "unorouter", BaseURL: upstream.URL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cfg.Models.PublicAlias = "kiwi-auto"
	// 8k-context candidate is listed FIRST in the catalog.
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "small-8k:free", IsFree: true, ContextWindow: 8192},
		{ProviderID: "unorouter", ModelID: "big-128k:free", IsFree: true, ContextWindow: 131072},
	}}
	s := NewServer(cfg, cat, stratReg, nil, nil, nil)

	// ~100k-token prompt (400k chars).
	big := strings.Repeat("x", 400_000)
	req := Request{
		Model:    "kiwi-auto",
		Messages: []map[string]any{{"role": "user", "content": big}},
	}
	raw := map[string]any{"model": "kiwi-auto", "messages": []any{map[string]any{"role": "user", "content": big}}}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.serveDirectModel(rec, httpReq, s.cfg.Load(), s.catalog.Load(), req, raw)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if v, _ := served.Load().([]string); len(v) != 1 || v[0] != "big-128k:free" {
		t.Errorf("100k-token request served = %v, want [big-128k:free]", v)
	}
}

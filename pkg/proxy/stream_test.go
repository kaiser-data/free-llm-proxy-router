package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// SSE payloads used by the fake streaming upstreams. Event contents are
// distinct markers so tests can prove exactly which bytes reached the client.
const (
	sseEventOne = `{"id":"c1","object":"chat.completion.chunk","model":"eye2-qwen","choices":[{"index":0,"delta":{"content":"UNO_STREAM_ONE"},"finish_reason":null}]}`
	sseEventTwo = `{"id":"c1","object":"chat.completion.chunk","model":"eye2-qwen","choices":[{"index":0,"delta":{"content":"UNO_STREAM_TWO"},"finish_reason":null}]}`
	sseEventB   = `{"id":"cb","object":"chat.completion.chunk","model":"eye2-qwen","choices":[{"index":0,"delta":{"content":"B_ONLY_MARKER"},"finish_reason":null}]}`
)

// writeSSE emits each payload as an SSE data event (flushed) — the shape a
// real OpenAI-compatible streaming upstream produces. Tests pass sseDone as
// the final event for the terminating "data: [DONE]".
var sseDone = "[DONE]"

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// unoStreamReqRaw builds a canonical-alias client request with stream:true.
func unoStreamReqRaw(model string) (Request, map[string]any) {
	msgs := []any{map[string]any{"role": "user", "content": "Reply exactly: UNO_LIVE_OK"}}
	req := Request{
		Model:    model,
		Messages: []map[string]any{{"role": "user", "content": "Reply exactly: UNO_LIVE_OK"}},
		Stream:   true,
	}
	raw := map[string]any{"model": model, "messages": msgs, "stream": true}
	return req, raw
}

// streamUpstreamRecord captures what the fake streaming upstream received.
type streamUpstreamRecord struct {
	hits   atomic.Int32
	model  atomic.Value // string — requested model
	stream atomic.Value // bool   — stream flag as received
	auth   atomic.Value // string — Authorization header
}

func (u *streamUpstreamRecord) Hits() int32   { return u.hits.Load() }
func (u *streamUpstreamRecord) Model() string { s, _ := u.model.Load().(string); return s }
func (u *streamUpstreamRecord) Stream() bool  { b, _ := u.stream.Load().(bool); return b }
func (u *streamUpstreamRecord) Auth() string  { s, _ := u.auth.Load().(string); return s }

// sseRecordingHandler decodes the request body (model + stream flag) and
// answers with the given SSE events.
func sseRecordingHandler(rec *streamUpstreamRecord, events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.auth.Store(r.Header.Get("Authorization"))
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		rec.model.Store(body.Model)
		rec.stream.Store(body.Stream)
		writeSSE(w, events...)
	}
}

// unoStreamAlias drives serveAliasModel with a streaming canonical "qwen3"
// request through a recorder (production path: handleChatCompletions →
// serveDirectModel → serveAliasModel).
func unoStreamAlias(s *Server) *httptest.ResponseRecorder {
	req, raw := unoStreamReqRaw("qwen3")
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.serveAliasModel(rec, httpReq, s.cfg.Load(), s.catalog.Load(), req, raw, s.aliasRes.Load())
	return rec
}

// --- StreamProxy unit tests -------------------------------------------------

// TestStreamProxyForwardsSSEVerbatim: the upstream's exact SSE bytes —
// including "data: [DONE]" and blank-line event boundaries — must reach the
// client unmodified, with the SSE transport headers and the router-owned
// contract headers applied.
func TestStreamProxyForwardsSSEVerbatim(t *testing.T) {
	srv := httptest.NewServer(sseRecordingHandler(&streamUpstreamRecord{}, sseEventOne, sseEventTwo, sseDone))
	defer srv.Close()

	rec := httptest.NewRecorder()
	sp := &StreamProxy{HTTPClient: srv.Client()}
	cfg := config.ProviderConfig{ID: "unorouter", BaseURL: srv.URL, APIKey: "k1", Enabled: true}
	body := map[string]any{"model": "qwen3:free", "messages": []any{}}

	if err := sp.Forward(context.Background(), rec, cfg, body, "qwen3"); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("X-Used-Model"); got != "qwen3" {
		t.Errorf("X-Used-Model = %q, want qwen3", got)
	}
	if got := rec.Header().Get("X-Free-Router-Upstream-Provider"); got != "unorouter" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want unorouter", got)
	}
	if got := rec.Header().Get("X-Free-Router-Upstream-Model"); got != "qwen3:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want qwen3:free", got)
	}
	want := "data: " + sseEventOne + "\n\ndata: " + sseEventTwo + "\n\ndata: [DONE]\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body bytes differ:\n got  %q\n want %q", got, want)
	}
	if !rec.Flushed {
		t.Error("response was never flushed — client would not see incremental events")
	}
}

// TestStreamProxyNon200TypedError: a non-200 upstream answer must come back
// as *UpstreamStreamError with nothing written to the client, so the caller
// can classify and either fall back or surface the error.
func TestStreamProxyNon200TypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	sp := &StreamProxy{HTTPClient: srv.Client()}
	cfg := config.ProviderConfig{ID: "unorouter", BaseURL: srv.URL, Enabled: true}
	err := sp.Forward(context.Background(), rec, cfg, map[string]any{"model": "m"}, "qwen3")

	var use *UpstreamStreamError
	if !errors.As(err, &use) {
		t.Fatalf("err = %v, want *UpstreamStreamError", err)
	}
	if use.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", use.StatusCode)
	}
	if !strings.Contains(string(use.Body), "rate limited") {
		t.Errorf("Body = %q, want the upstream error body", use.Body)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Error("client response was touched on a non-200 upstream answer")
	}
}

// TestStreamProxyJSON200Passthrough: an upstream that answers 200 with JSON
// (it ignored stream=true) is forwarded as application/json — never
// disguised as an event stream.
func TestStreamProxyJSON200Passthrough(t *testing.T) {
	const upstreamBody = `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, upstreamBody)
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	sp := &StreamProxy{HTTPClient: srv.Client()}
	cfg := config.ProviderConfig{ID: "unorouter", BaseURL: srv.URL, Enabled: true}
	if err := sp.Forward(context.Background(), rec, cfg, map[string]any{"model": "m", "stream": true}, "qwen3"); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (JSON must not be disguised as SSE)", got)
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("body = %q, want verbatim upstream body", got)
	}
	if got := rec.Header().Get("X-Free-Router-Upstream-Provider"); got != "unorouter" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want unorouter", got)
	}
}

// newUnoTwoProviderServer builds a Server with two providers serving the same
// raw model ID (separate candidates under canonical "qwen3").
func newUnoTwoProviderServer(firstURL, secondURL string) *Server {
	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{
		{ID: "unorouter-a", BaseURL: firstURL, APIKey: "uno-secret-key", Enabled: true},
		{ID: "unorouter-b", BaseURL: secondURL, APIKey: "uno-secret-key", Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "unorouter-a", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true, TierType: "free"},
		{ProviderID: "unorouter-b", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true, TierType: "free"},
	}}
	return NewServer(cfg, cat, nil, nil, nil, nil)
}

// --- Alias-route streaming tests ---------------------------------------------

// TestStreamAliasRouteHeadersRawModelAndStreamFlag: a streaming request for
// canonical "qwen3" must reach UnoRouter as the raw free model ID with
// stream:true INTACT (not stripped like the buffered path), and the client
// must receive the SSE content type, the contract headers, and [DONE].
func TestStreamAliasRouteHeadersRawModelAndStreamFlag(t *testing.T) {
	rec := &streamUpstreamRecord{}
	srv := httptest.NewServer(sseRecordingHandler(rec, sseEventOne, sseEventTwo, sseDone))
	defer srv.Close()
	s := newUnoAliasServer(t, srv.URL, "uno-secret-key")

	resp := unoStreamAlias(s)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.Code, resp.Body.String())
	}
	if got := rec.Model(); got != "qwen/qwen3-235b-a22b:free" {
		t.Errorf("upstream model = %q, want the raw free ID", got)
	}
	if !rec.Stream() {
		t.Error("upstream request body had stream != true — the flag must not be stripped on the streaming path")
	}
	if got := rec.Auth(); got != "Bearer uno-secret-key" {
		t.Errorf("upstream Authorization = %q, want Bearer uno-secret-key", got)
	}
	if got := resp.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header().Get("X-Used-Model"); got != "qwen3" {
		t.Errorf("X-Used-Model = %q, want canonical qwen3", got)
	}
	if got := resp.Header().Get("X-Free-Router-Upstream-Provider"); got != "unorouter" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want unorouter", got)
	}
	if got := resp.Header().Get("X-Free-Router-Upstream-Model"); got != "qwen/qwen3-235b-a22b:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want the raw free ID", got)
	}
	body := resp.Body.String()
	for _, want := range []string{sseEventOne, sseEventTwo, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q (body: %q)", want, body)
		}
	}
	if !resp.Flushed {
		t.Error("response was never flushed")
	}
}

// TestStreamFailoverBeforeFirstByte: a retryable 503 from the first
// streaming candidate walks to the second candidate, which streams the SSE
// response — fallback before the first downstream byte.
func TestStreamFailoverBeforeFirstByte(t *testing.T) {
	rec := &streamUpstreamRecord{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec.hits.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"upstream rejected with 503"}}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		rec.model.Store(body.Model)
		writeSSE(w, sseEventB, sseDone)
	}))
	defer srv.Close()
	s := newUnoAliasServer(t, srv.URL, "uno-secret-key")

	resp := unoStreamAlias(s)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover (body: %s)", resp.Code, resp.Body.String())
	}
	if got := rec.Hits(); got != 2 {
		t.Errorf("upstream hits = %d, want 2", got)
	}
	if !strings.Contains(resp.Body.String(), "B_ONLY_MARKER") {
		t.Errorf("body %q missing second-candidate SSE event", resp.Body.String())
	}
	if got := resp.Header().Get("X-Free-Router-Upstream-Model"); got != "qwen/qwen3-30b-a3b:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want the second candidate's raw ID", got)
	}
}

// TestStreamNonRetryableSurfaces: a 400 from the first streaming candidate is
// non-retryable — surfaced verbatim, no further candidates, no masked success.
func TestStreamNonRetryableSurfaces(t *testing.T) {
	rec := &streamUpstreamRecord{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec.hits.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"upstream rejected with 400"}}`)
			return
		}
		writeSSE(w, sseEventB, sseDone)
	}))
	defer srv.Close()
	s := newUnoAliasServer(t, srv.URL, "uno-secret-key")

	resp := unoStreamAlias(s)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 surfaced as-is", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "upstream rejected with 400") {
		t.Errorf("body %q missing the verbatim upstream error", resp.Body.String())
	}
	if got := rec.Hits(); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (non-retryable must not walk further candidates)", got)
	}
	if got := resp.Header().Get("X-Used-Model"); got != "qwen3" {
		t.Errorf("X-Used-Model = %q, want qwen3", got)
	}
}

// --- Real-HTTP streaming tests (recorder cannot prove incrementality) -------

// unoStreamProxyServer wraps s.serveAliasModel in a real HTTP server so the
// client observes genuine incremental SSE delivery.
func unoStreamProxyServer(s *Server) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, raw := unoStreamReqRaw("qwen3")
		s.serveAliasModel(w, r, s.cfg.Load(), s.catalog.Load(), req, raw, s.aliasRes.Load())
	}))
}

// TestStreamAliasRouteTrueSSE: the client receives event one BEFORE the
// upstream has finished generating. The upstream blocks after its first
// event until the client confirms receipt — impossible unless the proxy
// forwards SSE incrementally instead of buffering until EOF.
func TestStreamAliasRouteTrueSSE(t *testing.T) {
	firstWrite := make(chan struct{})
	proceed := make(chan struct{})
	var upstreamTimedOut atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sseEventOne)
		close(firstWrite)
		select {
		case <-proceed:
		case <-time.After(5 * time.Second):
			upstreamTimedOut.Store(true)
		}
		writeSSE(w, sseEventTwo, sseDone)
	}))
	defer upstream.Close()
	s := newUnoAliasServer(t, upstream.URL, "uno-secret-key")
	proxySrv := unoStreamProxyServer(s)
	defer proxySrv.Close()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxySrv.URL, nil)
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	firstAt := time.Time{}
	var lines []string
	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		if line != "" {
			if strings.HasPrefix(line, "data:") && firstAt.IsZero() {
				firstAt = time.Now()
				close(proceed) // upstream may finish now — event one already flowed through
			}
			lines = append(lines, line)
		}
		if rerr != nil {
			break
		}
	}
	if upstreamTimedOut.Load() {
		t.Fatal("upstream never saw client confirmation — the proxy buffered the stream until EOF")
	}
	firstLatency := firstAt.Sub(start)
	if firstAt.IsZero() {
		t.Fatal("client never received an SSE event")
	}
	if firstLatency > 2*time.Second {
		t.Errorf("first event arrived after %v — proxy buffered instead of streaming", firstLatency)
	}
	joined := strings.Join(lines, "")
	for _, want := range []string{sseEventOne, sseEventTwo, "data: [DONE]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("client stream missing %q", want)
		}
	}
}

// TestStreamNoFallbackAfterStreamStart: candidate A starts the SSE stream,
// then its connection breaks mid-generation. The proxy must NOT start
// candidate B — that would concatenate two unrelated generations. The client
// keeps A's partial stream, then gets the OpenAI-style SSE error event and
// the [DONE] terminator (the pre-commit hold guarantees this failure happened
// AFTER the commit — post-commit is terminal). B is never contacted.
func TestStreamNoFallbackAfterStreamStart(t *testing.T) {
	hitA := &streamUpstreamRecord{}
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitA.hits.Add(1)
		writeSSE(w, sseEventOne) // stream started, data reached the client
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close() // abrupt mid-stream break
			}
		}
	}))
	defer upA.Close()
	hitB := &streamUpstreamRecord{}
	upB := httptest.NewServer(sseRecordingHandler(hitB, sseEventB, sseDone))
	defer upB.Close()
	s := newUnoTwoProviderServer(upA.URL, upB.URL)
	proxySrv := unoStreamProxyServer(s)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "UNO_STREAM_ONE") {
		t.Errorf("client lost candidate A's streamed event; body: %q", body)
	}
	if strings.Contains(string(body), "B_ONLY_MARKER") {
		t.Error("client received candidate B's generation — fallback after stream start is forbidden")
	}
	if !strings.Contains(string(body), "upstream_stream_interrupted") || !strings.Contains(string(body), "data: [DONE]") {
		t.Errorf("interrupted stream must end with the SSE error event + [DONE]; body: %q", body)
	}
	if got := hitB.Hits(); got != 0 {
		t.Errorf("candidate B hits = %d, want 0 (no candidate may start after the stream began)", got)
	}
}

// TestStreamClientCancelCancelsUpstream: when the downstream client
// disconnects mid-stream, the upstream request context is canceled — no
// inference keeps running for a gone client.
func TestStreamClientCancelCancelsUpstream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sseEventOne)
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	s := newUnoAliasServer(t, upstream.URL, "uno-secret-key")
	proxySrv := unoStreamProxyServer(s)
	defer proxySrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxySrv.URL, nil)
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	// Read the first event, then walk away like a client that got what it
	// needed (or crashed).
	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			break
		}
		if rerr != nil {
			t.Fatalf("stream ended before the first event: %v", rerr)
		}
	}
	cancel()

	select {
	case <-upstreamCanceled:
		// upstream request context canceled — connection released
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not canceled after the client disconnected")
	}
}

// --- Direct-route streaming (raw models) -------------------------------------

// TestStreamDirectRouteRawModel: raw model IDs stream too — the direct route
// forwards stream:true, the raw ID reaches upstream, SSE comes back, and
// (matching the buffered direct route) no X-Used-Model is reported.
func TestStreamDirectRouteRawModel(t *testing.T) {
	rec := &streamUpstreamRecord{}
	srv := httptest.NewServer(sseRecordingHandler(rec, sseEventOne, sseDone))
	defer srv.Close()
	s := newUnoAliasServer(t, srv.URL, "uno-secret-key")

	req, raw := unoStreamReqRaw("qwen/qwen3-235b-a22b:free")
	resp := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.serveDirectModel(resp, httpReq, s.cfg.Load(), s.catalog.Load(), req, raw)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.Code, resp.Body.String())
	}
	if got := rec.Model(); got != "qwen/qwen3-235b-a22b:free" {
		t.Errorf("upstream model = %q, want the requested raw ID", got)
	}
	if !rec.Stream() {
		t.Error("upstream request body had stream != true")
	}
	if got := resp.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header().Get("X-Free-Router-Upstream-Model"); got != "qwen/qwen3-235b-a22b:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want the raw ID", got)
	}
	if got := resp.Header().Get("X-Used-Model"); got != "" {
		t.Errorf("X-Used-Model = %q, want empty on the direct route (buffered-path parity)", got)
	}
	if !strings.Contains(resp.Body.String(), "data: [DONE]") {
		t.Errorf("body %q missing the [DONE] terminator", resp.Body.String())
	}
}

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

const (
	sseRole    = `data: {"model":"m","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
	sseHello   = `data: {"model":"m","choices":[{"delta":{"content":"hello"}}]}` + "\n\n"
	sseWorld   = `data: {"model":"m","choices":[{"delta":{"content":" world"},"finish_reason":"stop"}]}` + "\n\n"
	sseDone    = "data: [DONE]\n\n"
	sseErrEvnt = `data: {"error":{"message":"upstream overloaded","code":502}}` + "\n\n"
)

func sseHandler(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	}
}

// twoProviderChain routes "alpha/m" first, then "beta/m".
func twoProviderChain(alpha, beta string) *FallbackChain {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "alpha", BaseURL: alpha, Enabled: true},
			{ID: "beta", BaseURL: beta, Enabled: true},
		},
		Fallback: config.FallbackConfig{MaxAttempts: 5},
	}
	return &FallbackChain{
		Cfg: cfg,
		Strategy: listStrategy{
			{ProviderID: "alpha", ModelID: "m"},
			{ProviderID: "beta", ModelID: "m"},
		},
		Catalog:            &catalog.Catalog{},
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
		Stream:             true,
	}
}

func streamReq() Request {
	return Request{
		Stream:   true,
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Raw: map[string]any{
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
		},
	}
}

// executeToClient runs the chain and writes what the client would receive.
func executeToClient(t *testing.T, fc *FallbackChain) (*httptest.ResponseRecorder, *Response) {
	t.Helper()
	resp, err := fc.Execute(context.Background(), streamReq())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rec := httptest.NewRecorder()
	if resp.stream == nil {
		t.Fatalf("expected a committed stream, got status %d body %s", resp.StatusCode, resp.Body)
	}
	writeUpstreamStream(rec, resp)
	return rec, resp
}

func TestStream_PassesThroughProviderSSE(t *testing.T) {
	var sent map[string]any
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sent)
		sseHandler(sseRole, sseHello, sseWorld, sseDone)(w, r)
	}))
	defer alpha.Close()

	rec, resp := executeToClient(t, twoProviderChain(alpha.URL, "http://127.0.0.1:1"))
	if want := sseRole + sseHello + sseWorld + sseDone; rec.Body.String() != want {
		t.Errorf("client got %q, want the provider stream byte for byte", rec.Body.String())
	}
	if resp.UsedModel != "m" || rec.Header().Get("X-Used-Model") != "m" {
		t.Errorf("X-Used-Model = %q", rec.Header().Get("X-Used-Model"))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if sent["stream"] != true || sent["stream_options"] == nil {
		t.Errorf("provider should get stream and stream_options, got %v", sent)
	}
}

func TestStream_FallsBackBeforeFirstContent(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"429": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limited"}}`)
		},
		"error event in a 200 stream": sseHandler(sseRole, sseErrEvnt),
		"stream with no content":      sseHandler(sseRole, sseDone),
		"connection drops":            sseHandler(sseRole),
	}
	for name, alphaHandler := range cases {
		t.Run(name, func(t *testing.T) {
			alpha := httptest.NewServer(alphaHandler)
			defer alpha.Close()
			beta := httptest.NewServer(sseHandler(sseHello, sseDone))
			defer beta.Close()

			rec, _ := executeToClient(t, twoProviderChain(alpha.URL, beta.URL))
			if rec.Body.String() != sseHello+sseDone {
				t.Errorf("client got %q, want beta's stream only", rec.Body.String())
			}
		})
	}
}

// A provider that ignores stream:true and answers JSON still reaches the
// client as SSE through the buffered path.
func TestStream_JSONAnswerIsBufferedPath(t *testing.T) {
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer alpha.Close()
	resp, err := twoProviderChain(alpha.URL, "http://127.0.0.1:1").Execute(context.Background(), streamReq())
	if err != nil || resp.stream != nil || !strings.Contains(string(resp.Body), `"hi"`) {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestBuildBody_StreamOptionsOnlyWithStream(t *testing.T) {
	fc := twoProviderChain("http://a", "http://b")
	raw := streamReq().Raw

	fc.Stream = false
	if b := fc.buildBody("alpha", raw, "m"); b["stream"] != nil || b["stream_options"] != nil {
		t.Errorf("buffered body must drop stream and stream_options: %v", b)
	}
	fc.Stream = true
	if b := fc.buildBody("gemini", raw, "m"); b["stream"] != nil || b["stream_options"] != nil {
		t.Errorf("gemini stays buffered: %v", b)
	}
}

// End to end through the real middleware stack: the first chunk reaches the
// client while the provider is still generating.
func TestStream_FirstChunkArrivesBeforeProviderFinishes(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseHello)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, sseWorld+sseDone)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "alpha", BaseURL: upstream.URL, Enabled: true}},
		Fallback:  config.FallbackConfig{MaxAttempts: 3},
	}
	cfg.Proxy.Strategy = "adaptive"
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{{ProviderID: "alpha", ModelID: "m", IsFree: true}}}
	rl, rel := ratelimit.NewGlobalTracker(), reliability.New()
	s, err := NewServer(cfg, cat, strategy.NewRegistry(rel, rl, "", "", 3, 5, strategy.LongContextOptions{}), rl, nil, rel)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(loggingMiddleware(http.HandlerFunc(s.handleChatCompletions)))
	defer proxy.Close()
	defer close(release) // runs first, so the handlers can finish before Close

	// Without flushing, even the response headers are held back, so the
	// request itself runs inside the timed goroutine.
	got := make(chan string, 1)
	go func() {
		resp, err := http.Post(proxy.URL, "application/json",
			strings.NewReader(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			got <- err.Error()
			return
		}
		defer resp.Body.Close()
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, "hello") {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk was held back until the provider finished")
	}
}

// singleProviderServer is a Server with one free model "alpha/m".
func singleProviderServer(upstreamURL string, mutate func(*config.Config)) *Server {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "alpha", BaseURL: upstreamURL, Enabled: true}},
		Fallback:  config.FallbackConfig{MaxAttempts: 3},
	}
	cfg.Proxy.Strategy = "adaptive"
	if mutate != nil {
		mutate(cfg)
	}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{{ProviderID: "alpha", ModelID: "m", IsFree: true}}}
	rl, rel := ratelimit.NewGlobalTracker(), reliability.New()
	s, err := NewServer(cfg, cat, strategy.NewRegistry(rel, rl, "", "", 3, 5, strategy.LongContextOptions{}), rl, nil, rel)
	if err != nil {
		panic(err)
	}
	return s
}

const streamBody = `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// The per-provider timeout guards against a provider that never answers.
// It must not cut a stream that is already flowing: a long answer from a
// slow free model can take longer than the provider timeout.
func TestStream_ProviderTimeoutDoesNotCutAStartedStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseHello)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // longer than the provider timeout below
		io.WriteString(w, sseWorld+sseDone)
	}))
	defer upstream.Close()

	s := singleProviderServer(upstream.URL, func(c *config.Config) { c.Providers[0].Timeout = "150ms" })
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if got := rec.Body.String(); got != sseHello+sseWorld+sseDone {
		t.Errorf("stream was cut by the provider timeout: %q", got)
	}
}

// The provider timeout still applies until the first content chunk.
func TestStream_ProviderTimeoutAppliesBeforeFirstContent(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseRole)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	fast := httptest.NewServer(sseHandler(sseHello, sseDone))
	defer fast.Close()

	fc := twoProviderChain(slow.URL, fast.URL)
	fc.Cfg.Providers[0].Timeout = "150ms"
	start := time.Now()
	rec, _ := executeToClient(t, fc)
	if rec.Body.String() != sseHello+sseDone {
		t.Errorf("client got %q, want the fast provider", rec.Body.String())
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("fallback took %v; the provider timeout did not apply before the first chunk", el)
	}
}

// CRLF line endings and SSE comment lines (OpenRouter sends
// ": OPENROUTER PROCESSING") are replayed byte for byte.
func TestStream_CRLFAndCommentsReplayedVerbatim(t *testing.T) {
	events := ": OPENROUTER PROCESSING\r\n\r\n" +
		`data: {"model":"m","choices":[{"delta":{"role":"assistant"}}]}` + "\r\n\r\n" +
		`data: {"model":"m","choices":[{"delta":{"content":"hi"}}]}` + "\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	alpha := httptest.NewServer(sseHandler(events))
	defer alpha.Close()
	rec, _ := executeToClient(t, twoProviderChain(alpha.URL, "http://127.0.0.1:1"))
	if rec.Body.String() != events {
		t.Errorf("client got %q, want %q", rec.Body.String(), events)
	}
}

// A provider that breaks after the first content chunk: the client keeps the
// part it has, the stream ends, nothing hangs.
func TestStream_UpstreamBreaksAfterCommit(t *testing.T) {
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseHello)
		w.(http.Flusher).Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer alpha.Close()
	done := make(chan string, 1)
	go func() {
		rec, _ := executeToClient(t, twoProviderChain(alpha.URL, "http://127.0.0.1:1"))
		done <- rec.Body.String()
	}()
	select {
	case got := <-done:
		if !strings.HasPrefix(got, sseHello) || !strings.Contains(got, `"stream_interrupted"`) {
			t.Errorf("client got %q, want the first chunk then an error event", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a broken upstream stream hung the handler")
	}
}

// A client that goes away mid-stream must cancel the upstream call, or a
// small board keeps paying for a generation nobody reads.
func TestStream_ClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamGone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseHello)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				close(upstreamGone)
				return
			case <-time.After(20 * time.Millisecond):
				if _, err := io.WriteString(w, sseWorld); err != nil {
					close(upstreamGone)
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(loggingMiddleware(http.HandlerFunc(singleProviderServer(upstream.URL, nil).handleChatCompletions)))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL, "application/json", strings.NewReader(streamBody))
	if err != nil {
		t.Fatal(err)
	}
	bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close() // client hangs up

	select {
	case <-upstreamGone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream call kept running after the client disconnected")
	}
}

// A stream request that every provider rejects gets the provider's 400 as
// JSON, not an SSE stream and not a 503.
func TestStream_AllRejectedReturnsJSON400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"maximum context length exceeded"}}`)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	singleProviderServer(upstream.URL, nil).handleChatCompletions(rec,
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "context length") {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// A reasoning model that thinks for longer than maxStreamPrefix is committed
// anyway, so the buffer stays bounded on a small board.
func TestPeekStream_PrefixIsBounded(t *testing.T) {
	var in bytes.Buffer
	chunk := `data: {"model":"m","choices":[{"delta":{"reasoning":"` + strings.Repeat("x", 4000) + `"}}]}` + "\n\n"
	for in.Len() < maxStreamPrefix+len(chunk) {
		in.WriteString(chunk)
	}
	in.WriteString(sseHello + sseDone)
	want := in.String()
	resp, err := peekStream(200, http.Header{}, &in)
	if err != nil || resp.stream == nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(resp.stream.prefix) > maxStreamPrefix+len(chunk) {
		t.Errorf("prefix grew to %d bytes", len(resp.stream.prefix))
	}
	rest, _ := io.ReadAll(resp.stream.rest)
	if string(resp.stream.prefix)+string(rest) != want {
		t.Error("prefix + rest must equal the upstream stream")
	}
}

// Whatever the upstream sends, peekStream must not panic, and a committed
// stream must replay exactly what the provider sent.
func FuzzPeekStream(f *testing.F) {
	for _, seed := range []string{
		sseRole + sseHello + sseDone,
		sseErrEvnt,
		"data: [DONE]\n\n",
		": comment\r\n\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\r\n\r\n",
		"data: {not json}\n\ndata: {\"choices\":[{\"text\":\"t\"}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"a\"}]}}]}\n",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		resp, err := peekStream(200, http.Header{}, bytes.NewReader(in))
		if err != nil || resp.stream == nil {
			return
		}
		rest, _ := io.ReadAll(resp.stream.rest)
		if got := append(append([]byte(nil), resp.stream.prefix...), rest...); !bytes.Equal(got, in) {
			t.Errorf("replay mismatch:\n got %q\nwant %q", got, in)
		}
	})
}

// A committed stream cut by the request deadline must end with an error
// event, not a clean end that an SDK would take for a complete answer.
func TestStream_DeadlineCutEndsWithErrorEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseHello)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	s := singleProviderServer(upstream.URL, func(c *config.Config) { c.Fallback.RequestTimeout = 200 * time.Millisecond })
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	got := rec.Body.String()
	if !strings.HasPrefix(got, sseHello) || !strings.Contains(got, `"stream_interrupted"`) {
		t.Errorf("client got %q, want the first chunk then an error event", got)
	}
}

// A stream that ends without content and without [DONE] is an empty answer:
// fall back, but do not count it against the provider like a broken link.
func TestStream_EOFWithoutContentIsEmptyNotTransportError(t *testing.T) {
	alpha := httptest.NewServer(sseHandler(sseRole))
	defer alpha.Close()
	beta := httptest.NewServer(sseHandler(sseHello, sseDone))
	defer beta.Close()

	fc := twoProviderChain(alpha.URL, beta.URL)
	rec, _ := executeToClient(t, fc)
	if rec.Body.String() != sseHello+sseDone {
		t.Errorf("client got %q", rec.Body.String())
	}
	if rate := fc.ReliabilityTracker.SuccessRate("alpha"); rate < 1 {
		t.Errorf("empty stream lowered alpha's reliability to %v", rate)
	}
}

// A single line without a newline cannot grow memory without bound.
func TestPeekStream_HugeLineIsBounded(t *testing.T) {
	in := "data: " + strings.Repeat("x", 3*maxStreamPrefix)
	resp, err := peekStream(200, http.Header{}, strings.NewReader(in))
	if err != nil || resp.stream == nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(resp.stream.prefix) > maxStreamPrefix+streamReadBuffer {
		t.Errorf("buffered %d bytes of one line", len(resp.stream.prefix))
	}
	rest, _ := io.ReadAll(resp.stream.rest)
	if string(resp.stream.prefix)+string(rest) != in {
		t.Error("prefix + rest must equal the upstream stream")
	}
}

// SSE allows one event to span several data: lines; they are joined.
func TestStream_MultiLineDataEventCommits(t *testing.T) {
	up := httptest.NewServer(sseHandler(
		"data: {\"choices\":[{\"delta\":\n",
		"data: {\"content\":\"hi\"}}]}\n\n",
		sseDone,
	))
	defer up.Close()
	fc := twoProviderChain(up.URL, up.URL)
	resp, err := fc.Execute(context.Background(), streamReq())
	if err != nil || resp.stream == nil {
		t.Fatalf("a multi-line data event with content should commit: resp=%v err=%v", resp, err)
	}
	resp.Close()
}

// An error event inside a 200 stream ({"error":{"code":400}}, OpenRouter)
// is classified like a plain 400: the client gets it, not a 503 to retry.
func TestStream_ErrorEventCodeIsTheStatus(t *testing.T) {
	up := httptest.NewServer(sseHandler(
		": OPENROUTER PROCESSING\n\n",
		`data: {"error":{"code":400,"message":"This endpoint's maximum context length is 8192 tokens"}}`+"\n\n",
	))
	defer up.Close()
	fc, _, _ := newTestChain(up.URL, "p", "m")
	fc.Stream = true
	resp, err := fc.Execute(context.Background(), streamReq())
	if err != nil {
		t.Errorf("in-stream 400 error -> exhausted 503 (client retries forever): %v", err)
	} else {
		t.Logf("status=%d", resp.StatusCode)
	}
}

// Whitespace-only content is an empty answer, as on the buffered path:
// it must not commit the stream.
func TestStream_WhitespaceOnlyContentFallsBack(t *testing.T) {
	ws := httptest.NewServer(sseHandler(sseRole, `data: {"choices":[{"delta":{"content":"\n\n"}}]}`+"\n\n", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n", sseDone))
	defer ws.Close()
	good := httptest.NewServer(sseHandler(sseRole, sseHello, sseWorld, sseDone))
	defer good.Close()
	fc := twoProviderChain(ws.URL, good.URL)
	resp, err := fc.Execute(context.Background(), streamReq())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Close()
	if resp.stream != nil {
		b, _ := io.ReadAll(resp.stream.rest)
		if !bytes.Contains(append(resp.stream.prefix, b...), []byte("hello")) {
			t.Errorf("whitespace-only stream committed; client gets empty answer, no fallback")
		}
	}
}

// A provider that closes its connection cleanly after content but before it
// finished (no [DONE], no finish_reason) has cut the answer short. A plain
// end would look complete to an SDK, so the client gets an error event.
func TestStream_CleanEOFBeforeFinishEndsWithErrorEvent(t *testing.T) {
	alpha := httptest.NewServer(sseHandler(sseRole, sseHello))
	defer alpha.Close()
	rec, _ := executeToClient(t, twoProviderChain(alpha.URL, "http://127.0.0.1:1"))
	got := rec.Body.String()
	if !strings.HasPrefix(got, sseRole+sseHello) || !strings.Contains(got, `"stream_interrupted"`) {
		t.Errorf("client got %q, want the partial answer then an error event", got)
	}
}

// Some providers end with a finish_reason and skip [DONE]: that answer is
// complete and must pass through untouched.
func TestStream_FinishReasonWithoutDoneIsComplete(t *testing.T) {
	alpha := httptest.NewServer(sseHandler(sseHello, sseWorld))
	defer alpha.Close()
	rec, _ := executeToClient(t, twoProviderChain(alpha.URL, "http://127.0.0.1:1"))
	if got := rec.Body.String(); got != sseHello+sseWorld {
		t.Errorf("client got %q, want the stream unchanged", got)
	}
}

func TestStreamEnd(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		want   bool
	}{
		{"done", sseHello + sseDone, true},
		{"done with CRLF", sseHello + "data: [DONE]\r\n\r\n", true},
		{"finish_reason, no done", sseHello + sseWorld, true},
		{"finish_reason with a space", sseHello + `data: {"choices":[{"finish_reason": "length"}]}` + "\n\n", true},
		{"finish_reason null only", sseHello + `data: {"choices":[{"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n", false},
		{"cut mid-answer", sseRole + sseHello, false},
		{"done not at the end", sseHello + sseDone + sseHello, false}, // only a trailing [DONE] counts
	}
	for _, c := range cases {
		for _, size := range []int{1, 3, 7, 1 << 20} { // split every way, then whole
			t.Run(fmt.Sprintf("%s/%d", c.name, size), func(t *testing.T) {
				var e streamEnd
				for p := c.stream; len(p) > 0; {
					n := min(size, len(p))
					e.observe([]byte(p[:n]))
					p = p[n:]
				}
				if got := e.complete(); got != c.want {
					t.Errorf("complete() = %v, want %v", got, c.want)
				}
			})
		}
	}
}

package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// writeHeaderCounter wraps a ResponseRecorder and counts WriteHeader calls —
// the commit-once guard (a second WriteHeader would be a bug).
type writeHeaderCounter struct {
	*httptest.ResponseRecorder
	calls int32
}

func (c *writeHeaderCounter) WriteHeader(code int) {
	atomic.AddInt32(&c.calls, 1)
	c.ResponseRecorder.WriteHeader(code)
}

// streamBehavior scripts one upstream SSE handler.
type streamBehavior func(w http.ResponseWriter, r *http.Request)

// sseServer builds an upstream speaking SSE per the given script.
func sseServer(hits *int32, behavior streamBehavior) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		behavior(w, r)
	}))
}

// holdForeverSSE answers 200 text/event-stream (headers flushed) and then
// never writes a byte — the proxy's TTFT watchdog must abort the attempt;
// the handler exits when the aborted request's connection closes.
func holdForeverSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
}

// errorFirstEventSSE answers 200 SSE whose first event is an error object.
func errorFirstEventSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"error\":{\"message\":\"provider overloaded\"}}\n\n")
}

// doneFirstEventSSE answers 200 SSE whose first event is [DONE] — an empty
// stream.
func doneFirstEventSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// goodStreamSSE writes a usable two-event stream ending in [DONE].
func goodStreamSSE(marker string) streamBehavior {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"id\":\"1\",\"model\":\"m-%s\",\"choices\":[{\"delta\":{\"content\":\"hello from %s\"}}]}\n\n", marker, marker)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-"+marker+"\",\"choices\":[{}]}\n\ndata: [DONE]\n\n")
	}
}

// roleOnlyChunk is the opening chunk several providers send: a complete
// OpenAI-style event with only a role in the delta — no content, no tool
// calls, no finish reason (task J).
const roleOnlyChunk = "data: {\"id\":\"1\",\"model\":\"m-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"

// roleOnlyThenContentSSE opens with the role-only chunk and then streams
// real content — the shape that must commit normally (task J).
func roleOnlyThenContentSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, roleOnlyChunk)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-x\",\"choices\":[{\"delta\":{\"content\":\"hello after role\"}}]}\n\ndata: [DONE]\n\n")
}

// roleOnlyThenDoneSSE opens with the role-only chunk and then ends with a
// bare [DONE] — a stream that never produced a usable event.
func roleOnlyThenDoneSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, roleOnlyChunk)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// roleOnlyHoldSSE streams role-only chunks forever (until the proxy aborts
// the attempt) — a hold that never reaches a usable event.
func roleOnlyHoldSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		fmt.Fprint(w, roleOnlyChunk)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// reasoningStreamSSE holds silent for delay (a "reasoning" model thinking)
// and only then streams content + [DONE] — for the per-provider/model TTFT
// override test (task J).
func reasoningStreamSSE(delay time.Duration) streamBehavior {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
		fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-provider-a\",\"choices\":[{\"delta\":{\"content\":\"slow reasoning done\"}}]}\n\ndata: [DONE]\n\n")
	}
}

// streamPair is one (provider, upstream server) entry for newStreamChain.
type streamPair struct {
	id  string
	url string
}

// newStreamChain builds a chain over the ordered pairs with the given TTFT
// (seconds) and zero same-candidate retries unless overridden.
func newStreamChain(pairs []streamPair, ttftSeconds, retries int) *FallbackChain {
	rl := ratelimit.NewGlobalTracker()
	providers := make([]config.ProviderConfig, len(pairs))
	entries := make([]strategy.RankedEntry, len(pairs))
	catEntries := make([]catalog.CatalogEntry, len(pairs))
	for i, p := range pairs {
		providers[i] = config.ProviderConfig{ID: p.id, BaseURL: p.url, Enabled: true}
		entries[i] = strategy.RankedEntry{ProviderID: p.id, ModelID: "model-" + p.id}
		catEntries[i] = catalog.CatalogEntry{ProviderID: p.id, ModelID: "model-" + p.id, IsFree: true, TierType: "free"}
	}
	cfg := &config.Config{
		Providers: providers,
		Fallback: config.FallbackConfig{
			MaxAttempts:              6,
			RetriesPerCandidate:      retries,
			FirstTokenTimeoutSeconds: ttftSeconds,
		},
	}
	return &FallbackChain{
		Cfg:                cfg,
		Strategy:           &multiMockStrategy{entries: entries},
		Catalog:            &catalog.Catalog{Entries: catEntries},
		RateLimiter:        rl,
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
		StreamHTTPClient:   &http.Client{},
	}
}

// streamRequest builds the streaming chain request.
func streamRequest() Request {
	return Request{
		Model:       "model-provider-a",
		ClientModel: "model-provider-a",
		Stream:      true,
		Messages:    []map[string]any{{"role": "user", "content": "hi"}},
		Raw:         map[string]any{"model": "model-provider-a", "stream": true},
	}
}

// TestStreamTTFTTimeoutFallsBackSilently: a candidate that sits on a 200
// without a usable first event past first_token_timeout_seconds is dropped
// and the walk falls to the next candidate — the client sees ONLY the second
// candidate's events (nothing of the abandoned attempt).
func TestStreamTTFTTimeoutFallsBackSilently(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, holdForeverSSE)
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 1, 0)
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}

	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want a committed stream from provider-b", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Errorf("provider-a hits = %d, want 1 (abandoned after the TTFT timeout)", got)
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Errorf("provider-b hits = %d, want 1", got)
	}
	body := rec.Body.String()
	if strings.Contains(body, "provider-a") && !strings.Contains(body, "m-b") {
		t.Errorf("client saw the abandoned attempt's bytes: %q", body)
	}
	if !strings.Contains(body, "hello from b") {
		t.Errorf("client missed the committed stream: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("client missed the [DONE] sentinel: %q", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream (NG3)", got)
	}
	if got := atomic.LoadInt32(&rec.calls); got != 1 {
		t.Errorf("WriteHeader called %d times, want exactly once", got)
	}
}

// TestStreamBadFirstEventFallsThrough: an error-object first event and a
// bare [DONE] first event both fail the attempt BEFORE the commit — the walk
// continues to the next candidate and the client never sees them.
func TestStreamBadFirstEventFallsThrough(t *testing.T) {
	for _, bad := range []streamBehavior{errorFirstEventSSE, doneFirstEventSSE} {
		var hitsA, hitsB int32
		srvA := sseServer(&hitsA, bad)
		srvB := sseServer(&hitsB, goodStreamSSE("b"))
		fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 5, 0)
		rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}

		_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
		if err != nil {
			t.Fatalf("ExecuteStream error = %v, want fallback to provider-b", err)
		}
		if !committed {
			t.Fatal("stream not committed")
		}
		if got := atomic.LoadInt32(&hitsB); got != 1 {
			t.Errorf("provider-b hits = %d, want 1 (bad first event must fall through)", got)
		}
		body := rec.Body.String()
		if strings.Contains(body, "provider overloaded") {
			t.Errorf("upstream error event leaked to the client: %q", body)
		}
		if !strings.Contains(body, "hello from b") || !strings.Contains(body, "data: [DONE]") {
			t.Errorf("client missed the good stream: %q", body)
		}
		if got := atomic.LoadInt32(&rec.calls); got != 1 {
			t.Errorf("WriteHeader called %d times, want exactly once", got)
		}
		srvA.Close()
		srvB.Close()
	}
}

// TestStreamInterruptedAfterCommitGetsErrorEventAndDONE: once committed the
// attempt is terminal — an upstream that dies mid-stream (close without
// [DONE]) must end with the OpenAI-style SSE error event + data: [DONE],
// never a silent truncation and never another candidate.
func TestStreamInterruptedAfterCommitGetsErrorEventAndDONE(t *testing.T) {
	var hits int32
	srv := sseServer(&hits, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-x\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Abnormal end: connection closed without [DONE].
	})
	defer srv.Close()

	fc := newStreamChain([]streamPair{{"provider-a", srv.URL}}, 5, 0)
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}

	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want a terminal committed stream", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (no candidate walking after the commit)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Errorf("client missed the streamed events: %q", body)
	}
	if !strings.Contains(body, "upstream_stream_interrupted") {
		t.Errorf("client missed the SSE error event: %q", body)
	}
	if !strings.Contains(body, "\"type\":\"server_error\"") {
		t.Errorf("error event must use the OpenAI server_error type: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("client missed the [DONE] sentinel after the error event: %q", body)
	}
	if got := atomic.LoadInt32(&rec.calls); got != 1 {
		t.Errorf("WriteHeader called %d times, want exactly once", got)
	}
}

// TestStreamClientDisconnectDuringHoldNoLeak: a client that disconnects while
// the walk holds a role-only-opening upstream (the task-J hold) must abort
// promptly — the attempt ctx dies with the request ctx, no retry runs, and
// no goroutine stays parked on the hold: the goroutine count returns to the
// pre-call level.
func TestStreamClientDisconnectDuringHoldNoLeak(t *testing.T) {
	var hits int32
	srv := sseServer(&hits, roleOnlyHoldSSE)
	defer srv.Close()

	fc := newStreamChain([]streamPair{{"provider-a", srv.URL}}, 60, 0)

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		_, _, _ = fc.ExecuteStream(ctx, streamRequest(), rec)
		close(done)
	}()

	time.Sleep(150 * time.Millisecond) // let the attempt reach the hold
	cancel()                           // the client disconnects

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ExecuteStream did not return after the client disconnected — the hold leaked")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() == before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines did not settle back to %d: now=%d — possible leak", before, runtime.NumGoroutine())
}

// TestJudgeFirstSSE: the "first usable event" criterion (task J) — a
// role-only or empty-delta opening chunk keeps the hold (it is not
// "empty/error"), content/tool_calls/finish_reason commit, an error object
// or a bare [DONE] fails the attempt, and the decision scans complete
// events one at a time (never the whole stream).
func TestJudgeFirstSSE(t *testing.T) {
	tests := []struct {
		name string
		buf  string
		want sseVerdict
	}{
		{"role-only chunk holds", roleOnlyChunk, sseUndecided},
		{"empty delta holds", "data: {\"choices\":[{\"delta\":{}}]}\n\n", sseUndecided},
		{"empty-string content holds", "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n", sseUndecided},
		{"incomplete event holds", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}", sseUndecided},
		{"not-json data holds", "data: \n\n", sseUndecided},
		{"error-null holds", "data: {\"error\":null,\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", sseUndecided},
		{"keepalive then role-only holds", ": keepalive\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", sseUndecided},
		{"content commits", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", sseUsable},
		{"tool calls commit", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\"}]}}]}\n\n", sseUsable},
		{"finish reason commits", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", sseUsable},
		{"content parts array commits", "data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}]}\n\n", sseUsable},
		{"message content commits", "data: {\"choices\":[{\"message\":{\"content\":\"full\"}}]}\n\n", sseUsable},
		{"role-only then content commits", roleOnlyChunk + "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", sseUsable},
		{"error object fails", "data: {\"error\":{\"message\":\"provider overloaded\"}}\n\n", sseUnusable},
		{"bare DONE fails", "data: [DONE]\n\n", sseUnusable},
		{"event error fails", "event: error\ndata: {\"message\":\"x\"}\n\n", sseUnusable},
		{"role-only then DONE fails", roleOnlyChunk + "data: [DONE]\n\n", sseUnusable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict, reason := judgeFirstSSE([]byte(tt.buf))
			if verdict != tt.want {
				t.Errorf("judgeFirstSSE(%q) = %s (%q), want %s", tt.buf, verdict, reason, tt.want)
			}
		})
	}
}

// TestStreamRoleOnlyFirstChunkCommitsNormally: a provider that opens the
// stream with a role-only chunk (delta empty of content) must commit
// normally — the role-only event is NOT "empty/error": the hold continues
// until the first usable event and the buffered role-only event is
// forwarded to the client in order (task J).
func TestStreamRoleOnlyFirstChunkCommitsNormally(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, roleOnlyThenContentSSE)
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 5, 0)
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}

	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want a committed stream from provider-a", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Errorf("provider-a hits = %d, want 1 (the role-only opener must not abort the attempt)", got)
	}
	if got := atomic.LoadInt32(&hitsB); got != 0 {
		t.Errorf("provider-b hits = %d, want 0 (no fallback needed)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Errorf("client missed the role-only opening event (must be forwarded in order): %q", body)
	}
	if !strings.Contains(body, "hello after role") {
		t.Errorf("client missed the content event: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("client missed the [DONE] sentinel: %q", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := atomic.LoadInt32(&rec.calls); got != 1 {
		t.Errorf("WriteHeader called %d times, want exactly once", got)
	}
}

// TestStreamRoleOnlyThenDoneFallsBack: a role-only opener followed by a bare
// [DONE] never produced a usable event — the attempt fails silently and the
// walk falls to the next candidate.
func TestStreamRoleOnlyThenDoneFallsBack(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, roleOnlyThenDoneSSE)
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 5, 0)
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}

	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want a committed stream from provider-b", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Errorf("provider-a hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Errorf("provider-b hits = %d, want 1", got)
	}
	if body := rec.Body.String(); strings.Contains(body, `"role":"assistant"`) || !strings.Contains(body, "hello from b") {
		t.Errorf("client must see only provider-b's stream, got %q", body)
	}
}

// TestStreamRoleOnlyForeverHitsTTFT: an upstream that streams role-only
// chunks forever never reaches a usable event — the TTFT watchdog must drop
// the attempt at first_token_timeout_seconds (the hold is bounded; the
// stream is never buffered whole).
func TestStreamRoleOnlyForeverHitsTTFT(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, roleOnlyHoldSSE)
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 1, 0)
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}

	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want a committed stream from provider-b", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Errorf("provider-a hits = %d, want 1 (abandoned at the TTFT timeout)", got)
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Errorf("provider-b hits = %d, want 1", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "m-x") || !strings.Contains(body, "hello from b") {
		t.Errorf("client must see only provider-b's stream, got %q", body)
	}
}

// TestFirstTokenTimeoutPerProviderModelOverride: fallback.first_token_timeouts
// (task J) lets a reasoning model outlast the global pre-commit hold — with
// the "provider/model" override the slow first token commits; without it the
// global timeout drops the attempt and the next candidate serves.
func TestFirstTokenTimeoutPerProviderModelOverride(t *testing.T) {
	t.Run("override keeps the reasoning model", func(t *testing.T) {
		var hitsA, hitsB int32
		srvA := sseServer(&hitsA, reasoningStreamSSE(1500*time.Millisecond))
		defer srvA.Close()
		srvB := sseServer(&hitsB, goodStreamSSE("b"))
		defer srvB.Close()

		fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 1, 0)
		fc.Cfg.Fallback.FirstTokenTimeouts = map[string]int{"provider-a/model-provider-a": 5}

		rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}
		_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
		if err != nil {
			t.Fatalf("ExecuteStream error = %v, want a committed stream from provider-a", err)
		}
		if !committed {
			t.Fatal("stream not committed")
		}
		if got := atomic.LoadInt32(&hitsA); got != 1 {
			t.Errorf("provider-a hits = %d, want 1 (the override must outlast the 1s global hold)", got)
		}
		if got := atomic.LoadInt32(&hitsB); got != 0 {
			t.Errorf("provider-b hits = %d, want 0", got)
		}
		if body := rec.Body.String(); !strings.Contains(body, "slow reasoning done") {
			t.Errorf("client missed the reasoning model's content: %q", body)
		}
	})

	t.Run("no override drops at the global hold", func(t *testing.T) {
		var hitsA, hitsB int32
		srvA := sseServer(&hitsA, reasoningStreamSSE(1500*time.Millisecond))
		defer srvA.Close()
		srvB := sseServer(&hitsB, goodStreamSSE("b"))
		defer srvB.Close()

		fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 1, 0)

		rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}
		_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
		if err != nil {
			t.Fatalf("ExecuteStream error = %v, want a committed stream from provider-b", err)
		}
		if !committed {
			t.Fatal("stream not committed")
		}
		if got := atomic.LoadInt32(&hitsA); got != 1 {
			t.Errorf("provider-a hits = %d, want 1 (dropped at the 1s global hold)", got)
		}
		if got := atomic.LoadInt32(&hitsB); got != 1 {
			t.Errorf("provider-b hits = %d, want 1", got)
		}
		if body := rec.Body.String(); !strings.Contains(body, "hello from b") {
			t.Errorf("client must see provider-b's stream, got %q", body)
		}
	})

	t.Run("provider-wide override applies to its models", func(t *testing.T) {
		var hitsA, hitsB int32
		srvA := sseServer(&hitsA, reasoningStreamSSE(1500*time.Millisecond))
		defer srvA.Close()
		srvB := sseServer(&hitsB, goodStreamSSE("b"))
		defer srvB.Close()

		fc := newStreamChain([]streamPair{{"provider-a", srvA.URL}, {"provider-b", srvB.URL}}, 1, 0)
		fc.Cfg.Fallback.FirstTokenTimeouts = map[string]int{"provider-a": 5}

		rec := httptest.NewRecorder()
		_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
		if err != nil {
			t.Fatalf("ExecuteStream error = %v, want a committed stream from provider-a", err)
		}
		if !committed {
			t.Fatal("stream not committed")
		}
		if got := atomic.LoadInt32(&hitsB); got != 0 {
			t.Errorf("provider-b hits = %d, want 0 (provider-wide override kept provider-a)", got)
		}
	})
}

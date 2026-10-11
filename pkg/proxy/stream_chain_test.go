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
// the walk holds a silent upstream must abort promptly — the attempt ctx
// dies with the request ctx, no retry runs, and no goroutine stays parked on
// the hold.
func TestStreamClientDisconnectDuringHoldNoLeak(t *testing.T) {
	var hits int32
	srv := sseServer(&hits, holdForeverSSE)
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

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 { // small tolerance for test-runtime goroutines
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines did not settle: before=%d after=%d — possible leak", before, runtime.NumGoroutine())
}

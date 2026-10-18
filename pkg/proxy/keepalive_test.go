package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// TestKeepaliveWriterCommentOnlyUntilData locks the writer's contract: the
// watchdog sends the 200 header + ": keepalive" comments on its schedule,
// comments never count as data, WriteHeader is guarded to exactly once, and
// the first real Write both commits the header state and flips
// dataCommitted.
func TestKeepaliveWriterCommentOnlyUntilData(t *testing.T) {
	rec := httptest.NewRecorder()
	kw := newKeepaliveWriter(rec, "req-1", 30*time.Millisecond, 30*time.Millisecond)
	if kw.HeaderSent() || kw.DataCommitted() {
		t.Fatal("fresh writer must be header-less and data-less")
	}
	kw.arm()
	// Give the watchdog time to fire (after=30ms, interval=30ms) — stop()
	// must not race the first tick.
	time.Sleep(100 * time.Millisecond)
	kw.stop() // stop waits for the goroutine — deterministic from here

	if rec.Code != http.StatusOK {
		t.Fatalf("keepalive must send the 200 header, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("expected at least one keepalive comment, got %q", body)
	}
	if strings.Contains(body, "data:") {
		t.Fatalf("keepalive comments must carry no data events, got %q", body)
	}
	if !kw.HeaderSent() {
		t.Fatal("headerSent must be set after the watchdog fired")
	}
	if kw.DataCommitted() {
		t.Fatal("comments must not count as committed data")
	}

	// WriteHeader again: guarded — still exactly one recorded code.
	kw.WriteHeader(http.StatusTeapot)
	if rec.Code != http.StatusOK {
		t.Fatalf("second WriteHeader must be swallowed, got %d", rec.Code)
	}

	// The first data byte commits the response.
	kw.Write([]byte(`data: {"x":1}` + "\n\n"))
	if !kw.DataCommitted() {
		t.Fatal("a body Write must mark dataCommitted")
	}
}

// TestKeepaliveWriterDataBeforeFire: a commit that wins the race suppresses
// the keepalive entirely — no comment may ever follow real data.
func TestKeepaliveWriterDataBeforeFire(t *testing.T) {
	rec := httptest.NewRecorder()
	kw := newKeepaliveWriter(rec, "req-1", 80*time.Millisecond, 30*time.Millisecond)
	kw.arm()
	kw.Write([]byte("data: real\n\n"))
	time.Sleep(150 * time.Millisecond)
	kw.stop()
	if strings.Contains(rec.Body.String(), ": keepalive") {
		t.Fatalf("keepalive comments must never follow committed data: %q", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("the data Write implies the 200 header, got %d", rec.Code)
	}
}

// TestKeepaliveWriterStopWithoutArm: stopping an unarmed writer must not
// hang waiting for a goroutine that never started.
func TestKeepaliveWriterStopWithoutArm(t *testing.T) {
	rec := httptest.NewRecorder()
	kw := newKeepaliveWriter(rec, "req-1", time.Second, time.Second)
	done := make(chan struct{})
	go func() { kw.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop() on an unarmed writer deadlocked")
	}
	if rec.Code != 200 && rec.Code != 0 {
		t.Fatalf("unarmed writer must not write anything, got %d", rec.Code)
	}
}

// TestKeepaliveConcurrentWriteAndComment: the keepalive goroutine and the
// handler's commit write serialize — under -race this is the contention
// point (mirrors the hold-commit race).
func TestKeepaliveConcurrentWriteAndComment(t *testing.T) {
	rec := httptest.NewRecorder()
	kw := newKeepaliveWriter(rec, "req-1", 10*time.Millisecond, 10*time.Millisecond)
	kw.arm()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); kw.Write([]byte("data: real\n\n")) }()
	go func() { defer wg.Done(); time.Sleep(5 * time.Millisecond) }()
	wg.Wait()
	time.Sleep(60 * time.Millisecond)
	kw.stop()
	if !kw.HeaderSent() || !kw.DataCommitted() {
		t.Fatalf("expected committed state, headerSent=%v dataCommitted=%v", kw.HeaderSent(), kw.DataCommitted())
	}
}

// TestKeepaliveCommentsDuringQueueWaitThenSSEError: a streaming request
// parked in the slot queue gets ONLY keepalive comments while it waits;
// when the queue budget expires after the header went out, the request
// terminates with the OpenAI-style SSE error event + data: [DONE] — never
// a fake 200 completion (tasks L0+P).
func TestKeepaliveCommentsDuringQueueWaitThenSSEError(t *testing.T) {
	s, rec := queuedServer(t, 3, 1) // queue wait 3s; keepalive after 1s
	// Occupy the single slot for the whole test (a 429-shaped block): the
	// request cannot gain a slot and must wait out its queue budget.
	s.slotLimit.AdjustNextAvailable("tp/model-a", time.Now().Add(10*time.Second))

	bodyCh := make(chan string, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"qwen3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		r := httptest.NewRecorder()
		s.handleChatCompletions(r, req)
		bodyCh <- r.Body.String()
	}()
	select {
	case body := <-bodyCh:
		if !strings.Contains(body, ": keepalive") {
			t.Fatalf("expected keepalive comments during the queue wait, got %q", body)
		}
		if !strings.Contains(body, `"error"`) || !strings.Contains(body, "data: [DONE]") {
			t.Fatalf("post-header failure must end in the SSE error event + [DONE], got %q", body)
		}
		if strings.Contains(body, "real answer") {
			t.Fatalf("no completion content may leak on the failure path, got %q", body)
		}
		if hits := rec.list(); len(hits) != 0 {
			t.Fatalf("the blocked slot must never reach the upstream, hits=%v", hits)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("stream request did not finish within its queue budget")
	}
}

// TestKeepaliveSilentFallbackLeaksNoChunk: candidate A opens the SSE
// (role-only chunk) and stalls — the TTFT watchdog aborts it and the walk
// falls to candidate B, which commits real content. The client may have
// seen keepalive comments, but NEVER a byte of A's held stream (comments
// are not data; A's buffered events were discarded with the attempt).
func TestKeepaliveSilentFallbackLeaksNoChunk(t *testing.T) {
	blockA := make(chan struct{})
	aSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"id":"a1","model":"model-a","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-blockA // stall — the TTFT watchdog must abort this attempt
	}))
	// Defer order matters (LIFO — the LAST registered defer runs FIRST):
	// close(blockA) must run before aSrv.Close(), because Close blocks
	// until the stalled handler returns.
	defer aSrv.Close()
	defer close(blockA)
	bRec := &hitRecorder{}
	bSrv := httptest.NewServer(bRec.handler())
	defer bSrv.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Fallback.FirstTokenTimeoutSeconds = 1 // abort A after 1s
	cfg.Fallback.StreamKeepaliveAfterSeconds = 1
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", BaseURL: aSrv.URL, Enabled: true},
		{ID: "b", BaseURL: bSrv.URL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a", "model-b"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "a", ModelID: "model-a", IsFree: true},
		{ProviderID: "b", ModelID: "model-b", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	r := httptest.NewRecorder()
	s.handleChatCompletions(r, req)

	if r.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", r.Code)
	}
	body := r.Body.String()
	if strings.Contains(body, "model-a") {
		t.Fatalf("candidate A's held stream must never reach the client, got %q", body)
	}
	if !strings.Contains(body, "real answer") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("candidate B must commit the real completion + [DONE], got %q", body)
	}
}

// TestKeepaliveDisabledForNonStream: buffered requests never get the
// keepalive — they wait silently inside the deadline and answer with their
// real JSON (task P).
func TestKeepaliveDisabledForNonStream(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		time.Sleep(1500 * time.Millisecond) // slower than the keepalive window
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, unoJSONCompletion(req.Model))
	}))
	defer slow.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Fallback.StreamKeepaliveAfterSeconds = 1 // would fire if wrongly wired
	cfg.Providers = []config.ProviderConfig{
		{ID: "tp", BaseURL: slow.URL, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "tp", ModelID: "model-a", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3","messages":[{"role":"user","content":"hi"}]}`))
	r := httptest.NewRecorder()
	s.handleChatCompletions(r, req)

	if r.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", r.Code)
	}
	if strings.Contains(r.Body.String(), ": keepalive") {
		t.Fatalf("non-stream requests must not get keepalive comments, got %q", r.Body.String())
	}
	if !strings.Contains(r.Body.String(), "real answer") {
		t.Fatalf("expected the real completion, got %q", r.Body.String())
	}
}

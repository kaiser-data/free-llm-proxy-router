package proxy

import (
	"fmt"
	"io"
	"net"
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

// TestWriteDeadlineRefreshClampsToCeiling (task S, unit): the refresh always
// moves the deadline to now+idle but never past the request's total ceiling.
// The recorder transport cannot hold deadlines — the test asserts the
// COMPUTED value (what refresh would ask SetWriteDeadline for).
func TestWriteDeadlineRefreshClampsToCeiling(t *testing.T) {
	// Idle refresh: now + idle, well under the ceiling.
	ceiling := time.Now().Add(10 * time.Minute)
	ctl := newWriteDeadlineCtl(httptest.NewRecorder(), 30*time.Second, ceiling)
	got := ctl.refresh()
	if got.IsZero() {
		t.Fatal("refresh must produce a deadline when idle > 0")
	}
	if d := time.Until(got); d < 25*time.Second || d > 35*time.Second {
		t.Fatalf("refresh deadline = %s, want now+30s", d)
	}

	// Ceiling clamp: idle would reach past the ceiling — the ceiling wins.
	tightCeiling := time.Now().Add(100 * time.Millisecond)
	ctl2 := newWriteDeadlineCtl(httptest.NewRecorder(), 30*time.Second, tightCeiling)
	got2 := ctl2.refresh()
	if !got2.Equal(tightCeiling) {
		t.Fatalf("refresh deadline = %s, want clamped to the ceiling %s", got2, tightCeiling)
	}

	// Idle disabled (0): no deadline activity at all.
	ctl3 := newWriteDeadlineCtl(httptest.NewRecorder(), 0, ceiling)
	if got3 := ctl3.refresh(); !got3.IsZero() {
		t.Fatalf("idle=0 must not touch the deadline, got %s", got3)
	}
}

// TestFlowingStreamSurvivesBeyondServerWriteTimeout (task S): a stream that
// keeps writing past the OLD http.Server.WriteTimeout (here 100ms) is not
// cut, because every successful write/flush refreshes the per-connection
// deadline through http.ResponseController. Without the refresh the server
// ceiling would kill the connection mid-stream around 100ms.
func TestFlowingStreamSurvivesBeyondServerWriteTimeout(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		ctl := newWriteDeadlineCtl(w, 200*time.Millisecond, time.Now().Add(10*time.Second))
		w = &idleRefreshWriter{ResponseWriter: w, ctl: ctl}
		flusher := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			fmt.Fprintf(w, "data: chunk %d\n\n", i)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond) // 300ms total > WriteTimeout 100ms
		}
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(h))
	ts.Config.WriteTimeout = 100 * time.Millisecond // the old total-ceiling style
	ts.Start()
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 256)
	buf := make([]byte, 128)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	got := string(body)
	for i := 0; i < 6; i++ {
		want := fmt.Sprintf("data: chunk %d", i)
		if !contains(got, want) {
			t.Fatalf("flowing stream was CUT: missing %q in %q (chunks before the cut: %s)", want, got, got)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// TestStalledClientCutByIdleTimeout (task S): a client that stops reading
// blocks the handler's writes — the refresh cannot fire while a write is
// blocked, so the last successful write's idle window cuts the connection.
// The handler must RETURN (write error) shortly after the idle window, not
// hang on the blocked write forever (the old no-deadline behavior).
func TestStalledClientCutByIdleTimeout(t *testing.T) {
	start := time.Now()
	done := make(chan time.Duration, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctl := newWriteDeadlineCtl(w, 300*time.Millisecond, time.Now().Add(30*time.Second))
		w = &idleRefreshWriter{ResponseWriter: w, ctl: ctl}
		defer func() { done <- time.Since(start) }()
		big := make([]byte, 1<<20) // 1MB per write — fills the socket fast
		for i := 0; i < 200; i++ {
			if _, err := w.Write(big); err != nil {
				return // the write deadline fired — the stalled client is cut
			}
		}
	})
	ts := httptest.NewUnstartedServer(h)
	ts.Config.WriteTimeout = 0 // server-level disabled (task S)
	ts.Start()
	defer ts.Close()

	// Raw TCP client: send the request, then NEVER read.
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case took := <-done:
		if took > 5*time.Second {
			t.Fatalf("handler ran %s — the stalled client must be cut ~one idle window (300ms) after the last successful write", took)
		}
		t.Logf("stalled client cut after %s (idle window 300ms + socket-fill time)", took)
	case <-time.After(10 * time.Second):
		t.Fatal("handler still blocked after 10s — the idle write deadline did NOT cut the stalled client")
	}
}

// TestQueueWaitThenStreamCompletes (task S, real TCP): a streaming request
// that WAITS past the old write_timeout (here a 700ms server ceiling) —
// first in a queue-shaped hold, then through a slow first token — still
// streams to completion: the keepalive comments and every data chunk
// refresh the write deadline, and the initial per-request ceiling covers
// the whole queue_wait + attempt + stream budget.
func TestQueueWaitThenStreamCompletes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond) // slow first token — past a 700ms ceiling
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, `data: {"id":"ok","model":"model-a","choices":[{"index":0,"delta":{"content":"c%d"}}]}`+"\n\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Fallback.StreamKeepaliveAfterSeconds = 1 // first comment at 1s (inside the wait)
	cfg.Fallback.StreamKeepaliveIntervalSeconds = 1
	cfg.Fallback.FirstTokenTimeoutSeconds = 5
	// WriteIdleTimeoutSeconds unset → default 30s idle; every write/flush
	// refreshes. The initial per-request ceiling (automatic) replaces the
	// server-level WriteTimeout below — that is the mechanism under test.
	cfg.Fallback.StreamIdleTimeoutSeconds = 5
	cfg.Providers = []config.ProviderConfig{{ID: "tp", BaseURL: upstream.URL, Enabled: true}}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{{ProviderID: "tp", ModelID: "model-a", IsFree: true}}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	// The REAL server wiring: the deadline middleware wraps the chat
	// handler; the server ceiling is the old-style 700ms — the new
	// mechanism must override it for the whole 1.2s wait + 0.6s stream.
	ts := httptest.NewUnstartedServer(s.withWriteDeadlines(http.HandlerFunc(s.handleChatCompletions)))
	ts.Config.WriteTimeout = 700 * time.Millisecond
	ts.Start()
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"qwen3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	got := string(raw)
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("expected keepalive comments during the wait, got %q", got)
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(got, fmt.Sprintf(`"content":"c%d"`, i)) {
			t.Fatalf("the post-wait stream was cut: missing chunk c%d in %q", i, got)
		}
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("stream must end with [DONE], got %q", got)
	}
}

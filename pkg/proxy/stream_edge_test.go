package proxy

import (
	"bufio"
	"bytes"
	"context"
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
	return NewServer(cfg, cat, strategy.NewRegistry(rel, rl, "", "", 3, 5), rl, nil, rel)
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

// Some providers stream SSE with a wrong or missing Content-Type. The chain
// must still recognise the stream instead of treating it as an empty JSON body.
func TestStream_SSEWithoutEventStreamContentType(t *testing.T) {
	for _, ct := range []string{"text/plain; charset=utf-8", "application/json", ""} {
		t.Run(ct, func(t *testing.T) {
			alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Content-Type"] = []string{ct}
				io.WriteString(w, sseHello+sseDone)
			}))
			defer alpha.Close()
			rec, _ := executeToClient(t, twoProviderChain(alpha.URL, "http://127.0.0.1:1"))
			if rec.Body.String() != sseHello+sseDone {
				t.Errorf("client got %q", rec.Body.String())
			}
		})
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
		if !strings.HasPrefix(got, sseHello) {
			t.Errorf("client got %q", got)
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

var _ = context.Background

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

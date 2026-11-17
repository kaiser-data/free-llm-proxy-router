package proxy

import (
	"bufio"
	"context"
	"encoding/json"
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
	s := NewServer(cfg, cat, strategy.NewRegistry(rel, rl, "", "", 3, 5), rl, nil, rel)
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

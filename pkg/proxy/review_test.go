package proxy

// Regression tests from an independent review of the streaming and
// failure-handling changes (#28, #29). Each one failed before its fix.

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// R1: provider timeout (30s default) cuts a committed stream mid-answer.
func TestReview_ProviderTimeoutCutsCommittedStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseHello)
		w.(http.Flusher).Flush()
		for i := 0; i < 6; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			io.WriteString(w, `data: {"choices":[{"delta":{"content":" more"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, sseDone)
	}))
	defer up.Close()
	fc := twoProviderChain(up.URL, up.URL)
	fc.Cfg.Providers[0].Timeout = "700ms"
	// client side: real HTTP server so we can see what an SDK would see
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := fc.Execute(r.Context(), streamReq())
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		writeUpstreamStream(w, resp)
	}))
	defer front.Close()
	res, err := http.Post(front.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, rerr := io.ReadAll(res.Body)
	t.Logf("client read err=%v status=%d body=%q", rerr, res.StatusCode, b)
	if !bytes.Contains(b, []byte("[DONE]")) {
		t.Errorf("stream cut by provider timeout; client got clean EOF=%v without [DONE] or error event", rerr == nil)
	}
}

// R2: one provider 400s for its own quirk, every other provider merely skipped
// (cooldown) -> client gets a non-retryable 400 instead of 503.
func TestReview_RejectionPassthroughIgnoresSkippedProviders(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"'tool_choice' value 'required' is not supported"}}`, 400)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody)
	}))
	defer good.Close()
	fc := twoProviderChain(good.URL, bad.URL)
	fc.Stream = false
	fc.RateLimiter.SetCooldown("alpha", time.Now().Add(time.Minute)) // e.g. 429 cooldown from another request
	resp, err := fc.Execute(context.Background(), hiReq())
	if err == nil {
		t.Errorf("client gets status %d (non-retryable) while a working provider is only on cooldown: %s", resp.StatusCode, resp.Body)
	}
}

// R3: a model-specific 400 (not the client's fault) is passed through as 400.
func TestReview_ModelSpecific400PassedToClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"The model x has been decommissioned"}}`, 400)
	}))
	defer srv.Close()
	fc, _, _ := newTestChain(srv.URL, "p", "m")
	resp, err := fc.Execute(context.Background(), hiReq())
	if err == nil {
		t.Errorf("client gets %d for a decommissioned model (its request was fine): %s", resp.StatusCode, resp.Body)
	}
}

// R4: false-positive model cooldowns from request-dependent 400s.
func TestReview_ModelSpecific400FalsePositives(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Image input is not supported by this model"}}`,
		`{"error":{"message":"tools is not supported for this model"}}`,
		`{"error":{"message":"This model only supports a single image per request"}}`,
		`{"error":{"message":"The 'functions' parameter has been deprecated, use 'tools'"}}`,
		`{"error":{"message":"'logprobs' is no longer supported"}}`,
		`{"error":{"message":"response_format json_schema: only supports strict mode"}}`,
		`{"error":{"message":"Invalid model parameters: temperature must be <= 1"}}`,
	} {
		if isModelSpecific400([]byte(body)) {
			t.Errorf("1h model cooldown for everyone from request-shaped 400: %s", body)
		}
	}
}

// R5: a content 403 (OpenRouter moderation) with N keys burns every key for 10 min.
func TestReview_Moderation403BurnsAllKeys(t *testing.T) {
	t.Setenv("FLPR_R5", "k1")
	t.Setenv("FLPR_R5_2", "k2")
	t.Setenv("FLPR_R5_3", "k3")
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":{"code":403,"message":"Your chosen model requires moderation and your input was flagged"}}`, 403)
	}))
	defer srv.Close()
	fc, rl, _ := newTestChain(srv.URL, "openrouter2", "m")
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_R5"
	fc.Execute(context.Background(), hiReq())
	n := 0
	for s := 0; s < 3; s++ {
		if rl.IsOnCooldown(authCooldownKey("openrouter2", s, 3)) {
			n++
		}
	}
	t.Logf("calls=%d keys cooled=%d", calls, n)
	if n == 3 {
		t.Errorf("one flagged prompt put all 3 keys on 10min cooldown for every client")
	}
}

// R6: multi-key provider with every key auth-cooled: each model is walked
// individually with a misleading "rate limited on every key" log line.
func TestReview_AllKeysBadWalksEveryModel(t *testing.T) {
	t.Setenv("FLPR_R6", "k1")
	t.Setenv("FLPR_R6_2", "k2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, okBody) }))
	defer srv.Close()
	fc, rl, _ := newTestChain(srv.URL, "nim", "m0")
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_R6"
	var l listStrategy
	for i := 0; i < 50; i++ {
		l = append(l, strategy.RankedEntry{ProviderID: "nim", ModelID: "m" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	fc.Strategy = l
	rl.SetCooldown("nim#key1", time.Now().Add(time.Minute))
	rl.SetCooldown("nim#key2", time.Now().Add(time.Minute))
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)
	fc.Execute(context.Background(), hiReq())
	n := strings.Count(buf.String(), "rate limited on every key")
	if n > 1 {
		t.Errorf("%d log lines 'rate limited on every key' for one request (keys are auth-rejected, not rate limited)", n)
	}
}

// R7: CR-only / multi-line data never commits: the whole answer is buffered,
// then discarded as empty.
func TestReview_MultiLineDataNeverCommits(t *testing.T) {
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

// R8: an in-stream error event with code 400 (context too long) becomes 502 ->
// client gets 503 retry loop instead of the 400.
func TestReview_StreamErrorEvent400Becomes503(t *testing.T) {
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

// R9: whitespace-only content commits a stream (non-stream path would fall back).
func TestReview_WhitespaceContentCommits(t *testing.T) {
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

package proxy

import (
	"context"
	"fmt"
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

// --- Task W helpers: the emptiness contract — a client must NEVER receive
// an empty completion. Every empty form fails the attempt and the walk
// delivers the next candidate's REAL answer; the client never sees a bit of
// the failed one.

// okBody is a real (non-empty) completion body for the given marker.
func okBody(marker string) string {
	return fmt.Sprintf(`{"id":"ok","object":"chat.completion","model":"m-%s","choices":[{"index":0,"message":{"role":"assistant","content":"ANSWER_%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, marker, strings.ToUpper(marker))
}

// emptyHandler returns an upstream handler scripted with per-hit bodies:
// body[i] is served to the i-th hit (the last entry repeats).
func emptyHandler(bodies ...string) (http.HandlerFunc, *int32) {
	var hits int32
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&hits, 1)) - 1
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, bodies[i])
	}), &hits
}

// emptyChain builds a buffered two-candidate chain (a first, b second) with
// the given FallbackConfig.
func emptyChain(cfg *config.Config, urlA, urlB string) *FallbackChain {
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "prov-a", BaseURL: urlA, Enabled: true},
		{ID: "prov-b", BaseURL: urlB, Enabled: true},
	}
	entries := []strategy.RankedEntry{
		{ProviderID: "prov-a", ModelID: "model-a"},
		{ProviderID: "prov-b", ModelID: "model-b"},
	}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "prov-a", ModelID: "model-a", IsFree: true, TierType: "free"},
		{ProviderID: "prov-b", ModelID: "model-b", IsFree: true, TierType: "free"},
	}}
	if cfg.Fallback.MaxAttempts <= 0 {
		cfg.Fallback.MaxAttempts = 6
	}
	return &FallbackChain{
		Cfg:                cfg,
		Strategy:           &multiMockStrategy{entries: entries},
		Catalog:            cat,
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
	}
}

// emptyRequest is the buffered chain request.
func emptyRequest() Request {
	return Request{
		Model:       "model-a",
		ClientModel: "model-a",
		Messages:    []map[string]any{{"role": "user", "content": "hi"}},
		Raw:         map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}
}

// TestEmptyCompletionFormsFallBackToRealAnswer: every empty form of the
// round-11 contract fails the attempt — the client gets provider-b's real
// answer and never a bit of provider-a's empty one.
func TestEmptyCompletionFormsFallBackToRealAnswer(t *testing.T) {
	forms := map[string]string{
		"blank body":               "",
		"whitespace body":          "   \n\t ",
		"unparseable body":         "<html>login wall</html>",
		"choices empty":            `{"id":"x","object":"chat.completion","choices":[]}`,
		"choices missing":          `{"id":"x","object":"chat.completion"}`,
		"content null":             `{"choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`,
		"content empty string":     `{"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
		"content whitespace":       `{"choices":[{"index":0,"message":{"role":"assistant","content":"   "},"finish_reason":"stop"}]}`,
		"finish stop no content":   `{"choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":"stop"}]}`,
		"finish length no content": `{"choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":"length"}]}`,
		"completion_tokens zero":   `{"choices":[{"index":0,"message":{"role":"assistant"}}],"usage":{"prompt_tokens":9,"completion_tokens":0,"total_tokens":9}}`,
		"reasoning only":           `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"I thought hard"},"finish_reason":"stop"}]}`,
		"reasoning only alt key":   `{"choices":[{"index":0,"message":{"role":"assistant","reasoning":"I thought hard"},"finish_reason":"stop"}]}`,
		"empty tool_calls array":   `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[]},"finish_reason":"tool_calls"}]}`,
	}
	for name, emptyBody := range forms {
		t.Run(name, func(t *testing.T) {
			hA, hitsA := emptyHandler(emptyBody)
			srvA := httptest.NewServer(hA)
			defer srvA.Close()
			hB, hitsB := emptyHandler(okBody("b"))
			srvB := httptest.NewServer(hB)
			defer srvB.Close()

			fc := emptyChain(nil, srvA.URL, srvB.URL)
			resp, err := fc.Execute(context.Background(), emptyRequest())
			if err != nil {
				t.Fatalf("Execute error = %v, want provider-b's real answer", err)
			}
			if resp == nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %v, want 200", resp)
			}
			body := string(resp.Body)
			if !strings.Contains(body, "ANSWER_B") {
				t.Fatalf("client body %q — want provider-b's ANSWER_B (never the empty form)", body)
			}
			if strings.Contains(body, "reasoning_content") && name == "reasoning only" {
				t.Fatalf("provider-a's reasoning leaked to the client: %q", body)
			}
			if got := atomic.LoadInt32(hitsA); got < 1 {
				t.Errorf("provider-a hits = %d, want >= 1 (the empty form consumed an attempt)", got)
			}
			if got := atomic.LoadInt32(hitsB); got != 1 {
				t.Errorf("provider-b hits = %d, want 1", got)
			}
		})
	}
}

// TestEmptyUsableFormsAreDelivered: the positive half of the contract — a
// result with a usable signal is NEVER treated as empty, whatever the weird
// usage block around it.
func TestEmptyUsableFormsAreDelivered(t *testing.T) {
	forms := map[string]string{
		"padded content":           `{"choices":[{"index":0,"message":{"role":"assistant","content":" hi "},"finish_reason":"stop"}]}`,
		"valid tool call":          `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"bkk\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"content beats usage zero": `{"choices":[{"index":0,"message":{"role":"assistant","content":"real"},"finish_reason":"stop"}],"usage":{"completion_tokens":0}}`,
		"parts array content":      `{"choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"part text"}]},"finish_reason":"stop"}]}`,
		"reasoning plus content":   `{"choices":[{"index":0,"message":{"role":"assistant","content":"real","reasoning_content":"thought"},"finish_reason":"stop"}]}`,
		"no usage block at all":    `{"choices":[{"index":0,"message":{"role":"assistant","content":"real"},"finish_reason":"stop"}]}`,
	}
	for name, body := range forms {
		t.Run(name, func(t *testing.T) {
			hA, _ := emptyHandler(body)
			srvA := httptest.NewServer(hA)
			defer srvA.Close()
			hB, hitsB := emptyHandler(okBody("b"))
			srvB := httptest.NewServer(hB)
			defer srvB.Close()

			fc := emptyChain(nil, srvA.URL, srvB.URL)
			resp, err := fc.Execute(context.Background(), emptyRequest())
			if err != nil {
				t.Fatalf("Execute error = %v, want the usable body delivered", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			// The usable form is delivered VERBATIM — provider-b's answer
			// (and any fallback) must never appear.
			if strings.Contains(string(resp.Body), "ANSWER_B") {
				t.Fatalf("provider-b's answer leaked — the usable form must be delivered as-is: %q", resp.Body)
			}
			if got := atomic.LoadInt32(hitsB); got != 0 {
				t.Errorf("provider-b hits = %d, want 0 (a usable result never falls back)", got)
			}
		})
	}
}

// TestCommitOnReasoningDeliversReasoningOnly: fallback.commit_on_reasoning
// flips the reasoning-only verdict — the same body that failed and fell back
// under the default is delivered verbatim when the knob is true.
func TestCommitOnReasoningDeliversReasoningOnly(t *testing.T) {
	reasoningOnly := `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"I thought hard"},"finish_reason":"stop"}]}`
	hA, hitsA := emptyHandler(reasoningOnly)
	srvA := httptest.NewServer(hA)
	defer srvA.Close()
	hB, hitsB := emptyHandler(okBody("b"))
	srvB := httptest.NewServer(hB)
	defer srvB.Close()

	cfg := &config.Config{}
	cfg.Fallback.CommitOnReasoning = true
	fc := emptyChain(cfg, srvA.URL, srvB.URL)
	resp, err := fc.Execute(context.Background(), emptyRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want the reasoning-only body delivered", err)
	}
	if !strings.Contains(string(resp.Body), "I thought hard") {
		t.Fatalf("commit_on_reasoning must deliver the reasoning-only body verbatim: %q", resp.Body)
	}
	if got := atomic.LoadInt32(hitsA); got != 1 || atomic.LoadInt32(hitsB) != 0 {
		t.Errorf("hits a=%d b=%d, want 1/0 (committed on the first candidate)", atomic.LoadInt32(hitsA), atomic.LoadInt32(hitsB))
	}
}

// TestEmptyCompletionLogCarriesReqAndPromptSize: the empty attempt is logged
// with req=, the "empty" cause, and the prompt size (task W).
func TestEmptyCompletionLogCarriesReqAndPromptSize(t *testing.T) {
	logs := captureLogs(t)
	hA, _ := emptyHandler(`{"choices":[]}`)
	srvA := httptest.NewServer(hA)
	defer srvA.Close()
	hB, _ := emptyHandler(okBody("b"))
	srvB := httptest.NewServer(hB)
	defer srvB.Close()

	fc := emptyChain(nil, srvA.URL, srvB.URL)
	req := emptyRequest()
	req.RequestID = "req-empty-42"
	if _, err := fc.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "req=req-empty-42") {
		t.Errorf("attempt log missing req=req-empty-42:\n%s", out)
	}
	if !strings.Contains(out, "empty completion") || !strings.Contains(out, "reason=choices_empty") {
		t.Errorf("attempt log missing the empty cause + reason:\n%s", out)
	}
	if !strings.Contains(out, "prompt_bytes=") || !strings.Contains(out, "prompt_tokens_est=") {
		t.Errorf("attempt log missing the prompt size:\n%s", out)
	}
}

// TestEmptyCompletionScoresWasted: the empty attempt scores the heaviest
// penalty (OutcomeWasted) — the EMA after one empty answer must sit at the
// wasted weight, not the milder rate-limit one (task W).
func TestEmptyCompletionScoresWasted(t *testing.T) {
	hA, _ := emptyHandler(`{"choices":[]}`)
	srvA := httptest.NewServer(hA)
	defer srvA.Close()
	hB, _ := emptyHandler(okBody("b"))
	srvB := httptest.NewServer(hB)
	defer srvB.Close()

	fc := emptyChain(nil, srvA.URL, srvB.URL)
	if _, err := fc.Execute(context.Background(), emptyRequest()); err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	got := fc.ReliabilityTracker.SuccessRate("prov-a")
	wasted := reliability.New()
	wasted.RecordOutcome("p", reliability.OutcomeWasted)
	limited := reliability.New()
	limited.RecordOutcome("p", reliability.OutcomeRateLimited)
	if got != wasted.SuccessRate("p") {
		t.Errorf("empty attempt scored %v, want the OutcomeWasted weight (%v) — the heaviest penalty", got, wasted.SuccessRate("p"))
	}
	if got == limited.SuccessRate("p") {
		t.Errorf("empty attempt scored the OutcomeRateLimited weight — the heaviest penalty must apply")
	}
}

// --- Stream side: the same emptiness contract on the pre-commit hold (the
// client never sees a committed empty stream; the attempt fails silently and
// the next candidate answers).

// finishStopNoContentSSE: a stream that ends with a finish_reason=stop chunk
// and never sent content or tool calls (round 11 task W — the old criterion
// committed this empty stream).
func finishStopNoContentSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

// whitespaceContentSSE: content that is only whitespace, then a stop.
func whitespaceContentSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"   \"}}]}\n\n")
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

// reasoningOnlyThenDoneSSE: a thinking model that streamed reasoning_content
// and ended with [DONE] — no content, no tool calls (round 11 task W).
func reasoningOnlyThenDoneSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"deep thought\"}}]}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"more thought\"}}]}\n\ndata: [DONE]\n\n")
}

// eofNoEventSSE: 200 SSE headers flushed, then the connection closes with
// not a single event.
func eofNoEventSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// contentFilterStreamSSE: the refusal arrives as a finish_reason chunk.
func contentFilterStreamSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\ndata: [DONE]\n\n")
}

// TestStreamEmptyFormsFallBackSilently: every empty stream form fails the
// attempt before the commit — provider-b's real stream is committed and the
// client never sees provider-a's bytes.
func TestStreamEmptyFormsFallBackSilently(t *testing.T) {
	behaviors := map[string]streamBehavior{
		"bare DONE first":              doneFirstEventSSE,
		"finish stop without content":  finishStopNoContentSSE,
		"whitespace content then stop": whitespaceContentSSE,
		"reasoning-only then DONE":     reasoningOnlyThenDoneSSE,
		"EOF without an event":         eofNoEventSSE,
	}
	for name, behavior := range behaviors {
		t.Run(name, func(t *testing.T) {
			var hitsA, hitsB int32
			srvA := sseServer(&hitsA, behavior)
			defer srvA.Close()
			srvB := sseServer(&hitsB, goodStreamSSE("b"))
			defer srvB.Close()

			fc := newStreamChain([]streamPair{{"prov-a", srvA.URL}, {"prov-b", srvB.URL}}, 3, 0)
			rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}
			_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
			if err != nil {
				t.Fatalf("ExecuteStream error = %v, want provider-b's stream", err)
			}
			if !committed {
				t.Fatal("stream not committed")
			}
			if got := atomic.LoadInt32(&hitsB); got != 1 {
				t.Errorf("provider-b hits = %d, want 1", got)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "hello from b") {
				t.Errorf("client missed provider-b's stream: %q", body)
			}
			if strings.Contains(body, `"model":"m-a"`) {
				t.Errorf("provider-a's bytes leaked to the client: %q", body)
			}
		})
	}
}

// TestStreamCommitOnReasoningDeliversReasoningOnlyStream: with
// fallback.commit_on_reasoning=true the same reasoning-only stream that
// fails under the default is committed verbatim — reasoning + a clean
// [DONE] close (round 11 task W, the flipped knob).
func TestStreamCommitOnReasoningDeliversReasoningOnlyStream(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, reasoningOnlyThenDoneSSE)
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"prov-a", srvA.URL}, {"prov-b", srvB.URL}}, 3, 0)
	fc.Cfg.Fallback.CommitOnReasoning = true
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}
	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want the committed reasoning-only stream", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 || atomic.LoadInt32(&hitsB) != 0 {
		t.Errorf("hits a=%d b=%d, want 1/0", atomic.LoadInt32(&hitsA), atomic.LoadInt32(&hitsB))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "deep thought") {
		t.Errorf("client missed the reasoning content: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("client missed the [DONE] sentinel: %q", body)
	}
	if strings.Contains(body, "hello from b") {
		t.Errorf("provider-b's stream leaked: %q", body)
	}
}

// reasoningKeepAliveSSE streams a reasoning_content chunk every `every`
// (6 chunks) and only then the content — a thinking model whose reasoning
// must keep the pre-commit hold alive (round 11 task W).
func reasoningKeepAliveSSE(every time.Duration) streamBehavior {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		for i := 0; i < 6; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(every):
			}
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"step %d\"}}]}\n\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"finally the answer\"}}]}\n\ndata: [DONE]\n\n")
	}
}

// TestStreamReasoningKeepsTTFTHoldAlive: the TTFT watchdog (1s) would cut a
// silent hold, but a stream that keeps producing reasoning_content chunks is
// WORKING — the hold re-arms on every reasoning event and the content that
// arrives at ~1.5s commits from the FIRST candidate (no fallback).
func TestStreamReasoningKeepsTTFTHoldAlive(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, reasoningKeepAliveSSE(250*time.Millisecond))
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"prov-a", srvA.URL}, {"prov-b", srvB.URL}}, 1, 0)
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}
	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want the committed stream from provider-a", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hitsB); got != 0 {
		t.Errorf("provider-b hits = %d, want 0 (reasoning must keep the hold alive — no fallback)", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "finally the answer") {
		t.Errorf("client missed provider-a's content: %q", body)
	}
}

// TestContentFilterIsFailFastNotFallback: finish_reason=content_filter (a
// 200 from the upstream) aborts the walk — the client gets the synthesized
// 422 envelope, and the SECOND candidate is never entered (the refusal is
// deterministic; another upstream cannot fix it).
func TestContentFilterIsFailFastNotFallback(t *testing.T) {
	filtered := `{"choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"content_filter"}]}`
	hA, hitsA := emptyHandler(filtered)
	srvA := httptest.NewServer(hA)
	defer srvA.Close()
	hB, hitsB := emptyHandler(okBody("b"))
	srvB := httptest.NewServer(hB)
	defer srvB.Close()

	fc := emptyChain(nil, srvA.URL, srvB.URL)
	resp, err := fc.Execute(context.Background(), emptyRequest())
	if err != nil {
		t.Fatalf("Execute error = %v, want the surfaced content_filter response", err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (a clear error, never a masked 200)", resp.StatusCode)
	}
	if body := string(resp.Body); !strings.Contains(body, "content_filter") || strings.Contains(body, "ANSWER_B") {
		t.Fatalf("body = %q, want the content_filter envelope without provider-b's answer", body)
	}
	if got := atomic.LoadInt32(hitsB); got != 0 {
		t.Errorf("provider-b hits = %d, want 0 (fail-fast: the walk must not continue)", got)
	}
	if got := atomic.LoadInt32(hitsA); got != 1 {
		t.Errorf("provider-a hits = %d, want 1 (no same-candidate retry on a refusal)", got)
	}
}

// TestStreamContentFilterFailsFast: the finish_reason=content_filter chunk
// ends the walk — the returned response is the 422 envelope and the second
// candidate is never entered.
func TestStreamContentFilterFailsFast(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, contentFilterStreamSSE)
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"prov-a", srvA.URL}, {"prov-b", srvB.URL}}, 3, 0)
	rec := httptest.NewRecorder()
	resp, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil || committed {
		t.Fatalf("ExecuteStream = (%v, %v), want the surfaced 422 response", err, committed)
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %v, want 422", resp)
	}
	if body := string(resp.Body); !strings.Contains(body, "content_filter") {
		t.Errorf("body = %q, want the content_filter envelope", body)
	}
	if got := atomic.LoadInt32(&hitsB); got != 0 {
		t.Errorf("provider-b hits = %d, want 0 (fail-fast)", got)
	}
}

// TestStreamNonSSEEmptyBodyFallsBack: a provider that ignored stream:true
// and answered a buffered JSON empty completion — the passthrough guard
// applies the full emptiness contract and the walk falls to the next
// candidate (round 11 task W).
func TestStreamNonSSEEmptyBodyFallsBack(t *testing.T) {
	var hitsA, hitsB int32
	srvA := sseServer(&hitsA, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":"stop"}]}`)
	})
	defer srvA.Close()
	srvB := sseServer(&hitsB, goodStreamSSE("b"))
	defer srvB.Close()

	fc := newStreamChain([]streamPair{{"prov-a", srvA.URL}, {"prov-b", srvB.URL}}, 3, 0)
	rec := &writeHeaderCounter{ResponseRecorder: httptest.NewRecorder()}
	_, committed, err := fc.ExecuteStream(context.Background(), streamRequest(), rec)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want provider-b's stream", err)
	}
	if !committed {
		t.Fatal("stream not committed")
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Errorf("provider-b hits = %d, want 1", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "hello from b") {
		t.Errorf("client missed provider-b's stream: %q", body)
	}
}

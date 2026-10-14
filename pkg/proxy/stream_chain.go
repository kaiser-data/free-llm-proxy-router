package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
)

// Streaming contract for the pre-commit hold (second P0 pass, task E):
//
//   - NOTHING is written to the client until a usable first SSE event
//     arrives (non-empty, not a comment/keepalive, not an error event, not
//     a bare [DONE]). Every failure before that point — transport error,
//     non-200 status, TTFT timeout, empty/error first event — is silent to
//     the client and runs through the retry ladder (ClassifyFailure):
//     retry_same, fallback, or fail_fast.
//   - The commit happens exactly once per request: SSE headers +
//     WriteHeader(200) + first bytes, then the pump. A second WriteHeader
//     would panic the handler or corrupt the stream — commit is guarded.
//   - After the commit the attempt is terminal: no retry, no cross-provider
//     continuation. Any failure ends with an OpenAI-style SSE error event +
//     data: [DONE] so SSE clients see a structured error instead of a
//     silently truncated completion.
//   - first_token_timeout_seconds bounds the pre-commit hold;
//     stream_idle_timeout_seconds bounds each silent gap after the commit;
//     the stream HTTP client's own cap (fallback.stream_timeout_seconds,
//     NG6) bounds the whole attempt — separate from write_timeout.

// streamClientParams groups the knobs of one streaming attempt that are not
// provider config. The hold/pump logic below has no access to Server/Config
// — callers assemble the params, tests drive them directly.
type streamClientParams struct {
	Client            *http.Client
	FirstTokenTimeout time.Duration // 0 → 30s default
	IdleTimeout       time.Duration // 0 → 30s default
	RequestID         string        // X-Request-Id on commit (may be empty)
	// FirstTokenTimeoutFor resolves the per-provider/model override of
	// FirstTokenTimeout (fallback.first_token_timeouts, task J); nil keeps
	// the global value everywhere.
	FirstTokenTimeoutFor func(providerID, modelID string) time.Duration
	// CanonicalFor resolves the X-Used-Model identity for a commit:
	// (providerID, upstreamModel) → canonical name. Nil = identity.
	CanonicalFor func(providerID, upstreamModel string) string
	// OnTTFT, when non-nil, receives every measured time-to-first-token
	// observation (round 10): at the commit point and at every
	// TTFT-watchdog abort. forced marks a watchdog abort — counted slow
	// regardless of the configured threshold. Nil disables tracking.
	OnTTFT func(providerID, modelID string, ttft time.Duration, forced bool)
	// CommitOnReasoning (round 11, task W): when true, a stream that ends
	// with ONLY reasoning content (reasoning_content / reasoning — no
	// content, no tool_calls) is delivered verbatim instead of failing the
	// attempt (the fallback.commit_on_reasoning knob, default false).
	// Reasoning is never a commit trigger on its own in either mode — it
	// only extends the pre-commit hold as a working signal.
	CommitOnReasoning bool
}

// sseChunkSize is the pump read buffer.
const sseChunkSize = 4096

// streamUpstreamHeld performs one streaming attempt against an upstream.
//
// Returns:
//
//	resp != nil, !committed — a non-200 upstream response: classify and let
//	                        the ladder decide; nothing was written to the client
//	committed               — the client was answered (headers written);
//	                        any post-commit failure already ended in the
//	                        SSE error event + [DONE]; the attempt is terminal
//	err != nil, !committed  — transport-level failure before any byte:
//	                        classify and let the ladder decide
func streamUpstreamHeld(ctx context.Context, w http.ResponseWriter, cfg config.ProviderConfig, body map[string]any, p streamClientParams) (*Response, bool, error) {
	// The pre-commit hold is a WAITING phase (task P): arm the SSE
	// keepalive — if the wait passes stream_keepalive_after_seconds the
	// client gets the 200 header + ": keepalive\n\n" comments while the
	// hold keeps every data byte buffered.
	armKeepalive(w)
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	upstreamModel, _ := body["model"].(string)
	// TTFT measurement (round 10): the whole attempt start → the commit
	// point (or the watchdog abort) is the time-to-first-token the
	// client experienced. Wired into the latency tracker + slow-TTFT
	// circuit breaker via OnTTFT.
	attemptStart := time.Now()
	measureTTFT := func(forced bool) {
		if p.OnTTFT != nil {
			p.OnTTFT(cfg.ID, upstreamModel, time.Since(attemptStart), forced)
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, false, fmt.Errorf("encoding stream request: %w", err)
	}

	// The attempt ctx is cancelable so the TTFT/idle watchdogs can abort
	// the in-flight upstream call and body reads without touching the
	// parent (the parent's cancel still means "client/budget gone").
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, false, fmt.Errorf("building stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if h, v := cfg.ResolvedAuth(); v != "" && v != "Bearer " {
		req.Header.Set(h, v)
	}
	for k, val := range cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}

	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	// The TTFT watchdog covers the WHOLE pre-commit window — awaiting the
	// response headers AND the first usable event. An upstream that never
	// flushes its headers would otherwise hang client.Do forever when the
	// client carries no cap of its own.
	ttft := p.FirstTokenTimeout
	if ttft <= 0 {
		ttft = 30 * time.Second
	}
	// Per-provider/model override (fallback.first_token_timeouts, task J) —
	// reasoning models routinely think longer than the global hold.
	if p.FirstTokenTimeoutFor != nil {
		if o := p.FirstTokenTimeoutFor(cfg.ID, upstreamModel); o > 0 {
			ttft = o
		}
	}
	ttftTimer := time.AfterFunc(ttft, cancelAttempt)
	defer ttftTimer.Stop()

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, err // client/budget gone — parent ctx carries it
		}
		if attemptCtx.Err() != nil {
			measureTTFT(true) // watchdog abort — the hardest slow observation
			return nil, false, fmt.Errorf("stream attempt aborted by the TTFT watchdog: %w", context.DeadlineExceeded)
		}
		return nil, false, err
	}
	defer resp.Body.Close()
	ttftTimer.Stop() // headers arrived — the body phases re-arm their own watchdogs

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		out := &Response{StatusCode: resp.StatusCode, Header: http.Header{}, Body: b}
		out.Header.Set("X-Free-Router-Upstream-Provider", cfg.ID)
		if upstreamModel != "" {
			out.Header.Set("X-Free-Router-Upstream-Model", upstreamModel)
		}
		return out, false, nil
	}

	if !isSSEContentType(resp.Header.Get("Content-Type")) {
		// The provider ignored stream:true and answered a buffered JSON
		// completion. Hold it until it is complete and usable, then commit
		// it as JSON under its own content type — JSON is never disguised
		// as an event stream.
		b, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, false, rerr
			}
			return nil, false, fmt.Errorf("reading non-SSE stream body: %w", rerr)
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, false, fmt.Errorf("upstream answered 200 with an empty non-SSE body")
		}
		// The full emptiness contract (round 11 task W) applies to the
		// buffered-JSON passthrough too: null/""/whitespace-only content,
		// empty choices, finish_reason without content, completion_tokens=0
		// without a usable signal, reasoning-only (unless
		// fallback.commit_on_reasoning). content_filter is fail-fast.
		if verdict := inspectCompletion(b, p.CommitOnReasoning); verdict.contentFilter {
			return contentFilterResponse(cfg, body), false, errContentFilter
		} else if verdict.empty {
			return nil, false, fmt.Errorf("upstream answered 200 with an empty non-SSE completion (reason=%s)", verdict.reason)
		}
		usedModel := parseUsedModel(b)
		if usedModel == "" {
			usedModel = upstreamModel
		}
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		if canonical := applyCanonical(p, cfg.ID, usedModel); canonical != "" {
			w.Header().Set("X-Used-Model", canonical)
		}
		if p.RequestID != "" {
			w.Header().Set("X-Request-Id", p.RequestID)
		}
		w.Header().Set("X-Free-Router-Upstream-Provider", cfg.ID)
		if upstreamModel != "" {
			w.Header().Set("X-Free-Router-Upstream-Model", upstreamModel)
		}
		measureTTFT(false) // buffered-JSON passthrough: commit = first token
		w.WriteHeader(http.StatusOK)
		w.Write(b)
		if flusher, ok := flusherOf(w); ok {
			flusher.Flush()
		}
		return &Response{StatusCode: http.StatusOK, UsedModel: usedModel}, true, nil
	}

	// --- Pre-commit hold: read until the first USABLE complete event. ---
	ttftTimer = time.AfterFunc(ttft, cancelAttempt) // re-arm for the hold
	var buf bytes.Buffer
	chunk := make([]byte, sseChunkSize)
	reasoningSeen := false
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			verdict, reason, rseen := judgeFirstSSE(buf.Bytes())
			reasoningSeen = reasoningSeen || rseen
			if verdict != sseUndecided {
				ttftTimer.Stop()
				if verdict == sseUnusable {
					if p.CommitOnReasoning && reasoningSeen {
						// fallback.commit_on_reasoning=true (round 11 task
						// W): the stream produced reasoning and nothing else
						// — deliver it verbatim instead of failing the
						// attempt.
						return commitHeldStream(ctx, w, p, cfg, buf.Bytes(), upstreamModel, measureTTFT, resp.Body, cancelAttempt), true, nil
					}
					if reason == "finish_reason=content_filter" {
						// The deterministic refusal (task W): fail-fast —
						// the synthesized 422 envelope is surfaced, never a
						// masked 200 without content.
						return contentFilterResponse(cfg, body), false, errContentFilter
					}
					return nil, false, fmt.Errorf("upstream %s: first SSE event unusable (%s)", cfg.ID, reason)
				}
				// --- COMMIT: exactly once per request. ---
				usedModel := parseUsedModel(buf.Bytes())
				if usedModel == "" {
					usedModel = upstreamModel
				}
				setStreamContractHeaders(w, applyCanonical(p, cfg.ID, usedModel), cfg.ID, upstreamModel)
				w.Header().Set("Content-Type", "text/event-stream")
				if p.RequestID != "" {
					w.Header().Set("X-Request-Id", p.RequestID)
				}
				measureTTFT(false) // commit = the client's first token
				w.WriteHeader(http.StatusOK)
				if _, werr := w.Write(buf.Bytes()); werr != nil {
					// The client vanished in the commit window — the
					// attempt is terminal either way.
					log.Printf("stream: req=%s client write error at commit (no fallback after commit): %v", p.RequestID, werr)
					return nil, true, nil
				}
				if flusher, ok := flusherOf(w); ok {
					flusher.Flush()
				}
				pumpCommittedStream(ctx, w, resp.Body, p, cancelAttempt)
				return &Response{StatusCode: http.StatusOK, UsedModel: usedModel}, true, nil
			}
			if reasoningSeen {
				// Reasoning is a WORKING signal (round 11, task W): a
				// thinking model streams reasoning_content long before the
				// first content token — that is not silence. Re-arm the
				// TTFT watchdog so a healthy thinking stream is not cut;
				// the hard caps (the stream client's own timeout and the
				// request's total deadline) still bound the hold.
				ttftTimer.Reset(ttft)
			}
		}
		if rerr != nil {
			ttftTimer.Stop()
			if ctx.Err() != nil {
				return nil, false, rerr // client/budget gone
			}
			if attemptCtx.Err() != nil {
				measureTTFT(true) // pre-commit hold outran the TTFT watchdog
				return nil, false, fmt.Errorf("no first token within %v: %w", ttft, context.DeadlineExceeded)
			}
			if rerr == io.EOF {
				if p.CommitOnReasoning && reasoningSeen {
					// The stream ended with only reasoning — deliverable
					// under fallback.commit_on_reasoning (round 11).
					return commitHeldStream(ctx, w, p, cfg, buf.Bytes(), upstreamModel, measureTTFT, nil, cancelAttempt), true, nil
				}
				return nil, false, fmt.Errorf("upstream %s: EOF before a usable first event", cfg.ID)
			}
			return nil, false, fmt.Errorf("reading stream hold: %w", rerr)
		}
	}
}

// commitHeldStream commits the held buffer to the client (round 11, task W)
// — the fallback.commit_on_reasoning delivery paths. Same commit contract
// as the usable-first-event path: headers once, WriteHeader(200) once, the
// buffered bytes, then either the pump (rest != nil — the stream is still
// open, e.g. the finish_reason chunk arrived before [DONE]) or a closing
// [DONE] sentinel (rest == nil — the upstream already ended).
func commitHeldStream(ctx context.Context, w http.ResponseWriter, p streamClientParams, cfg config.ProviderConfig, buf []byte, upstreamModel string, measureTTFT func(bool), rest io.Reader, cancelAttempt context.CancelFunc) *Response {
	usedModel := parseUsedModel(buf)
	if usedModel == "" {
		usedModel = upstreamModel
	}
	setStreamContractHeaders(w, applyCanonical(p, cfg.ID, usedModel), cfg.ID, upstreamModel)
	w.Header().Set("Content-Type", "text/event-stream")
	if p.RequestID != "" {
		w.Header().Set("X-Request-Id", p.RequestID)
	}
	measureTTFT(false) // commit = the client's first token
	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(buf); werr != nil {
		// The client vanished in the commit window — terminal either way.
		log.Printf("stream: req=%s client write error at the reasoning commit (no fallback after commit): %v", p.RequestID, werr)
		return &Response{StatusCode: http.StatusOK, UsedModel: usedModel}
	}
	if rest == nil && !bytes.Contains(buf, []byte("[DONE]")) {
		// The upstream ended before the sentinel — close the SSE stream
		// cleanly so the client never sees a silently truncated stream.
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	if flusher, ok := flusherOf(w); ok {
		flusher.Flush()
	}
	if rest != nil {
		pumpCommittedStream(ctx, w, rest, p, cancelAttempt)
	}
	return &Response{StatusCode: http.StatusOK, UsedModel: usedModel}
}

// sseVerdict is the pre-commit judgment of the buffered bytes.
type sseVerdict int

const (
	sseUndecided sseVerdict = iota // keep holding — no complete usable event yet
	sseUsable                      // first usable event seen — commit
	sseUnusable                    // the first event is an error/empty stream — fail the attempt
)

func (v sseVerdict) String() string {
	switch v {
	case sseUndecided:
		return "undecided"
	case sseUsable:
		return "usable"
	case sseUnusable:
		return "unusable"
	default:
		return "unknown"
	}
}

// judgeFirstSSE decides whether the buffered bytes contain a usable first
// event. Events are scanned one complete event ("\n\n") at a time — the hold
// ends at the first USABLE event and the whole stream is never buffered
// (task J; round 11 task W extends the criteria):
//
//   - keepalive comments and event-only blocks are skipped;
//   - an error object or a bare [DONE] first data payload fails the attempt
//     BEFORE any commit — the ladder then tries the next candidate;
//   - an OpenAI-style chunk with content / VALID tool calls is usable —
//     commit (a finish_reason without content is an ENDING empty stream —
//     it fails; content_filter fails fast);
//   - a ROLE-ONLY, empty-delta, or REASONING-ONLY chunk (several providers
//     open the stream with one; reasoning models stream reasoning_content
//     for a long time) keeps the hold going — it is not "empty/error"; the
//     NEXT event decides. The TTFT watchdog bounds the wait, re-armed while
//     reasoning flows (a working signal, not silence).
//
// The third return value reports whether any scanned event carried
// reasoning — the working signal the hold uses to re-arm the watchdog and
// the commit_on_reasoning delivery uses at stream end.
func judgeFirstSSE(buf []byte) (sseVerdict, string, bool) {
	reasoningSeen := false
	for offset := 0; ; {
		end := bytes.Index(buf[offset:], []byte("\n\n"))
		if end < 0 {
			return sseUndecided, "", reasoningSeen // no complete event yet — keep holding
		}
		end += offset
		verdict, reason, sig := judgeSSEEvent(string(buf[offset:end]))
		reasoningSeen = reasoningSeen || sig.reasoning
		if verdict != sseUndecided {
			return verdict, reason, reasoningSeen
		}
		offset = end + 2
	}
}

// judgeSSEEvent judges ONE complete SSE event block (without the trailing
// blank line); the returned signal aggregates the event's chunk signals.
func judgeSSEEvent(event string) (sseVerdict, string, chunkSignal) {
	agg := chunkSignal{}
	for _, line := range strings.Split(event, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue // comment/keepalive
		}
		if strings.HasPrefix(trimmed, "event:") {
			if strings.Contains(strings.ToLower(trimmed), "error") {
				return sseUnusable, "event: error", agg
			}
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				return sseUnusable, "first event was [DONE] (empty stream)", agg
			}
			sig := sseChunkSignal(payload)
			agg.reasoning = agg.reasoning || sig.reasoning
			if sig.errShaped {
				return sseUnusable, "first event carried an error object", agg
			}
			if sig.usable {
				return sseUsable, "", agg
			}
			if sig.finish != "" {
				// The stream is ENDING and produced no usable signal (round
				// 11 task W): finish_reason=stop/length/… with no content
				// and no valid tool_calls is an empty completion — fail the
				// attempt so the ladder falls back. content_filter is the
				// fail-fast refusal.
				if sig.finish == "content_filter" {
					return sseUnusable, "finish_reason=content_filter", agg
				}
				return sseUnusable, fmt.Sprintf("stream ended with finish_reason=%s and no content", sig.finish), agg
			}
			continue // role-only / empty delta / reasoning-only — keep holding
		}
	}
	// A complete event without data lines — keep holding for real content.
	return sseUndecided, "", agg
}

// chunkSignal aggregates what one OpenAI-style chunk data payload carries
// (round 11 task W):
//
//	usable    — a content / VALID tool_calls signal: the commit trigger
//	            (an arguments-only tool_calls continuation delta is NOT
//	            valid — the hold continues for the named call);
//	errShaped — an error object;
//	reasoning — reasoning_content / reasoning text: a WORKING signal, never
//	            a commit trigger on its own;
//	finish    — a finish_reason value: with no usable signal the stream is
//	            ending empty (stop/length/…); content_filter fails fast.
type chunkSignal struct {
	usable    bool
	errShaped bool
	reasoning bool
	finish    string
}

// sseChunkSignal reports what one OpenAI-style chunk data payload carries.
// Unparseable or signal-free payloads (role-only and empty deltas) report
// the zero signal so the hold continues and the next event decides.
func sseChunkSignal(payload string) chunkSignal {
	var chunk struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Delta struct {
				Role             string          `json:"role"`
				Content          json.RawMessage `json:"content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
			} `json:"delta"`
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	sig := chunkSignal{}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return sig // not chunk JSON — the next event decides
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		sig.errShaped = true
		return sig
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != "" && sig.finish == "" {
			sig.finish = c.FinishReason
		}
		if len(c.Delta.ToolCalls) > 0 && string(c.Delta.ToolCalls) != "null" && validToolCalls(c.Delta.ToolCalls) {
			sig.usable = true
		}
		if contentHasText(c.Delta.Content) || contentHasText(c.Message.Content) {
			sig.usable = true
		}
		if reasoningHasText(c.Delta.ReasoningContent) || reasoningHasText(c.Delta.Reasoning) ||
			reasoningHasText(c.Message.ReasoningContent) || reasoningHasText(c.Message.Reasoning) {
			sig.reasoning = true
		}
	}
	return sig
}

// contentHasText reports whether a content JSON value carries text: a
// NON-BLANK string (whitespace-only is empty — round 11 task W) or a
// non-empty content-parts array (some providers stream content as
// [{"type":"text","text":"…"}]). Any other non-null JSON counts as content.
func contentHasText(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		for _, p := range parts {
			if strings.TrimSpace(p.Text) != "" {
				return true
			}
		}
		return false
	}
	return true
}

// parseUsedModel extracts the "model" field of the first JSON data payload —
// OpenAI-style chunks carry it, mirroring what callProvider reads from
// buffered bodies for X-Used-Model.
func parseUsedModel(buf []byte) string {
	for _, raw := range bytes.Split(buf, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var probe struct {
			Model string `json:"model"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && probe.Model != "" {
			return probe.Model
		}
	}
	return ""
}

// flusherOf returns the Flusher for w when available (the logging
// middleware forwards it — commit 16f01ad).
func flusherOf(w http.ResponseWriter) (http.Flusher, bool) {
	f, ok := w.(http.Flusher)
	return f, ok
}

// applyCanonical resolves the X-Used-Model identity for a commit:
// CanonicalFor(providerID, upstreamModel) when set, identity otherwise.
func applyCanonical(p streamClientParams, providerID, upstreamModel string) string {
	if p.CanonicalFor != nil {
		return p.CanonicalFor(providerID, upstreamModel)
	}
	return upstreamModel
}

// commitOnReasoning reports whether a reasoning-only upstream result may be
// committed to the client (fallback.commit_on_reasoning, round 11 task W).
// Default false: reasoning-only counts as empty and the walk falls back.
func (fc *FallbackChain) commitOnReasoning() bool {
	return fc.Cfg.Fallback.CommitOnReasoning
}

// firstTokenTimeout returns the pre-commit hold cap:
// fallback.first_token_timeout_seconds (default 30).
func (fc *FallbackChain) firstTokenTimeout() time.Duration {
	if s := fc.Cfg.Fallback.FirstTokenTimeoutSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 30 * time.Second
}

// firstTokenTimeoutFor returns the pre-commit hold cap for one provider/model
// pair: the fallback.first_token_timeouts override ("provider/model" then
// "provider"), else the global fallback.first_token_timeout_seconds
// (default 30) — reasoning models think longer than the global default
// (task J).
func (fc *FallbackChain) firstTokenTimeoutFor(providerID, modelID string) time.Duration {
	if s := config.LookupFirstTokenTimeout(fc.Cfg.Fallback.FirstTokenTimeouts, providerID, modelID); s > 0 {
		return time.Duration(s) * time.Second
	}
	return fc.firstTokenTimeout()
}

// streamIdleTimeout returns the post-commit per-read gap cap:
// fallback.stream_idle_timeout_seconds (default 30).
func (fc *FallbackChain) streamIdleTimeout() time.Duration {
	if s := fc.Cfg.Fallback.StreamIdleTimeoutSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 30 * time.Second
}

// canonicalForParams adapts the alias resolver for the streaming commit:
// the canonical alias of the upstream model when known, else the raw
// requested id — the same X-Used-Model rule callProvider applies to
// buffered bodies.
func (fc *FallbackChain) canonicalForParams() func(providerID, upstreamModel string) string {
	return func(providerID, upstreamModel string) string {
		if fc.Resolver != nil && upstreamModel != "" {
			if canon := fc.Resolver.CanonicalFor(providerID, upstreamModel); canon != "" {
				return canon
			}
		}
		return upstreamModel
	}
}

// streamAttemptOnce runs one streaming attempt with the chain's streaming
// client (fallback.stream_timeout_seconds — NG6, separate from
// write_timeout) and timeout knobs.
func (fc *FallbackChain) streamAttemptOnce(ctx context.Context, w http.ResponseWriter, callCfg config.ProviderConfig, body map[string]any, reqID string) (*Response, bool, error) {
	// Per-provider timeout override — the same contract callProvider
	// applies to buffered attempts: the deadline must wrap the context
	// BEFORE the request is built.
	pctx := ctx
	cancelP := func() {}
	if d := callCfg.ResolvedTimeout(); d > 0 {
		pctx, cancelP = context.WithTimeout(ctx, d)
	}
	defer cancelP()

	params := streamClientParams{
		Client:               fc.StreamHTTPClient,
		FirstTokenTimeout:    fc.firstTokenTimeout(),
		FirstTokenTimeoutFor: fc.firstTokenTimeoutFor,
		IdleTimeout:          fc.streamIdleTimeout(),
		RequestID:            reqID,
		OnTTFT:               fc.recordTTFT,
		CommitOnReasoning:    fc.commitOnReasoning(),
		CanonicalFor:         fc.canonicalForParams(),
	}
	return streamUpstreamHeld(pctx, w, callCfg, body, params)
}

// streamCandidateWithRetries is the streaming mirror of
// callProviderWithRetries: one attempt plus the bounded same-candidate
// retry ladder (ClassifyFailure decides; retries consume the attempt
// budget; a commit is terminal). Every attempt is logged with the request
// id, candidate, attempt number, cause, duration and decision (task F).
func (fc *FallbackChain) streamCandidateWithRetries(ctx context.Context, w http.ResponseWriter, callCfg config.ProviderConfig, body map[string]any, attempt *int, reqID string) (*Response, bool, error) {
	start := time.Now()
	resp, committed, err := fc.streamAttemptOnce(ctx, w, callCfg, body, reqID)
	dur := time.Since(start)
	if committed {
		return resp, true, nil
	}
	*attempt++
	if err == nil && resp.StatusCode == http.StatusOK {
		fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, dur, "ok", "ok")
		return resp, false, nil
	}
	return fc.retryStreamCandidate(ctx, w, callCfg, body, resp, err, attempt, reqID, dur)
}

// retryStreamCandidate retries the SAME provider/model for the streaming
// walk — the same decide-then-act ladder rules as retrySameCandidate.
// HuggingFace 503-loading stays owned by the walk's dedicated cold-start
// wait.
func (fc *FallbackChain) retryStreamCandidate(ctx context.Context, w http.ResponseWriter, callCfg config.ProviderConfig, body map[string]any, resp *Response, err error, attempt *int, reqID string, lastDur time.Duration) (*Response, bool, error) {
	maxRetries := fc.Cfg.Fallback.RetriesPerCandidate
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > 5 {
		maxRetries = 5
	}
	if callCfg.ID == "huggingface" && resp != nil && resp.StatusCode == http.StatusServiceUnavailable && parseHFLoadingWait(resp.Body) > 0 {
		fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, lastDur, "huggingface 503 loading — deferring to the walk's cold-start wait", "fallback")
		return resp, false, err
	}
	modelID := bodyModel(body)
	timeouts := 0
	if err != nil && isTimeoutErr(err) {
		timeouts = 1
	}
	retriesDone := 0
	for {
		dec := ClassifyFailure(fc.classifierInput(ctx, resp, err, retriesDone, maxRetries, timeouts))
		if dec == RetrySame {
			if *attempt >= fc.maxAttemptsBudget() {
				dec = FallbackNext
			} else {
				base, maxDelay := ratelimit.DefaultBackoffParams()
				if delay := ratelimit.ExponentialBackoff(retriesDone, base, maxDelay); remainingBudget(ctx) > 0 && delay >= remainingBudget(ctx) {
					dec = FallbackNext
				}
			}
		}
		fc.logAttempt(reqID, callCfg.ID, modelID, *attempt, lastDur, failureCause(resp, err), dec.String())
		if dec != RetrySame {
			return resp, false, err
		}
		base, maxDelay := ratelimit.DefaultBackoffParams()
		delay := ratelimit.ExponentialBackoff(retriesDone, base, maxDelay)
		log.Printf("stream fallback: req=%s %s/%s retryable failure — same-candidate retry %d/%d after %v", reqID, callCfg.ID, modelID, retriesDone+1, maxRetries, delay)
		select {
		case <-ctx.Done():
			return resp, false, err
		case <-time.After(delay):
		}
		start := time.Now()
		resp, committed, rerr := fc.streamAttemptOnce(ctx, w, callCfg, body, reqID)
		lastDur = time.Since(start)
		if committed {
			return resp, true, nil
		}
		err = rerr
		*attempt++
		retriesDone++
		if err != nil && isTimeoutErr(err) {
			timeouts++
		}
		if err == nil && resp.StatusCode == http.StatusOK {
			fc.logAttempt(reqID, callCfg.ID, modelID, *attempt, lastDur, "ok", "ok")
			return resp, false, nil
		}
	}
}

// ExecuteStream runs the same candidate walk in streaming mode: every
// attempt holds its upstream response until a usable first event and only
// then commits (pre-commit hold). Pre-commit failures run the retry ladder
// silently — the client sees nothing of them; post-commit failures end
// with the SSE error event + [DONE].
//
// Returns:
//
//	err != nil  — exhaustion / hop ceiling / request budget: the caller
//	              answers with the exhaustion response (nothing was written)
//	committed   — the client was answered by the walk itself
//	resp != nil — a fail-fast upstream error to surface verbatim
func (fc *FallbackChain) ExecuteStream(ctx context.Context, req Request, w http.ResponseWriter) (*Response, bool, error) {
	// The deadline wraps the QUEUE loop — a slot wait counts against the
	// same request budget (task L0).
	ctx, cancelTotal := fc.withTotalDeadline(ctx)
	if cancelTotal != nil {
		defer cancelTotal()
	}
	// Loop mode (round 11, task AA): same contract as Execute — the
	// deadline is the primary stop, transient failures re-enter, and every
	// attempt obeys the pre-commit hold so the client never sees an empty
	// stream.
	if fc.loopUntilDeadline(ctx) {
		fc.Loop = newLoopState()
	}
	for {
		resp, committed, err := fc.walk(ctx, req, fc.streamTry(w, req), fc.streamTryOnce(w, req.RequestID))
		busy, ok := err.(*errAllSlotsBusy)
		if ok {
			// The queue wait is a WAITING phase (task P): arm the SSE
			// keepalive while the request parks.
			armKeepalive(w)
			served, qerr := fc.waitInQueue(ctx, req, busy, false)
			if served {
				continue
			}
			if fc.Loop == nil {
				return nil, false, qerr // single walk — the historical behavior
			}
			// Loop mode: wait a round (the keepalive holds the client) and
			// walk again — bounded by the deadline.
			if ctx.Err() != nil {
				return nil, false, errRequestBudget(ctx)
			}
			if !fc.sleepRound(ctx, req.RequestID) {
				return nil, false, errRequestBudget(ctx)
			}
			continue
		}
		if committed || err == nil {
			return resp, committed, err // answered, or a surfaced fail-fast response
		}
		if fc.Loop == nil {
			return resp, committed, err // single walk — the historical behavior
		}
		// --- Loop mode: the deadline is the primary stop (round 11). ---
		if ctx.Err() != nil {
			return nil, false, errRequestBudget(ctx)
		}
		if errors.Is(err, errHopCeiling) {
			return nil, false, err
		}
		// Last-resort tier streams through the same pre-commit hold.
		if _, committed, engaged := fc.lastResortAttempt(ctx, req, w, true); engaged && committed {
			return nil, true, nil
		}
		if !fc.sleepRound(ctx, req.RequestID) {
			return nil, false, errRequestBudget(ctx)
		}
	}
}

// streamTry wraps the streaming attempt with its same-candidate retry.
func (fc *FallbackChain) streamTry(w http.ResponseWriter, req Request) candidateAttempt {
	reqID := req.RequestID
	return func(ctx context.Context, callCfg config.ProviderConfig, body map[string]any, attempt *int) (*Response, bool, error) {
		return fc.streamCandidateWithRetries(ctx, w, callCfg, body, attempt, reqID)
	}
}

// streamTryOnce is a single streaming attempt — the OpenRouter steps and
// the HF cold-start re-attempt run without the retry ladder. They still
// consume the attempt budget (task G) and emit the same per-attempt log
// (task F) with the decision the walk acts on.
func (fc *FallbackChain) streamTryOnce(w http.ResponseWriter, reqID string) candidateAttempt {
	return func(ctx context.Context, callCfg config.ProviderConfig, body map[string]any, attempt *int) (*Response, bool, error) {
		start := time.Now()
		resp, committed, err := fc.streamAttemptOnce(ctx, w, callCfg, body, reqID)
		dur := time.Since(start)
		*attempt++
		if committed || (err == nil && resp != nil && resp.StatusCode == http.StatusOK) {
			fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, dur, "ok", "ok")
			return resp, committed, err
		}
		fc.logAttempt(reqID, callCfg.ID, bodyModel(body), *attempt, dur, failureCause(resp, err), finalFailureDecision(resp, err).String())
		return resp, committed, err
	}
}

// pumpCommittedStream streams the remaining upstream events to the client.
// Post-commit the attempt is TERMINAL: on any failure — upstream read
// error, idle timeout, abnormal close before [DONE], client write error —
// the stream ends with an OpenAI-style SSE error event + data: [DONE] and
// never retries or continues from another provider. A clean EOF after
// data: [DONE] ends the stream normally.
func pumpCommittedStream(ctx context.Context, w http.ResponseWriter, body io.Reader, p streamClientParams, cancelAttempt context.CancelFunc) {
	idle := p.IdleTimeout
	if idle <= 0 {
		idle = 30 * time.Second
	}
	idleTimer := time.AfterFunc(idle, cancelAttempt)
	defer idleTimer.Stop()

	flusher, canFlush := flusherOf(w)
	buf := make([]byte, sseChunkSize)
	sawDone := false
	// carry holds the tail of the previous chunk so a [DONE] split across
	// two reads is still recognized.
	var carry []byte
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			probe := append(append([]byte(nil), carry...), buf[:n]...)
			if bytes.Contains(probe, []byte("[DONE]")) {
				sawDone = true
			}
			if len(probe) > 8 {
				carry = append(carry[:0], probe[len(probe)-8:]...)
			} else {
				carry = append(carry[:0], probe...)
			}
			idleTimer.Reset(idle)
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client gone mid-stream: no fallback after stream start.
				log.Printf("stream: req=%s client write error (stream aborted, no fallback after commit): %v", p.RequestID, werr)
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF && sawDone {
				return // clean upstream end — the sentinel reached the client
			}
			if ctx.Err() != nil {
				// Client gone: an error event has no audience.
				log.Printf("stream: req=%s upstream ended after commit with client gone: %v", p.RequestID, rerr)
				return
			}
			log.Printf("stream: req=%s upstream interrupted after commit (sawDone=%v): %v", p.RequestID, sawDone, rerr)
			writeSSEErrorEvent(w, flusher, canFlush)
			return
		}
	}
}

// writeSSEErrorEvent terminates a committed-but-interrupted stream with an
// OpenAI-style error event and the [DONE] sentinel so SSE clients never see
// a silently truncated completion. Write failures (client already gone) are
// ignored — there is nobody left to tell.
func writeSSEErrorEvent(w http.ResponseWriter, flusher http.Flusher, canFlush bool) {
	writeSSEErrorEventMsg(w, flusher, canFlush,
		"upstream stream interrupted after the response started", "upstream_stream_interrupted")
}

// writeSSEErrorEventMsg is the parameterized form — the post-header terminal
// for every route: interrupted streams, surfaced fail-fast upstream errors,
// slot-queue timeouts. The message/code are JSON-escaped here so callers
// can carry status details safely.
func writeSSEErrorEventMsg(w http.ResponseWriter, flusher http.Flusher, canFlush bool, message, code string) {
	msgJSON, _ := json.Marshal(message)
	codeJSON, _ := json.Marshal(code)
	errEvent := `data: {"error":{"message":` + string(msgJSON) + `,"type":"server_error","code":` + string(codeJSON) + `}}` + "\n\n"
	if _, err := w.Write([]byte(errEvent)); err != nil {
		return
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if canFlush {
		flusher.Flush()
	}
}

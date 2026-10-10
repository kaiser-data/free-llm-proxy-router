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

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// UpstreamStreamError is returned by StreamProxy.Forward when the upstream
// answered a streaming request with a non-200 status. Nothing has been
// written to the client at that point, so the caller can classify the
// failure (see classifyUpstreamFailure) and either walk to the next
// candidate or surface the upstream response verbatim.
type UpstreamStreamError struct {
	ProviderID string
	StatusCode int
	Body       []byte
}

func (e *UpstreamStreamError) Error() string {
	return fmt.Sprintf("stream: provider %s status %d: %s", e.ProviderID, e.StatusCode, e.Body)
}

// StreamProxy transparently forwards a streaming (SSE) chat-completions
// response from a provider back to the client. It does not buffer the SSE
// body: every read from the upstream response is written downstream and
// flushed immediately, so events reach the client as the upstream produces
// them.
type StreamProxy struct {
	HTTPClient *http.Client
}

// Forward proxies a streaming chat-completions request to the given provider.
//
// Failure semantics — the streaming fallback contract:
//
//   - transport error or non-200 upstream status → nothing has been written
//     downstream; the error is returned for classification. Non-200 statuses
//     come back as *UpstreamStreamError: retryable classes (429/5xx/404/408)
//     let the caller try the next candidate; client errors (400/401/403, …)
//     are surfaced to the client verbatim.
//   - upstream 200 with an SSE Content-Type → the body is pumped through
//     byte-for-byte with per-read flushes. Once the pump has begun the
//     attempt is terminal: client write errors and upstream read errors are
//     logged and the function returns nil, so the caller never starts
//     another candidate after the stream began (that would concatenate two
//     unrelated generations).
//   - upstream 200 with a non-SSE Content-Type → the provider ignored
//     stream=true; the JSON body is buffered and forwarded as-is under its
//     own content type (the buffered behavior this streaming path replaces
//     for such providers). JSON is never disguised as an event stream.
//
// canonicalModel, when non-empty, is reported to the client in X-Used-Model
// (the canonical alias name); the raw upstream identity always goes into
// X-Free-Router-Upstream-Provider / X-Free-Router-Upstream-Model — the same
// router-owned header contract the buffered routes apply.
//
// Cancellation: ctx is the downstream request context. When the client
// disconnects the server cancels the context, the upstream request is
// aborted and the pump stops — no inference keeps running for a gone client.
func (sp *StreamProxy) Forward(ctx context.Context, w http.ResponseWriter, cfg config.ProviderConfig, body map[string]any, canonicalModel string) error {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"

	// Ensure stream is set
	body = copyMap(body)
	body["stream"] = true

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding stream request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("building stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	h, v := cfg.ResolvedAuth()
	if v != "" && v != "Bearer " {
		req.Header.Set(h, v)
	}
	for k, val := range cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}

	resp, err := sp.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return &UpstreamStreamError{ProviderID: cfg.ID, StatusCode: resp.StatusCode, Body: b}
	}

	if !isSSEContentType(resp.Header.Get("Content-Type")) {
		// Upstream answered 200 but not with an event stream — it ignored
		// stream=true. Buffer and forward the body as-is under its own
		// content type; never disguise JSON as an event stream.
		b, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return fmt.Errorf("stream: reading non-SSE 200 body from %s: %w", cfg.ID, rerr)
		}
		setStreamContractHeaders(w, canonicalModel, cfg.ID, body["model"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(b)
		return nil
	}

	// SSE pass-through. Router-owned contract headers are set together with
	// the SSE transport headers and the status, before any body byte flows.
	setStreamContractHeaders(w, canonicalModel, cfg.ID, body["model"])
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	// Byte-faithful pump: preserve the upstream's exact SSE framing (event
	// boundaries, CRLF variants, "data: [DONE]") — no line rewriting, no
	// re-chunking, no re-serialization. Flush after every read so events
	// reach the client as soon as the upstream produces them.
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client went away mid-stream. The stream has started —
				// returning an error would invite candidate walking after
				// the fact, which the fallback contract forbids.
				log.Printf("stream: client write error (stream aborted, no fallback after stream start): %v", werr)
				return nil
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF && !errors.Is(readErr, context.Canceled) {
				log.Printf("stream: upstream read error mid-stream (no fallback after stream start): %v", readErr)
			}
			return nil
		}
	}
}

// isSSEContentType reports whether a Content-Type header value marks an
// event stream.
func isSSEContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// setStreamContractHeaders applies the router-owned response headers shared
// by the streaming routes: the documented upstream-identity contract plus
// SSE transport headers. Upstream transport headers (Content-Length,
// Content-Encoding, Transfer-Encoding) are deliberately not copied — the
// proxy re-chunks the body to the client.
func setStreamContractHeaders(w http.ResponseWriter, canonicalModel, providerID string, upstreamModel any) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if canonicalModel != "" {
		w.Header().Set("X-Used-Model", canonicalModel)
	}
	w.Header().Set("X-Free-Router-Upstream-Provider", providerID)
	if m, ok := upstreamModel.(string); ok && m != "" {
		w.Header().Set("X-Free-Router-Upstream-Model", m)
	}
}

// streamOneCandidate runs one streaming attempt against provCfg and returns
// whether the attempt is terminal:
//
//	true  — the client response was written (SSE pass-through, non-SSE JSON
//	        passthrough, or a surfaced non-retryable upstream error), or the
//	        client canceled and no response is possible.
//	false — nothing was written downstream and the failure is retryable; the
//	        caller may try the next candidate.
//
// Fallback rule: fallback is only possible BEFORE the first downstream byte.
// Once the upstream answers 200 and the pump starts, Forward returns nil on
// every mid-stream failure, so no candidate is ever started after the stream
// began.
func (s *Server) streamOneCandidate(w http.ResponseWriter, r *http.Request, provCfg *config.ProviderConfig, body map[string]any, canonicalModel, clientModel, route string) bool {
	modelID, _ := body["model"].(string)
	sp := &StreamProxy{HTTPClient: s.streamHTTPClient()}
	err := sp.Forward(r.Context(), w, *provCfg, body, canonicalModel)
	if err == nil {
		log.Printf("%s: %s/%s streamed for %q", route, provCfg.ID, modelID, clientModel)
		return true
	}
	var use *UpstreamStreamError
	if errors.As(err, &use) {
		resp := &Response{StatusCode: use.StatusCode, Header: http.Header{}, Body: use.Body}
		resp.Header.Set("X-Free-Router-Upstream-Provider", provCfg.ID)
		if modelID != "" {
			resp.Header.Set("X-Free-Router-Upstream-Model", modelID)
		}
		if classifyUpstreamFailure(resp, nil) == nonRetryable {
			log.Printf("%s: %s/%s non-retryable status %d (stream) — surfacing to client, no further candidates", route, provCfg.ID, modelID, use.StatusCode)
			s.writeUpstreamError(w, clientModel, resp)
			return true
		}
		log.Printf("%s: %s/%s status %d (stream) — trying next candidate", route, provCfg.ID, modelID, use.StatusCode)
		return false
	}
	// Transport-level failure: timeout, connection failure, or a canceled
	// context (client disconnect). Only the canceled case aborts the walk —
	// the client is gone and no further attempt is meaningful.
	if classifyUpstreamFailure(nil, err) == nonRetryable {
		log.Printf("%s: %s/%s stream request canceled — aborting candidate walk", route, provCfg.ID, modelID)
		return true
	}
	log.Printf("%s: %s/%s stream error: %v — trying next candidate", route, provCfg.ID, modelID, err)
	return false
}

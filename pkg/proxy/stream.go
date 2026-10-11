package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// UpstreamStreamError is returned by StreamProxy.Forward when the upstream
// answered a streaming request with a non-200 status. Nothing has been
// written to the client at that point, so the caller can classify the
// failure (see ClassifyFailure) and either walk to the next candidate or
// surface the upstream response verbatim.
type UpstreamStreamError struct {
	ProviderID string
	StatusCode int
	Body       []byte
}

func (e *UpstreamStreamError) Error() string {
	return fmt.Sprintf("stream: provider %s status %d: %s", e.ProviderID, e.StatusCode, e.Body)
}

// StreamProxy transparently forwards a streaming (SSE) chat-completions
// response from a provider back to the client with the pre-commit hold
// (stream_chain.go): nothing reaches the client until a usable first event
// arrives, so every pre-commit failure — transport error, non-200 status,
// TTFT timeout, empty/error first event — is silent and the caller may
// retry or fall back.
type StreamProxy struct {
	HTTPClient        *http.Client
	FirstTokenTimeout time.Duration // 0 → 30s default
	IdleTimeout       time.Duration // 0 → 30s default
	RequestID         string        // X-Request-Id on commit (may be empty)
}

// Forward proxies a streaming chat-completions request to the given
// provider, delegating the hold/commit/pump machinery to streamUpstreamHeld
// (stream_chain.go).
//
// Failure semantics for the caller (streamOneCandidate):
//
//   - returned error before any client byte → classify and retry/fall back:
//     *UpstreamStreamError (non-200 status) or a transport error (connect
//     failure, TTFT timeout, empty/error first event, EOF before the first
//     token). A canceled context (client gone) also comes back as an error
//     and terminates the walk.
//   - nil → the stream was committed and finished; a mid-stream failure has
//     already ended in the OpenAI-style SSE error event + data: [DONE] —
//     terminal either way, the caller never starts another candidate after
//     the commit.
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
	// Ensure stream is set
	body = copyMap(body)
	body["stream"] = true

	p := streamClientParams{
		Client:            sp.HTTPClient,
		FirstTokenTimeout: sp.FirstTokenTimeout,
		IdleTimeout:       sp.IdleTimeout,
		RequestID:         sp.RequestID,
		// Direct/alias contract: X-Used-Model is the caller-computed
		// canonical identity (empty on the direct route — buffered-path
		// parity), not a canonicalization of the chunk's model field.
		CanonicalFor: func(_, _ string) string { return canonicalModel },
	}
	resp, committed, err := streamUpstreamHeld(ctx, w, cfg, body, p)
	if err != nil {
		return err
	}
	if committed {
		return nil
	}
	return &UpstreamStreamError{ProviderID: cfg.ID, StatusCode: resp.StatusCode, Body: resp.Body}
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
func (s *Server) streamOneCandidate(w http.ResponseWriter, r *http.Request, provCfg *config.ProviderConfig, body map[string]any, canonicalModel, clientModel, route, requestID string) bool {
	modelID, _ := body["model"].(string)
	sp := &StreamProxy{
		HTTPClient:        s.streamHTTPClient(),
		FirstTokenTimeout: s.firstTokenTimeout(),
		IdleTimeout:       s.streamIdleTimeout(),
		RequestID:         requestID,
	}
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
		// The same ClassifyFailure verdict every route applies (task H):
		// fail-fast defects surface verbatim; 401/403 cool the provider
		// down and the walk continues; other retryable classes walk on.
		if finalFailureDecision(resp, nil) == FailFast {
			log.Printf("%s: %s/%s fail-fast status %d (stream) — surfacing to client, no further candidates", route, provCfg.ID, modelID, use.StatusCode)
			s.writeUpstreamError(w, clientModel, resp)
			return true
		}
		if use.StatusCode == http.StatusUnauthorized || use.StatusCode == http.StatusForbidden {
			noteAuthFailure(s.rateLimiter, provCfg.ID, modelID, use.StatusCode)
		}
		log.Printf("%s: %s/%s status %d (stream) — trying next candidate", route, provCfg.ID, modelID, use.StatusCode)
		return false
	}
	// Transport-level failure: timeout, connection failure, or a canceled
	// context (client disconnect). Only the canceled case aborts the walk —
	// the client is gone and no further attempt is meaningful (the same
	// ClassifyFailure verdict every route applies, task H).
	if finalFailureDecision(nil, err) == FailFast {
		log.Printf("%s: %s/%s stream request canceled — aborting candidate walk", route, provCfg.ID, modelID)
		return true
	}
	log.Printf("%s: %s/%s stream error: %v — trying next candidate", route, provCfg.ID, modelID, err)
	return false
}

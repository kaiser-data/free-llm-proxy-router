package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// upstreamErrorClass describes whether a failed upstream attempt may be
// followed by another candidate, or must be surfaced to the client as-is.
type upstreamErrorClass int

const (
	// retryable means the failure is upstream-side (rate limit, server
	// error, timeout, connection failure): the next candidate may still
	// serve the request.
	retryable upstreamErrorClass = iota
	// nonRetryable means the client request itself was rejected: no other
	// upstream can succeed with the same payload, so walking more
	// candidates would only waste calls and mask the real error — possibly
	// behind a fake 200 exhaustion response.
	nonRetryable
)

// classifyUpstreamFailure decides whether a failed upstream attempt should
// abort the candidate walk (nonRetryable — surface the upstream response to
// the client verbatim) or continue to the next candidate (retryable).
//
// Classification:
//
//	400, 401, 403            → nonRetryable (client/request error)
//	other 4xx                → nonRetryable
//	404                      → retryable (see note below)
//	408                      → retryable (upstream request timeout)
//	429                      → retryable (rate limit)
//	5xx                      → retryable (server failure)
//	transport error          → retryable (timeout, connection failure)
//	context.Canceled         → nonRetryable (client is gone)
//
// 404 note: during a candidate walk an upstream 404 means "this provider
// does not serve this model" — per-model availability, not a client error.
// Other candidates are tried precisely because they may still have the
// model, and the existing provider semantics (MarkNeedsReverification +
// cooldown) already treat 404 as catalog staleness. A 404 caused by request
// semantics — a model that is neither in the catalog nor a canonical alias
// — never reaches an upstream: serveDirectModel answers it directly.
func classifyUpstreamFailure(resp *Response, err error) upstreamErrorClass {
	if err != nil {
		// Transport-level failure: timeout, connection refused, DNS or
		// TLS error, unexpected reset. All upstream-side and retryable.
		// The only exception is a canceled context — the client is gone
		// and no further attempt is meaningful.
		if errors.Is(err, context.Canceled) {
			return nonRetryable
		}
		return retryable
	}
	if resp == nil {
		return retryable
	}
	switch sc := resp.StatusCode; {
	case sc >= 200 && sc < 300:
		// Not a failure; callers handle success before classifying.
		return retryable
	case sc == http.StatusRequestTimeout, // 408
		sc == http.StatusTooManyRequests, // 429
		sc >= http.StatusInternalServerError: // 500, 502, 503, 504, …
		return retryable
	case sc == http.StatusNotFound: // 404 — see note above
		return retryable
	default:
		// 400, 401, 403, 405, 409, 413, 422, … — the request itself was
		// rejected. No other upstream can succeed with the same payload.
		return nonRetryable
	}
}

// writeUpstreamError forwards a non-retryable upstream error response to the
// client verbatim: the status code and body are preserved so the client sees
// the real validation/auth error instead of a masked success. Only
// router-owned headers are set — upstream transport headers describe the
// buffered body callProvider already consumed.
func (s *Server) writeUpstreamError(w http.ResponseWriter, canonicalModel string, resp *Response) {
	if v := resp.Header.Get("X-Free-Router-Upstream-Provider"); v != "" {
		w.Header().Set("X-Free-Router-Upstream-Provider", v)
	}
	if v := resp.Header.Get("X-Free-Router-Upstream-Model"); v != "" {
		w.Header().Set("X-Free-Router-Upstream-Model", v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Used-Model", canonicalModel)
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Body)
}

// writeExhaustionError answers the client when every candidate failed with a
// retryable failure (candidate exhaustion). It returns 502 Bad Gateway with
// an OpenAI-style JSON error body so clients can tell a proxy outage apart
// from a real completion — the previous behavior answered 200 with a canned
// "I'm temporarily unavailable" completion, which agents cannot distinguish
// from a genuine model answer (new gap NG1 in the scenario-matrix report).
func (s *Server) writeExhaustionError(w http.ResponseWriter, clientModel string, cause error) {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body.Error.Message = fmt.Sprintf("all provider candidates exhausted: %v", cause)
	body.Error.Type = "upstream_error"
	body.Error.Code = "all_candidates_exhausted"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Used-Model", clientModel)
	w.WriteHeader(http.StatusBadGateway)
	json.NewEncoder(w).Encode(body)
}

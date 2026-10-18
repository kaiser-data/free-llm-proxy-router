package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// The old two-way classifyUpstreamFailure (retryable/nonRetryable) was
// folded into ClassifyFailure (tasks C/H): every route now drives the same
// three-way classifier — retry_same / fallback / fail_fast — so a 401/403
// cools the provider down and walks on everywhere, and a 400 is split by
// subtype (context-length / modality vs payload defect) uniformly.

// writeUpstreamError forwards a fail-fast upstream error response to the
// client verbatim: the status code and body are preserved so the client sees
// the real validation/auth error instead of a masked success. Only
// router-owned headers are set — upstream transport headers describe the
// buffered body callProvider already consumed. When the keepalive header
// already went out on a streaming request (task P) the status is no longer
// free — the error becomes the structured SSE error event + [DONE] instead.
func (s *Server) writeUpstreamError(w http.ResponseWriter, canonicalModel string, resp *Response) {
	if kw := keepaliveOf(w); kw != nil && kw.HeaderSent() {
		flusher, canFlush := flusherOf(w)
		writeSSEErrorEventMsg(w, flusher, canFlush,
			fmt.Sprintf("upstream rejected the request with status %d after the response started", resp.StatusCode),
			"upstream_fail_fast_after_header")
		return
	}
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
// retryable failure (candidate exhaustion). It returns 503 Service
// Unavailable — the status that tells an OpenAI-compatible client "the
// server is temporarily unable to handle the request, retry later" — with a
// Retry-After hint in seconds from fallback.retry_after_seconds (default 5)
// and an OpenAI-style JSON error body whose type "server_error" is the type
// official SDKs branch on for server-side failures. The code
// "all_candidates_exhausted" stays machine-readable and X-Used-Model keeps
// the identity the client asked for. The original behavior answered 200 with
// a canned "I'm temporarily unavailable" completion, which agents cannot
// distinguish from a genuine model answer (gap NG1 in the scenario-matrix
// report); the first NG1 commit shipped this as 502, which this spec pass
// corrects to 503.
func (s *Server) writeExhaustionError(w http.ResponseWriter, clientModel string, cause error) {
	// Streaming request with the keepalive header already out (task P):
	// the status is no longer free — end with the structured SSE error
	// event + [DONE] instead of a 503 the wire can no longer carry.
	if kw := keepaliveOf(w); kw != nil && kw.HeaderSent() {
		flusher, canFlush := flusherOf(w)
		writeSSEErrorEventMsg(w, flusher, canFlush,
			fmt.Sprintf("all provider candidates exhausted: %v", cause),
			"all_candidates_exhausted")
		return
	}
	retryAfter := s.cfg.Load().Fallback.RetryAfterSeconds
	if retryAfter <= 0 {
		retryAfter = 5 // mirror the viper default for zeroed Config structs
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body.Error.Message = fmt.Sprintf("all provider candidates exhausted: %v", cause)
	body.Error.Type = "server_error"
	body.Error.Code = "all_candidates_exhausted"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Used-Model", clientModel)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(body)
}

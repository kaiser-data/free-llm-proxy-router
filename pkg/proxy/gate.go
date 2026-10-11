package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
)

// Capability gate BEFORE the slot queue (task Q): a request's candidates
// are filtered against what the upstream actually advertises — context
// window fit and tools/vision support ("ถ้า API ให้" — the API-provided
// capability enumeration is the only lack-signal; a model the API says
// nothing about is never rejected). An entry flagged NeedsReverification
// (NG8: set on a 429/404 of a "free" model) is never selected.
//
// When the filtered set is EMPTY the request must fail immediately with a
// clear error — it never enters the slot queue, because no amount of
// waiting makes a too-small context window grow.
//
// NG8 verdict: flagged entries were previously still selectable through the
// alias/pool ranking; the gate now excludes them everywhere selection
// happens (ranked walk, alias/pool candidates). A successful live refresh
// clears the flag (confirmed by the current :free list).

// capabilityDemand is what one request needs from its candidate.
type capabilityDemand struct {
	tools  bool
	vision bool
	// neededTokens is the estimated prompt + requested completion; 0 when
	// unknown (the context gate then never binds).
	neededTokens int
}

// demandOf reads the capability demands out of the incoming request:
// OpenAI-style tools/tool_choice/functions for tool calling, and
// image_url/input_image content parts for vision.
func demandOf(raw map[string]any, msgs []map[string]any, maxTokens int) capabilityDemand {
	d := capabilityDemand{neededTokens: maxTokens}
	if raw != nil {
		if t, ok := raw["tools"].([]any); ok && len(t) > 0 {
			d.tools = true
		}
		if _, ok := raw["tool_choice"]; ok {
			d.tools = true
		}
		if _, ok := raw["functions"]; ok {
			d.tools = true
		}
	}
	for _, m := range msgs {
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if _, is := pm["image_url"]; is {
				d.vision = true
			}
			if t, is := pm["type"].(string); is && (t == "image_url" || t == "input_image") {
				d.vision = true
			}
		}
	}
	return d
}

// metaStrings flattens a metadata string-list that may arrive either as
// []string (built in-process by a scanner) or []any (decoded from JSON).
func metaStrings(v any) []string {
	switch ts := v.(type) {
	case []string:
		return ts
	case []any:
		out := make([]string, 0, len(ts))
		for _, item := range ts {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// entryPassesGate reports whether the candidate may serve the request.
// NG8 exclusion is unconditional; context fit uses positive metadata only
// (ContextWindow > 0); tools/vision rejections require the API's
// capability enumeration (supported_endpoint_types).
func entryPassesGate(e catalog.CatalogEntry, d capabilityDemand) bool {
	if e.NeedsReverification {
		return false // NG8: a flagged model is not selected
	}
	if d.neededTokens > 0 && e.ContextWindow > 0 && d.neededTokens > e.ContextWindow {
		return false // positive knowledge: cannot hold the request
	}
	if !d.tools && !d.vision {
		return true
	}
	ets := metaStrings(e.Metadata["supported_endpoint_types"])
	if len(ets) == 0 {
		return true // the API did not enumerate capabilities — keep
	}
	hasTools, hasVision := false, false
	for _, t := range ets {
		lt := strings.ToLower(t)
		switch {
		case strings.Contains(lt, "tool"), strings.Contains(lt, "function"):
			hasTools = true
		case strings.Contains(lt, "vision"), strings.Contains(lt, "image"):
			hasVision = true
		}
	}
	if d.tools && !hasTools {
		return false
	}
	if d.vision && !hasVision {
		return false
	}
	return true
}

// gateEntries filters a candidate list through the gate.
func gateEntries(entries []catalog.CatalogEntry, d capabilityDemand) []catalog.CatalogEntry {
	out := make([]catalog.CatalogEntry, 0, len(entries))
	for _, e := range entries {
		if entryPassesGate(e, d) {
			out = append(out, e)
		}
	}
	return out
}

// writeNoCapableCandidate answers the request that no surviving candidate
// can serve — immediately, with a clear OpenAI-style error, and never via
// the slot queue (task Q).
func (s *Server) writeNoCapableCandidate(w http.ResponseWriter, clientModel string, d capabilityDemand) {
	why := "no candidate model can serve this request"
	if d.neededTokens > 0 {
		why = fmt.Sprintf("no candidate model can serve this request (needed ~%d tokens incl. completion)", d.neededTokens)
	}
	if d.tools {
		why += " or does not support tools"
	}
	if d.vision {
		why += " or does not support vision"
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body.Error.Message = why
	body.Error.Type = "invalid_request_error"
	body.Error.Code = "no_capable_candidate"
	if kw := keepaliveOf(w); kw != nil && kw.HeaderSent() {
		flusher, canFlush := flusherOf(w)
		writeSSEErrorEventMsg(w, flusher, canFlush, why, "no_capable_candidate")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Used-Model", clientModel)
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(body)
}

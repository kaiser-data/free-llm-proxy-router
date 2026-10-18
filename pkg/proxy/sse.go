package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// writeCompletionAsSSE re-emits a buffered (non-streaming) chat completion as an
// OpenAI-style SSE stream. The fallback chain always buffers provider responses
// so it can fail over; clients that asked for stream:true still expect
// text/event-stream, so we synthesize the chunks from the final completion.
//
// If body is not a parseable chat completion it is written as plain JSON.
func writeCompletionAsSSE(w http.ResponseWriter, body []byte) {
	var c struct {
		ID                string           `json:"id"`
		Created           int64            `json:"created"`
		Model             string           `json:"model"`
		SystemFingerprint string           `json:"system_fingerprint,omitempty"`
		Choices           []map[string]any `json:"choices"`
		Usage             map[string]any   `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &c); err != nil || len(c.Choices) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return
	}
	if c.ID == "" {
		c.ID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if c.Created == 0 {
		c.Created = time.Now().Unix()
	}

	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	emit := func(choices []map[string]any, usage map[string]any) {
		chunk := map[string]any{
			"id":      c.ID,
			"object":  "chat.completion.chunk",
			"created": c.Created,
			"model":   c.Model,
			"choices": choices,
		}
		if c.SystemFingerprint != "" {
			chunk["system_fingerprint"] = c.SystemFingerprint
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// One content chunk per choice, then one finish chunk per choice.
	for i, ch := range c.Choices {
		idx := choiceIndex(ch, i)
		msg, _ := ch["message"].(map[string]any)
		delta := map[string]any{"role": "assistant"}
		if content, ok := msg["content"]; ok && content != nil {
			delta["content"] = content
		} else {
			delta["content"] = ""
		}
		if rc, ok := msg["reasoning_content"]; ok && rc != nil {
			delta["reasoning_content"] = rc
		}
		if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
			// Streaming tool-call deltas carry an explicit index.
			indexed := make([]any, 0, len(calls))
			for j, tc := range calls {
				if m, ok := tc.(map[string]any); ok {
					m = copyMap(m)
					m["index"] = j
					indexed = append(indexed, m)
				}
			}
			delta["tool_calls"] = indexed
		}
		emit([]map[string]any{{"index": idx, "delta": delta, "finish_reason": nil}}, nil)
	}
	for i, ch := range c.Choices {
		finish := ch["finish_reason"]
		if finish == nil {
			finish = "stop"
		}
		var usage map[string]any
		if i == len(c.Choices)-1 {
			usage = c.Usage
		}
		emit([]map[string]any{{"index": choiceIndex(ch, i), "delta": map[string]any{}, "finish_reason": finish}}, usage)
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func choiceIndex(ch map[string]any, fallback int) int {
	if v, ok := ch["index"].(float64); ok {
		return int(v)
	}
	return fallback
}

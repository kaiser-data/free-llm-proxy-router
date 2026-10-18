package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// parseSSE returns the decoded data payloads and whether [DONE] terminated the stream.
func parseSSE(t *testing.T, raw string) ([]map[string]any, bool) {
	t.Helper()
	var chunks []map[string]any
	done := false
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("invalid chunk JSON %q: %v", payload, err)
		}
		chunks = append(chunks, m)
	}
	return chunks, done
}

func TestWriteCompletionAsSSE_Content(t *testing.T) {
	body := `{"id":"abc","object":"chat.completion","created":123,"model":"llama-3.3-70b",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	rec := httptest.NewRecorder()
	writeCompletionAsSSE(rec, []byte(body))

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	chunks, done := parseSSE(t, rec.Body.String())
	if !done {
		t.Fatal("stream not terminated with [DONE]")
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}

	var content strings.Builder
	var finish any
	for _, c := range chunks {
		if c["object"] != "chat.completion.chunk" || c["id"] != "abc" || c["model"] != "llama-3.3-70b" {
			t.Errorf("bad chunk envelope: %v", c)
		}
		ch := c["choices"].([]any)[0].(map[string]any)
		if s, ok := ch["delta"].(map[string]any)["content"].(string); ok {
			content.WriteString(s)
		}
		if ch["finish_reason"] != nil {
			finish = ch["finish_reason"]
		}
	}
	if content.String() != "Hello there" {
		t.Errorf("reassembled content = %q", content.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %v, want stop", finish)
	}
	if chunks[1]["usage"] == nil {
		t.Error("usage missing from final chunk")
	}
}

func TestWriteCompletionAsSSE_ToolCallsIndexed(t *testing.T) {
	body := `{"id":"t","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},
		              {"id":"c2","type":"function","function":{"name":"b","arguments":"{}"}}]},
		"finish_reason":"tool_calls"}]}`
	rec := httptest.NewRecorder()
	writeCompletionAsSSE(rec, []byte(body))

	chunks, _ := parseSSE(t, rec.Body.String())
	delta := chunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	calls, ok := delta["tool_calls"].([]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("tool_calls = %v", delta["tool_calls"])
	}
	for i, c := range calls {
		if idx := c.(map[string]any)["index"]; idx != float64(i) {
			t.Errorf("tool_calls[%d].index = %v", i, idx)
		}
	}
	last := chunks[len(chunks)-1]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", last["finish_reason"])
	}
}

func TestWriteCompletionAsSSE_NonCompletionPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCompletionAsSSE(rec, []byte(`not json`))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json fallback", ct)
	}
	if rec.Body.String() != "not json" {
		t.Errorf("body altered: %q", rec.Body.String())
	}
}

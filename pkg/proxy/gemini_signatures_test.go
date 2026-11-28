package proxy

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

const signedToolCallResponse = `{
  "choices": [
    {
      "message": {
        "role": "assistant",
        "tool_calls": [
          {
            "id": "call-signed",
            "type": "function",
            "extra_content": {"google": {"thought_signature": "c2lnMQ=="}},
            "function": {"name": "get_weather", "arguments": "{}"}
          },
          {
            "id": "call-unsigned-sibling",
            "type": "function",
            "function": {"name": "get_time", "arguments": "{}"}
          }
        ]
      }
    }
  ]
}`

func TestCaptureGeminiThoughtSignatures(t *testing.T) {
	cache := newThoughtSignatureCache()
	if n := captureGeminiThoughtSignatures([]byte(signedToolCallResponse), cache); n != 1 {
		t.Fatalf("captured = %d, want 1 (only the signed call)", n)
	}
	if sig, ok := cache.get("call-signed"); !ok || sig != "c2lnMQ==" {
		t.Errorf("call-signed = %q ok=%v, want cached signature", sig, ok)
	}
	if sig, ok := cache.get("call-unsigned-sibling"); !ok || sig != "" {
		t.Errorf("call-unsigned-sibling = %q ok=%v, want known-unsigned (\"\", true)", sig, ok)
	}
	if _, ok := cache.get("never-seen"); ok {
		t.Errorf("never-seen id reported as known")
	}
}

func TestCaptureGeminiMessageLevelSignatureOnly(t *testing.T) {
	// Real shape observed 2026-11-10: text-only answers carry the signature
	// on the message, not on tool calls. Capture must ignore it, not panic.
	body := `{"choices":[{"message":{"role":"assistant","content":"OK",
	  "extra_content":{"google":{"thought_signature":"bXNn"}}}}]}`
	cache := newThoughtSignatureCache()
	if n := captureGeminiThoughtSignatures([]byte(body), cache); n != 0 {
		t.Errorf("captured = %d, want 0 (no tool calls)", n)
	}
}

func TestCaptureGeminiMalformedBody(t *testing.T) {
	cache := newThoughtSignatureCache()
	if n := captureGeminiThoughtSignatures([]byte("{not json"), cache); n != 0 {
		t.Errorf("captured = %d on malformed body, want 0", n)
	}
	if n := captureGeminiThoughtSignatures(nil, cache); n != 0 {
		t.Errorf("captured = %d on nil body, want 0", n)
	}
	if n := captureGeminiThoughtSignatures([]byte("{}"), nil); n != 0 {
		t.Errorf("captured = %d with nil cache, want 0", n)
	}
}

// unsignedHistory is an opencode-style replay: assistant tool_calls without
// the extra_content field (stripped by the client).
const unsignedHistory = `{
  "messages": [
    {"role": "user", "content": "weather in Paris?"},
    {"role": "assistant", "tool_calls": [
      {"id": "call-known", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}},
      {"id": "call-sibling", "type": "function", "function": {"name": "get_time", "arguments": "{}"}},
      {"id": "call-unknown", "type": "function", "function": {"name": "read_file", "arguments": "{}"}},
      {"id": "call-client-signed", "type": "function",
       "extra_content": {"google": {"thought_signature": "a2VwdA=="}},
       "function": {"name": "grep", "arguments": "{}"}}
    ]},
    {"role": "tool", "tool_call_id": "call-known", "content": "18C sunny"}
  ]
}`

func mustMessages(t *testing.T, s string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(s), &body); err != nil {
		t.Fatalf("unmarshalling body: %v", err)
	}
	return body
}

func toolCall(t *testing.T, body map[string]any, index int) map[string]any {
	t.Helper()
	msgs := body["messages"].([]any)
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok || index >= len(calls) {
			continue
		}
		if tc, ok := calls[index].(map[string]any); ok {
			return tc
		}
	}
	t.Fatalf("no assistant tool_calls[%d] in body", index)
	return nil
}

func sigOf(t *testing.T, tc map[string]any) (string, bool) {
	t.Helper()
	extra, ok := tc["extra_content"].(map[string]any)
	if !ok {
		return "", false
	}
	google, ok := extra["google"].(map[string]any)
	if !ok {
		return "", false
	}
	sig, _ := google["thought_signature"].(string)
	return sig, sig != ""
}

func TestInjectGeminiThoughtSignatures(t *testing.T) {
	cache := newThoughtSignatureCache()
	cache.set("call-known", "cmVhbA==")
	cache.set("call-sibling", "") // parallel sibling: must stay unsigned

	body := mustMessages(t, unsignedHistory)
	fromCache, sentinel := injectGeminiThoughtSignatures(body, cache)
	if fromCache != 1 || sentinel != 1 {
		t.Fatalf("fromCache, sentinel = %d, %d; want 1, 1", fromCache, sentinel)
	}

	if sig, ok := sigOf(t, toolCall(t, body, 0)); !ok || sig != "cmVhbA==" {
		t.Errorf("call-known: sig = %q ok=%v, want cached signature", sig, ok)
	}
	if _, ok := sigOf(t, toolCall(t, body, 1)); ok {
		t.Errorf("call-sibling: got a signature, want left unsigned")
	}
	if sig, ok := sigOf(t, toolCall(t, body, 2)); !ok || sig != skipThoughtSignatureValidator {
		t.Errorf("call-unknown: sig = %q ok=%v, want sentinel", sig, ok)
	}
	if sig, ok := sigOf(t, toolCall(t, body, 3)); !ok || sig != "a2VwdA==" {
		t.Errorf("call-client-signed: sig = %q ok=%v, want client value untouched", sig, ok)
	}
}

func TestInjectDoesNotMutateOriginalMessages(t *testing.T) {
	cache := newThoughtSignatureCache()
	cache.set("call-known", "cmVhbA==")

	raw := mustMessages(t, unsignedHistory)
	body := copyMap(raw) // what buildBody starts from (shallow)
	injectGeminiThoughtSignatures(body, cache)

	// The rebuilt body carries signatures…
	if sig, ok := sigOf(t, toolCall(t, body, 0)); !ok || sig != "cmVhbA==" {
		t.Fatalf("body call-known: sig = %q ok=%v, want injected", sig, ok)
	}
	// …but the original request map stays clean, so attempts against other
	// providers are unaffected.
	if _, ok := sigOf(t, toolCall(t, raw, 0)); ok {
		t.Errorf("original raw request was mutated by injection")
	}
	if _, ok := sigOf(t, toolCall(t, raw, 2)); ok {
		t.Errorf("original raw request was mutated by sentinel injection")
	}
}

func TestInjectKeepsSiblingExtraContentFields(t *testing.T) {
	cache := newThoughtSignatureCache()
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{
						"id":   "call-x",
						"type": "function",
						// extra_content exists with other fields, no signature.
						"extra_content": map[string]any{"other": map[string]any{"keep": true}},
						"function":      map[string]any{"name": "f", "arguments": "{}"},
					},
				},
			},
		},
	}
	injectGeminiThoughtSignatures(body, cache)
	tc := toolCall(t, body, 0)
	sig, ok := sigOf(t, tc)
	if !ok || sig != skipThoughtSignatureValidator {
		t.Fatalf("sig = %q ok=%v, want sentinel", sig, ok)
	}
	extra := tc["extra_content"].(map[string]any)
	if other, ok := extra["other"].(map[string]any); !ok || other["keep"] != true {
		t.Errorf("sibling extra_content field clobbered: %v", extra)
	}
}

func TestInjectNoMessagesOrEmptyToolCalls(t *testing.T) {
	cache := newThoughtSignatureCache()
	if fromCache, sentinel := injectGeminiThoughtSignatures(map[string]any{}, cache); fromCache != 0 || sentinel != 0 {
		t.Errorf("no messages: got %d, %d; want 0, 0", fromCache, sentinel)
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}}
	if fromCache, sentinel := injectGeminiThoughtSignatures(body, cache); fromCache != 0 || sentinel != 0 {
		t.Errorf("no tool calls: got %d, %d; want 0, 0", fromCache, sentinel)
	}
}

func TestBuildBodyInjectsOnlyForGemini(t *testing.T) {
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "gemini", Enabled: true, APIKey: "k"},
		{ID: "groq", Enabled: true, APIKey: "k"},
	}}
	fc := &FallbackChain{Cfg: cfg, Signatures: newThoughtSignatureCache()}

	geminiBody := fc.buildBody("gemini", mustMessages(t, unsignedHistory), "gemini-3.5-flash")
	if sig, ok := sigOf(t, toolCall(t, geminiBody, 2)); !ok || sig != skipThoughtSignatureValidator {
		t.Errorf("gemini body: call-unknown sig = %q ok=%v, want sentinel", sig, ok)
	}

	// The same request routed at another provider must stay untouched.
	groqBody := fc.buildBody("groq", mustMessages(t, unsignedHistory), "llama-3.3-70b")
	if _, ok := sigOf(t, toolCall(t, groqBody, 2)); ok {
		t.Errorf("groq body: unexpected thought signature injected")
	}
}

func TestThoughtSignatureCacheEviction(t *testing.T) {
	cache := newThoughtSignatureCache()
	for i := 0; i < thoughtSignatureCacheMax+10; i++ {
		cache.set(fmt.Sprintf("call-%d", i), "s")
	}
	// The 10 oldest ids were evicted FIFO; the cap holds.
	if got := len(cache.store); got != thoughtSignatureCacheMax {
		t.Errorf("cache holds %d entries, want cap %d", got, thoughtSignatureCacheMax)
	}
	if _, ok := cache.get("call-0"); ok {
		t.Errorf("oldest entry survived eviction")
	}
	if _, ok := cache.get(fmt.Sprintf("call-%d", thoughtSignatureCacheMax+9)); !ok {
		t.Errorf("newest entry was evicted")
	}
}

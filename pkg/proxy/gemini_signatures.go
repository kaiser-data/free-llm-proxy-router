package proxy

import (
	"encoding/json"
	"sync"
)

// Provider: Gemini — thought signatures (OpenAI-compatible endpoint)
//
// Gemini 3.x models attach an opaque, encrypted thought signature to the tool
// calls they emit, at a non-standard location on the OpenAI wire format:
//
//	choices[].message.tool_calls[].extra_content.google.thought_signature
//
// When that tool call is replayed in conversation history (the standard agent
// loop: model calls a tool, client sends back the result), Gemini requires the
// signature to come back verbatim. OpenAI-format clients (opencode, VS Code
// BYOK, the OpenAI SDKs) strip the non-standard extra_content field when they
// rebuild history, so every continuation of a tool-using session dies with:
//
//	400 "Function call is missing a thought_signature in functionCall parts."
//
// The proxy sits between the two and speaks both dialects, so it can keep the
// signatures alive: capture them from Gemini responses keyed by tool-call id,
// re-inject them on later Gemini requests, and inject Google's documented
// bypass sentinel for tool calls it never saw (history that predates the
// cache, or tool calls that another provider produced). LiteLLM and other
// routers use the same sentinel for history originating from non-Gemini
// sources.
//
// For parallel tool calls Gemini signs only the first tool call; the sibling
// calls must be replayed unsigned, so the cache also records "seen unsigned"
// ids and leaves them bare instead of sentinel-stamping them.
//
// Docs: https://ai.google.dev/gemini-api/docs/thought-signatures
// Observed on the OpenAI-compat endpoint 2026-11-10.

// skipThoughtSignatureValidator is Google's documented bypass sentinel for
// replayed function-call history that has no signature to return.
const skipThoughtSignatureValidator = "skip_thought_signature_validator"

// thoughtSignatureCacheMax bounds memory: ids are short, signatures are
// ~1-2 KB of base64. 5000 entries is a few MB at worst.
const thoughtSignatureCacheMax = 5000

// thoughtSignatureCache maps tool-call id → signature. An empty value means
// "seen without a signature" (parallel-call sibling) and must stay unsigned.
// It lives on the Server so signatures survive across requests; it is
// deliberately not persisted — after a restart the first continuation of an
// old session uses the sentinel until the cache refills.
type thoughtSignatureCache struct {
	mu    sync.Mutex
	store map[string]string
	order []string // insertion order, for FIFO eviction past the cap
}

func newThoughtSignatureCache() *thoughtSignatureCache {
	return &thoughtSignatureCache{store: make(map[string]string)}
}

// set records the signature for a tool-call id ("" = known unsigned).
func (c *thoughtSignatureCache) set(id, sig string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.store[id]; !exists {
		c.order = append(c.order, id)
	}
	c.store[id] = sig
	for len(c.order) > thoughtSignatureCacheMax {
		delete(c.store, c.order[0])
		c.order = c.order[1:]
	}
}

// get returns the signature for a tool-call id; ok is false when the id was
// never seen.
func (c *thoughtSignatureCache) get(id string) (sig string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sig, ok = c.store[id]
	return
}

// captureGeminiThoughtSignatures parses a Gemini chat completion and caches
// the signature of every tool call it carries, plus ids that arrived
// unsigned (parallel-call siblings). Returns the number of real signatures
// captured. Malformed bodies are ignored — capture must never break a
// response it cannot parse.
func captureGeminiThoughtSignatures(body []byte, cache *thoughtSignatureCache) int {
	if cache == nil {
		return 0
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
					ExtraContent struct {
						Google struct {
							ThoughtSignature string `json:"thought_signature"`
						} `json:"google"`
					} `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return 0
	}
	n := 0
	for _, ch := range parsed.Choices {
		for _, tc := range ch.Message.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if tc.ExtraContent.Google.ThoughtSignature != "" {
				cache.set(tc.ID, tc.ExtraContent.Google.ThoughtSignature)
				n++
			} else {
				cache.set(tc.ID, "") // parallel sibling: replay unsigned
			}
		}
	}
	return n
}

// injectGeminiThoughtSignatures ensures every assistant tool call about to be
// replayed to Gemini carries a thought signature:
//
//   - already signed by the client → untouched
//   - signature cached from an earlier Gemini response → injected
//   - id seen unsigned (parallel-call sibling) → left unsigned
//   - never seen (older history, or a tool call another provider made) →
//     Google's bypass sentinel
//
// Messages are rebuilt rather than mutated, so the client's original request
// map stays clean for attempts against other providers. Returns the number of
// cached and sentinel injections.
func injectGeminiThoughtSignatures(body map[string]any, cache *thoughtSignatureCache) (fromCache, sentinel int) {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return 0, 0
	}
	changed := false
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = m
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		patched := make([]any, len(calls))
		msgChanged := false
		for j, tc := range calls {
			patched[j] = tc
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			if hasThoughtSignature(tcm) {
				continue // client replayed it; leave untouched
			}
			id, _ := tcm["id"].(string)
			sig, known := "", false
			if cache != nil && id != "" {
				sig, known = cache.get(id)
			}
			if known && sig == "" {
				continue // unsigned on purpose (parallel sibling)
			}
			if !known {
				sig = skipThoughtSignatureValidator
				sentinel++
			} else {
				fromCache++
			}
			copied := copyMap(tcm)
			setThoughtSignature(copied, sig)
			patched[j] = copied
			msgChanged = true
		}
		if msgChanged {
			copiedMsg := copyMap(msg)
			copiedMsg["tool_calls"] = patched
			out[i] = copiedMsg
			changed = true
		}
	}
	if changed {
		body["messages"] = out
	}
	return fromCache, sentinel
}

// hasThoughtSignature reports whether a tool call already carries a Gemini
// thought signature at extra_content.google.thought_signature.
func hasThoughtSignature(toolCall map[string]any) bool {
	extra, ok := toolCall["extra_content"].(map[string]any)
	if !ok {
		return false
	}
	google, ok := extra["google"].(map[string]any)
	if !ok {
		return false
	}
	sig, _ := google["thought_signature"].(string)
	return sig != ""
}

// setThoughtSignature writes sig at extra_content.google.thought_signature,
// building fresh nested maps so no sibling field of an existing
// extra_content object is clobbered or shared with the original.
func setThoughtSignature(toolCall map[string]any, sig string) {
	extra := map[string]any{}
	if orig, ok := toolCall["extra_content"].(map[string]any); ok {
		for k, v := range orig {
			extra[k] = v
		}
	}
	google := map[string]any{}
	if orig, ok := extra["google"].(map[string]any); ok {
		for k, v := range orig {
			google[k] = v
		}
	}
	google["thought_signature"] = sig
	extra["google"] = google
	toolCall["extra_content"] = extra
}

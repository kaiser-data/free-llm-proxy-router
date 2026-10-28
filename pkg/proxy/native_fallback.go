// Package proxy implements the OpenAI-compatible HTTP proxy.
package proxy

import (
	"encoding/json"
	"strings"
)

// Provider: OpenRouter — native fallback integration
// Docs: https://openrouter.ai/docs/guides/routing/model-fallbacks
//       https://openrouter.ai/docs/guides/routing/routers/free-models-router
// Last verified: 2026-02-28
// Native features: models[] array fallback, openrouter/free router

// Provider: Groq — service_tier is sent only when native_features.service_tier
// is set in config. Free-tier orgs get a 400 for "auto" (verified 2026-10-28).
// Docs: https://console.groq.com/docs/api-reference

// BuildOpenRouterRequest returns a modified request body that uses OpenRouter's
// native models[] fallback array. OpenRouter tries each model in order,
// so the proxy only needs to send one request for intra-provider fallback.
// The actual model used is returned in the response body "model" field.
func BuildOpenRouterRequest(req map[string]any, freeModels []string) map[string]any {
	if len(freeModels) == 0 {
		return req
	}
	body := copyMap(req)
	body["model"] = freeModels[0]
	body["models"] = freeModels // OpenRouter tries these in order
	delete(body, "stream")      // non-streaming so fallback chain can parse the JSON response
	return body
}

// OpenRouterFreeRouterRequest builds a request using the "openrouter/free"
// ultimate fallback model. OpenRouter picks any available free model.
func OpenRouterFreeRouterRequest(req map[string]any) map[string]any {
	body := copyMap(req)
	body["model"] = "openrouter/free"
	delete(body, "models")  // clear any previous models[] array
	delete(body, "stream")  // non-streaming for consistent JSON response
	return body
}

// copyMap shallow-copies a map[string]any.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Provider: Gemini — OpenAI-compatible endpoint
// Thinking models (e.g. Gemma 4) return their reasoning inline in the message
// content as "<thought>…</thought>answer" and flag it with
// extra_content.google.thought (observed 2026-10-28).

// StripGeminiThoughts removes leading <thought>…</thought> blocks from each
// choice's message content so clients receive only the answer, as with any
// other OpenAI-compatible provider. Bodies that don't parse are returned as-is.
func StripGeminiThoughts(body []byte) []byte {
	if !strings.Contains(string(body), "<thought>") {
		return body
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return body
	}
	choices, _ := parsed["choices"].([]any)
	changed := false
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimLeft(content, " \n")
		for strings.HasPrefix(trimmed, "<thought>") {
			end := strings.Index(trimmed, "</thought>")
			if end < 0 {
				break
			}
			trimmed = strings.TrimLeft(trimmed[end+len("</thought>"):], " \n")
		}
		if trimmed != content {
			msg["content"] = trimmed
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return out
}

// Package proxy implements the OpenAI-compatible HTTP proxy.
package proxy

// Provider: OpenRouter — native fallback integration
// Docs: https://openrouter.ai/docs/guides/routing/model-fallbacks
//       https://openrouter.ai/docs/guides/routing/routers/free-models-router
// Last verified: 2026-02-28
// Native features: models[] array fallback, openrouter/free router

// Provider: Groq — service_tier: auto
// Docs: https://console.groq.com/docs/api-reference
// Last verified: 2026-02-28
// Native features: service_tier field

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
	return body
}

// OpenRouterFreeRouterRequest builds a request using the "openrouter/free"
// ultimate fallback model. OpenRouter picks any available free model.
func OpenRouterFreeRouterRequest(req map[string]any) map[string]any {
	body := copyMap(req)
	body["model"] = "openrouter/free"
	delete(body, "models") // clear any previous models[] array
	return body
}

// BuildGroqRequest injects service_tier: "auto" to maximise Groq's available capacity.
func BuildGroqRequest(req map[string]any) map[string]any {
	body := copyMap(req)
	body["service_tier"] = "auto"
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

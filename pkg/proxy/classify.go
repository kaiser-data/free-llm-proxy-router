package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// failureVerdict is what one failed upstream answer means for the walk.
// classifyFailure decides it from the response alone; callOnce applies it.
// Keeping the decision pure means a new status or body pattern is a change
// here plus a table row in classify_test.go, and the retry loop stays as is.
type failureVerdict struct {
	// outcome is outModelFail (try the provider's next model) or
	// outProviderFail (skip the provider for this request).
	outcome outcome
	// modelCooldown, when > 0, cools providerID/modelID for that long.
	modelCooldown time.Duration
	// countFailure records a reliability failure for the provider.
	countFailure bool
	// coolIfUnreliable additionally gives the provider a short cooldown once
	// its reliability drops below 50%. Only meaningful with countFailure.
	coolIfUnreliable bool
	// reason is the log text after "fallback: provider/model ".
	reason string
}

// unreliableProviderCooldown is the short provider cooldown that
// coolIfUnreliable applies.
const unreliableProviderCooldown = 30 * time.Second

// classifyFailure classifies a non-200 answer, or a transport failure
// (resp == nil). A 429 is outModelFail with no cooldown here: attempt
// cools the key slot and tries the provider's next key.
func classifyFailure(resp *Response) failureVerdict {
	if resp == nil {
		// Transport-level failure (timeout, conn refused): not evidence the
		// whole provider is down — one hung model must not stop the walk.
		// Cool the model briefly and continue with its siblings; reliability
		// still records the failure and may short-cool the provider.
		return failureVerdict{
			outcome: outModelFail, modelCooldown: transportModelCooldown,
			countFailure: true, coolIfUnreliable: true,
			reason: fmt.Sprintf("transport error — model cooldown %s, walking on", transportModelCooldown),
		}
	}
	status := resp.StatusCode
	switch {
	case isKeyRejection(resp):
		// The key itself was rejected (invalid or revoked): every model of
		// this provider would fail the same way — skip the provider.
		return failureVerdict{
			outcome: outProviderFail, countFailure: true, coolIfUnreliable: true,
			reason: "key rejected — skipping provider",
		}
	case status == http.StatusForbidden && isConsent403(resp.Body):
		// Provider: UnoRouter (verified 2026-11-20): a 403 consent_required
		// means the account has not consented to this model's upstream
		// provider yet (unorouter.com/consents). The key is fine and models
		// behind already-consented providers still answer, so this cools the
		// model only — cooling the key would hide them all.
		return failureVerdict{
			outcome: outModelFail, modelCooldown: consentCooldown,
			reason: fmt.Sprintf("needs upstream consent (403 consent_required) — model cooldown %s, trying next model", consentCooldown),
		}
	case status == http.StatusForbidden && isUpstreamCredit403(resp.Body):
		// Provider: UnoRouter (verified 2026-11-20): "This provider ran out
		// of credit on their side. That is on us, not you: retry and the
		// request goes to another provider." Upstream capacity, not the key
		// — a short model cooldown like a 5xx instead of cooling the key.
		return failureVerdict{
			outcome: outModelFail, modelCooldown: serverErrorCooldown, countFailure: true,
			reason: fmt.Sprintf("status 403 — upstream out of credit, model cooldown %s, trying next model", serverErrorCooldown),
		}
	case status == http.StatusBadRequest && isModelSpecific400(resp.Body):
		// The 400 blames the model (decommissioned, "only supports
		// Interactions API"): it sits out, and the provider's next model can
		// answer this very request.
		return failureVerdict{
			outcome: outModelFail, modelCooldown: badRequestCooldown,
			reason: fmt.Sprintf("unusable (400) — model cooldown %s, trying next model", badRequestCooldown),
		}
	case isRequestRejection(resp):
		// The provider rejects this request's shape, size or content (a
		// moderation 403); its other models most likely will too, so skip the
		// provider for this request. Not a health problem: no reliability
		// penalty, no cooldown.
		return failureVerdict{
			outcome: outProviderFail,
			reason:  fmt.Sprintf("status %d — provider rejects this request, skipping provider", status),
		}
	case status == http.StatusNotFound:
		return failureVerdict{
			outcome: outModelFail, modelCooldown: missingModelCooldown,
			reason: fmt.Sprintf("not found — model cooldown %s", missingModelCooldown),
		}
	case status >= 500:
		// An overloaded or failing model: let it rest briefly so concurrent
		// requests stop spending attempts on it, and count it against the
		// provider (a provider failing every model gets a short cooldown).
		return failureVerdict{
			outcome: outModelFail, modelCooldown: serverErrorCooldown,
			countFailure: true, coolIfUnreliable: true,
			reason: fmt.Sprintf("status %d — model cooldown %s, trying next model", status, serverErrorCooldown),
		}
	case status == http.StatusTooManyRequests:
		return failureVerdict{outcome: outModelFail, reason: "rate limited"}
	default:
		// Free tiers often fail one model (quota, retired, overloaded) while
		// the provider's other models still answer.
		return failureVerdict{
			outcome: outModelFail,
			reason:  fmt.Sprintf("status %d — trying next model", status),
		}
	}
}

// isRequestRejection reports whether the provider rejected this request
// itself (its shape, size or content) rather than being unavailable: a 400
// that does not name the model, 413, 422, or a moderation 403.
func isRequestRejection(resp *Response) bool {
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return !isModelSpecific400(resp.Body)
	case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	case http.StatusForbidden:
		return isModeration403(resp.Body)
	}
	return false
}

// isKeyRejection reports whether a response rejects the API key (401, or a
// 403 that is not about the request's content, its consent gate, or the
// upstream provider's own credit).
func isKeyRejection(resp *Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized ||
		(resp.StatusCode == http.StatusForbidden && !isModeration403(resp.Body) && !isConsent403(resp.Body) && !isUpstreamCredit403(resp.Body))
}

// isConsent403 reports whether a 403 asks for the account's consent to an
// upstream provider (Provider: UnoRouter, verified 2026-11-20:
// `{"error":{"code":"consent_required","message":"…needs your consent for
// these providers: …"}}`). The key is valid — cooling it down would hide
// every model behind already-consented providers.
func isConsent403(body []byte) bool {
	return bytes.Contains(body, []byte("consent_required"))
}

// isUpstreamCredit403 reports whether a 403 says the upstream provider ran
// out of credit (Provider: UnoRouter, verified 2026-11-20: "This provider
// ran out of credit on their side. That is on us, not you"). The key is
// valid and the failure is upstream capacity, so it is transient like a 5xx.
func isUpstreamCredit403(body []byte) bool {
	return bytes.Contains(body, []byte("ran out of credit"))
}

// isModeration403 reports whether a 403 blocks the request's content
// (OpenRouter: "…requires moderation and your input was flagged") rather than
// the key. Retrying it on the next key or cooling keys down cannot help.
func isModeration403(body []byte) bool {
	b := strings.ToLower(string(body))
	return strings.Contains(b, "moderation") || strings.Contains(b, "flagged")
}

// modelSpecific400 are phrases in a 400 body that blame the model itself, not
// the request. Most 400s blame the request (context too long, tools or images
// the model can't take, a bad parameter); cooling the model down for those
// would let one client's request take the model from everyone for an hour.
// So the list is deliberately narrow: a missed phrase costs one wasted call on
// a later request, a wrong one costs an hour of capacity.
var modelSpecific400 = []string{
	"decommissioned",
	"model_not_found",
	"model not found",
	"does not exist or you do not have access",
	"is not a valid model",
	"unknown model",
	"interactions api",               // Gemini: "This model only supports Interactions API."
	"only supported in v1/responses", // OpenAI: model needs the Responses API
	"not a chat model",
}

func isModelSpecific400(body []byte) bool {
	b := strings.ToLower(string(body))
	for _, p := range modelSpecific400 {
		if strings.Contains(b, p) {
			return true
		}
	}
	return false
}

// isZeroQuota reports whether a 429 body says the quota for this model is zero
// (Gemini returns "limit: 0" for models outside the free tier).
func isZeroQuota(body []byte) bool {
	return bytes.Contains(body, []byte("limit: 0,"))
}

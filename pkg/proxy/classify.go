package proxy

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FailureDecision is the three-way verdict for a failed upstream attempt —
// the state machine the retry ladder follows:
//
//	RetrySame    retry the SAME provider/model (bounded by
//	             fallback.retries_per_candidate and the max_attempts budget)
//	FallbackNext this candidate is done — move to the next one
//	FailFast     surface the failure and stop the walk; no retry and no
//	             fallback can fix it
type FailureDecision int

const (
	RetrySame FailureDecision = iota
	FallbackNext
	FailFast
)

func (d FailureDecision) String() string {
	switch d {
	case RetrySame:
		return "retry_same"
	case FallbackNext:
		return "fallback"
	case FailFast:
		return "fail_fast"
	default:
		return "unknown"
	}
}

// FailureInput carries everything the pure classifier needs to decide. It is
// deliberately free of proxy state so tests can drive every row of the
// decision table directly.
type FailureInput struct {
	// StatusCode is the upstream HTTP status; 0 when the attempt failed on
	// the transport (no response at all).
	StatusCode int
	// Err is the transport error, if any.
	Err error
	// Body is the upstream error body (used for 400-subtype detection).
	Body []byte
	// RetriesDone counts same-candidate retries already spent on this
	// candidate.
	RetriesDone int
	// MaxRetries is the clamped fallback.retries_per_candidate.
	MaxRetries int
	// RetryOn429 / RetryOn5xx mirror fallback.retry_on_429 / retry_on_5xx.
	RetryOn429 bool
	RetryOn5xx bool
	// RetryAfter is the parsed Retry-After value of the failure response
	// (429/503); 0 when absent.
	RetryAfter time.Duration
	// RemainingBudget is the time left before the request's total deadline
	// fires; 0 means "no deadline / unknown" and never blocks a retry.
	RemainingBudget time.Duration
	// TimeoutsDone counts timeout failures already seen on this candidate —
	// only the FIRST timeout earns a same-candidate retry; a second
	// consecutive timeout says the provider is too slow for this request.
	TimeoutsDone int
}

// cooldownSetter is the minimal cooldown surface the auth-failure handler
// needs; *ratelimit.GlobalTracker satisfies it.
type cooldownSetter interface {
	SetCooldown(providerID string, until time.Time)
}

// authCooldownDuration is how long a provider stays on cooldown after a
// 401/403: the credential was rejected, so retrying sooner is pointless.
const authCooldownDuration = 10 * time.Minute

// noteAuthFailure applies the 401/403 contract shared by every route
// (task H): the provider goes on cooldown and a warn-level log names the
// candidate and the cause — making a dead or rotated key obvious in ops
// output without grepping for status codes. The log line never contains the
// API key or the upstream body: 401/403 bodies can echo credentials, so only
// the status is reported.
func noteAuthFailure(setter cooldownSetter, providerID, modelID string, status int) {
	if setter != nil {
		setter.SetCooldown(providerID, time.Now().Add(authCooldownDuration))
	}
	candidate := providerID
	if modelID != "" {
		candidate += "/" + modelID
	}
	log.Printf("WARN auth failure: candidate=%s status=%d cause=credential rejected by provider (check the API key) — provider on cooldown %v, falling back to next candidate", candidate, status, authCooldownDuration)
}

// ClassifyFailure decides what the retry ladder does with one failed
// upstream attempt.
//
//	transport ECONNRESET/EOF/refused before any response byte → RetrySame
//	  while retries remain, then FallbackNext
//	timeout (per-attempt or client cap)                        → RetrySame on
//	  the FIRST timeout (gated by retry_on_5xx), FallbackNext after
//	context.Canceled (client gone)                             → FailFast
//	408                                                        → RetrySame
//	  while retries remain, then FallbackNext
//	429                                                        → RetrySame
//	  (gated by retry_on_429) unless the Retry-After exceeds the remaining
//	  request budget — sleeping it off would blow the deadline, so the walk
//	  falls back instead of waiting
//	404                                                        → FallbackNext
//	  (model gone for this provider — per-model availability, not a request
//	  defect; the walk marks the entry for reverification)
//	401/403                                                    → FallbackNext
//	  (the provider is cooled down for 10 minutes; other candidates may
//	  still serve the request)
//	413                                                        → FailFast
//	400 content-policy refusal                                 → FailFast
//	400 context-length / unsupported-modality                  → FallbackNext
//	  when detectable from the body, FailFast otherwise
//	400 anything else (genuine payload defect)                 → FailFast
//	5xx incl. 502/503/504/529                                  → RetrySame
//	  (gated by retry_on_5xx) while retries remain, then FallbackNext
//	other 4xx                                                  → FailFast
func ClassifyFailure(in FailureInput) FailureDecision {
	if in.Err != nil {
		if errors.Is(in.Err, context.Canceled) {
			return FailFast // client is gone — nothing left to answer
		}
		if isTimeoutErr(in.Err) {
			if in.TimeoutsDone == 0 && in.RetryOn5xx && in.RetriesDone < in.MaxRetries {
				return RetrySame
			}
			return FallbackNext
		}
		// Connection reset / unexpected EOF / refused / TLS failure before
		// any response byte: transient upstream-side failure.
		if in.RetriesDone < in.MaxRetries {
			return RetrySame
		}
		return FallbackNext
	}
	if in.StatusCode == 0 {
		return FallbackNext // defensive: no status and no error — try onward
	}
	switch sc := in.StatusCode; {
	case sc >= 200 && sc < 300:
		// Not a failure; callers handle success before classifying.
		return RetrySame
	case sc == http.StatusTooManyRequests:
		if !in.RetryOn429 {
			return FallbackNext
		}
		// A Retry-After longer than the remaining request budget means the
		// backoff sleep would blow the deadline — fall back instead of
		// sleeping past the budget.
		if in.RetryAfter > 0 && in.RemainingBudget > 0 && in.RetryAfter >= in.RemainingBudget {
			return FallbackNext
		}
		if in.RetriesDone < in.MaxRetries {
			return RetrySame
		}
		return FallbackNext
	case sc == http.StatusRequestTimeout: // 408 — upstream request timeout
		if in.RetriesDone < in.MaxRetries {
			return RetrySame
		}
		return FallbackNext
	case sc == http.StatusNotFound:
		return FallbackNext // model gone for this provider — try the next one
	case sc == http.StatusUnauthorized, sc == http.StatusForbidden:
		return FallbackNext // auth failure — cooldown applies; others may serve
	case sc == http.StatusRequestEntityTooLarge:
		return FailFast // 413 — shrinking the payload is the client's job
	case sc == http.StatusBadRequest:
		return classifyBadRequest(in.Body)
	case sc >= http.StatusInternalServerError:
		// 500, 502, 503, 504, 529 (Cloudflare overloaded), …
		if in.RetryOn5xx && in.RetriesDone < in.MaxRetries {
			if in.RetryAfter > 0 && in.RemainingBudget > 0 && in.RetryAfter >= in.RemainingBudget {
				return FallbackNext
			}
			return RetrySame
		}
		return FallbackNext
	default:
		// 402, 405, 409, 422, … — the request itself was rejected.
		return FailFast
	}
}

// classifyBadRequest splits the 400 class by what the upstream said
// (task I). The split lives ONLY here — every route shares it through
// ClassifyFailure:
//
//	content-policy refusal          → FailFast (no provider may serve it)
//	context-length overflow         → FallbackNext (a bigger window can)
//	unsupported image/audio/video   → FallbackNext (a multimodal model can)
//	anything else — a genuine       → FailFast (no candidate can fix the
//	  payload defect (malformed       payload; an unknown 400 never
//	  fields, bad types, unknown      becomes a silent fallback)
//	  params)
//
// Detection is a best-effort lowercase substring match of the upstream error
// body against the marker tables below — the single source of truth, with
// the per-provider message shapes documented on each table. Anything not
// confidently recognized defaults to FailFast so a broken payload is never
// masked behind an exhaustion response.
func classifyBadRequest(body []byte) FailureDecision {
	if containsAnyMarker(body, contentPolicyMarkers) {
		return FailFast
	}
	if containsAnyMarker(body, contextLengthMarkers) || containsAnyMarker(body, modalityMarkers) {
		return FallbackNext
	}
	return FailFast
}

// contentPolicyMarkers detect upstream refusals driven by usage policy.
var contentPolicyMarkers = []string{
	"content_policy", "content policy",
	"content_filter", "content filter", "content filtering",
}

// contextLengthMarkers detect "prompt too long for this model" errors — the
// one 400 subclass another candidate can genuinely fix (a model with a
// bigger context window). Matched as lowercase substrings of the error
// body; shapes per provider family:
//
//	OpenAI-style text (OpenRouter passthrough, Groq, Cerebras, NVIDIA NIM,
//	  vLLM): "This model's maximum context length is 8192 tokens. However,
//	  you requested … Please reduce the length of the messages or
//	  completion."
//	OpenRouter code style: {"error":{"code":"context_length_exceeded",…}}
//	Anthropic (raw metadata surfaced via OpenRouter): "prompt is too long:
//	  5000 tokens > 4096 maximum"
//	Gemini: "The input token count (50000) exceeds the maximum number of
//	  tokens allowed (32768)."
//	HuggingFace TGI: "Input validation error: `inputs` must have less than
//	  4096 tokens. Given: 5000"
var contextLengthMarkers = []string{
	"context length", "context_length", "context window",
	"maximum context", "max context",
	"exceeds the maximum", // Gemini: …exceeds the maximum number of tokens allowed
	"too many tokens", "too many input tokens", "tokens exceeded", "token limit",
	"prompt is too long", "prompt too long", // Anthropic
	"reduce the length", // OpenAI-style closing sentence
	"input is too long", "input too long",
	"must have less than", // HuggingFace TGI: `inputs` must have less than N tokens
}

// modalityMarkers detect "this model does not accept image/audio/video
// input" — same reasoning as context length: another candidate may accept
// the modality. Deliberately PHRASE-level: bare substrings like "images",
// "vision", "modality", or "image_url" also appear inside model IDs
// ("…-vision-instruct") and in malformed-field validation errors that no
// candidate can fix, which would misroute genuine payload defects into the
// fallback path (task I).
var modalityMarkers = []string{
	"image input", "audio input", "video input",
	"does not support image", "does not support audio", "does not support video",
	"multimodal", "unsupported modality",
}

func containsAnyMarker(body []byte, markers []string) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isTimeoutErr reports whether err is a timeout: either a net.Error with
// Timeout()=true (the http.Client cap) or a context deadline (the
// per-provider WithTimeout in callProvider). context.Canceled is NOT a
// timeout and must be checked first by the caller.
func isTimeoutErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// parseRetryAfter parses a Retry-After header value — delta-seconds or an
// HTTP-date. Missing, invalid, or already elapsed values return 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// remainingBudget reports the time left before ctx's deadline fires.
// 0 means "no deadline / unknown" — never used to block a retry.
func remainingBudget(ctx context.Context) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	if d := time.Until(dl); d > 0 {
		return d
	}
	return 0
}

// finalFailureDecision classifies a candidate's LAST failed attempt at walk
// level: same-candidate retries are already spent, so the verdict collapses
// to "surface and stop" (FailFast) or "next candidate" (FallbackNext). The
// retry flags stay false on purpose — they gate only the same-candidate
// retry, never the walk.
func finalFailureDecision(resp *Response, err error) FailureDecision {
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	return ClassifyFailure(FailureInput{
		StatusCode:  status,
		Err:         err,
		Body:        respBodyOf(resp),
		RetriesDone: 1, // ≥ MaxRetries(0): no RetrySame possible
	})
}

func respBodyOf(resp *Response) []byte {
	if resp == nil {
		return nil
	}
	return resp.Body
}

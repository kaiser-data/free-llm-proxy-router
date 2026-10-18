package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// The emptiness contract (round 11, task W) — the invariant:
//
//	a client must NEVER receive a completion that is empty, no matter what.
//
// An upstream "result" is EMPTY — the attempt failed and the retry ladder /
// fallback loop owns it — when, before any commit:
//
//   - the body is blank, or not parseable completion JSON at all;
//   - `choices` is missing or empty;
//   - every choice carries no usable signal: text content is
//     null / "" / whitespace-only (string or content-parts array), AND
//     there are no valid tool_calls;
//   - the result ends with finish_reason=stop/length (any finish_reason,
//     really) while producing no content and no valid tool_calls;
//   - usage.completion_tokens is present and 0 while nothing usable exists;
//   - the result is REASONING-ONLY — reasoning_content / reasoning carry
//     text but there is no content and no valid tool_calls. Reasoning is
//     never a "usable event" on its own: it does not commit. It still
//     counts as a working signal (the stream hold extends the TTFT
//     watchdog, bounded by the hard caps). With
//     fallback.commit_on_reasoning=true the reasoning-only result is
//     delivered verbatim instead of failing the attempt.
//
// A VALID tool_call makes the result usable: the tool_calls value parses as
// a non-empty JSON array whose first entry carries a non-empty function name
// (streaming deltas continue with bare argument chunks, so only the FIRST
// usable event decides — a bare-arguments opening delta keeps the hold).
//
// finish_reason=content_filter is NOT "empty": it is a fail-fast refusal —
// the payload was rejected, more upstreams cannot fix it, so the walk stops
// and the client gets a clear error (never a 200 with a filtered body
// disguised as a completion).

// errContentFilter marks a 200 whose finish_reason is content_filter — the
// fail-fast refusal (task W). Returned from callProvider; the walk surfaces
// it as a clear error envelope instead of masking it.
var errContentFilter = errors.New("upstream refused the prompt (finish_reason=content_filter)")

// emptyCompletionResult is the verdict of inspectCompletion.
type emptyCompletionResult struct {
	// empty is true when the body carries no usable completion.
	empty bool
	// reason names the emptiness form ("" when not empty).
	reason string
	// contentFilter is true when the body refused with finish_reason=
	// content_filter — fail-fast, distinct from empty.
	contentFilter bool
	// reasoning is true when reasoning text was present (with or without
	// other usable signal) — the stream hold uses it as a working signal.
	reasoning bool
}

// inspectCompletion judges a buffered 200 chat-completion body against the
// emptiness contract. Never empty when a usable signal exists — text content
// or a valid tool_call wins over a weird usage block.
func inspectCompletion(body []byte, commitOnReasoning bool) emptyCompletionResult {
	if len(bytes.TrimSpace(body)) == 0 {
		return emptyCompletionResult{empty: true, reason: "blank_body"}
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
			} `json:"message"`
			Delta struct {
				Content          json.RawMessage `json:"content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens any `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A 200 that is not completion JSON at all (an HTML error page, a
		// login wall) cannot serve a client — empty.
		return emptyCompletionResult{empty: true, reason: "unparseable_body"}
	}
	if len(parsed.Choices) == 0 {
		return emptyCompletionResult{empty: true, reason: "choices_empty"}
	}
	res := emptyCompletionResult{}
	usable := false
	anyReasoning := false
	finish := ""
	for _, c := range parsed.Choices {
		if c.FinishReason != "" && finish == "" {
			finish = c.FinishReason
		}
		if validToolCalls(c.Message.ToolCalls) || validToolCalls(c.Delta.ToolCalls) {
			usable = true
		}
		if contentHasText(c.Message.Content) || contentHasText(c.Delta.Content) {
			usable = true
		}
		if reasoningHasText(c.Message.ReasoningContent) || reasoningHasText(c.Message.Reasoning) ||
			reasoningHasText(c.Delta.ReasoningContent) || reasoningHasText(c.Delta.Reasoning) {
			anyReasoning = true
		}
	}
	res.reasoning = anyReasoning
	if usable {
		return res // a real answer exists — never empty regardless of usage
	}
	if finish == "content_filter" {
		res.contentFilter = true
		return res
	}
	if anyReasoning {
		if !commitOnReasoning {
			// Reasoning-only: the model "thought" but produced nothing
			// usable — empty, unless fallback.commit_on_reasoning delivers
			// the reasoning-only result verbatim (then: not empty).
			res.empty, res.reason = true, "reasoning_only"
		}
		return res
	}
	if n, ok := completionTokensOf(parsed.Usage.CompletionTokens); ok && n == 0 {
		res.empty, res.reason = true, "completion_tokens_0"
		return res
	}
	res.empty, res.reason = true, "no_content"
	return res
}

// reasoningHasText reports whether a reasoning JSON value carries text —
// reasoning_content / reasoning are streamed as strings; a parts array also
// counts. Any other non-null structure counts as reasoning signal.
func reasoningHasText(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		for _, p := range parts {
			if strings.TrimSpace(p.Text) != "" {
				return true
			}
		}
		return false
	}
	return true
}

// validToolCalls reports whether a tool_calls JSON value is a usable tool
// call: a non-empty array whose first entry carries a non-empty function
// name. An empty array or a bare continuation delta (arguments chunk with no
// name) does not commit an attempt by itself.
func validToolCalls(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var calls []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return false
	}
	return len(calls) > 0 && calls[0].Function.Name != ""
}

// completionTokensOf reads usage.completion_tokens when present as a JSON
// number. Absent/other types return (0, false) — a missing usage block is
// NOT emptiness evidence on its own.
func completionTokensOf(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

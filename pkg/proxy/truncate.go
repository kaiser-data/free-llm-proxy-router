package proxy

import (
	"log"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// Context truncation (round 10) — the payload half of the latency fix.
// Fragile free upstreams queue hard on oversized prompts (the incident:
// a ~8.2k-token prompt answered with a 45.7s TTFT), so before a request
// leaves for one, the chat history is sliding-windowed down to the most
// recent turns and a still-oversized single message is trimmed head+tail
// with an explicit marker.
//
// Hard rules:
//   - The capability gate ranks the FULL prompt (context-window fit is
//     decided on the real demand); truncation only trims what is SENT.
//   - Tool-calling requests are never truncated: dropping older turns
//     breaks tool_call/tool pairing upstream and turns a slow request
//     into a broken one.
//   - The caller's original messages (shared with req.Raw through the
//     shallow copyMap) are never mutated in place — truncation replaces
//     body["messages"] with a fresh slice and copies every map it edits.
const (
	// truncationMarker is spliced between the head and the tail of a
	// trimmed message so the model can see the cut explicitly.
	truncationMarker = "…[context truncated by proxy]…"

	// truncationMinMessageChars is the smallest head+tail a message may
	// be trimmed to. A fully gutted message is worse than an
	// over-ceiling prompt — trimming stops here.
	truncationMinMessageChars = 400

	// truncationDefaultCeiling / truncationDefaultTurns mirror the viper
	// defaults in pkg/config for zeroed FallbackConfigs (tests): ceiling
	// 8000 tokens, keep the last 6 turns.
	truncationDefaultCeiling = 8000
	truncationDefaultTurns   = 6
)

// applyContextTruncation rewrites body["messages"] under the
// fallback.context_truncation policy. Returns true when the body changed.
// Reads the config per call, so a hot reload takes effect immediately.
func applyContextTruncation(cfg *config.Config, body map[string]any) bool {
	if cfg == nil || body == nil {
		return false
	}
	tc := cfg.Fallback.ContextTruncation
	if !tc.Enabled {
		return false
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	if requestCarriesToolProtocol(body, msgs) {
		return false
	}
	ceiling := tc.PromptTokenCeiling
	if ceiling <= 0 {
		ceiling = truncationDefaultCeiling
	}
	keepTurns := tc.KeepRecentTurns
	if keepTurns <= 0 {
		keepTurns = truncationDefaultTurns
	}
	before := estimateTokensAny(msgs)
	if before <= ceiling {
		return false
	}
	kept, changed := windowMessages(msgs, keepTurns)
	if est := estimateTokensAny(kept); est > ceiling {
		var trimmed bool
		kept, trimmed = trimOversizedMessages(kept, ceiling)
		changed = changed || trimmed
	}
	if !changed {
		return false
	}
	after := estimateTokensAny(kept)
	body["messages"] = kept
	log.Printf("payload: chat history truncated est %d→%d tokens (ceiling %d, kept %d of %d messages)", before, after, ceiling, len(kept), len(msgs))
	return true
}

// requestCarriesToolProtocol reports whether the request uses a
// tool-calling wire shape — top-level tools/tool_choice/functions, a
// tool-role message, or an assistant message with tool_calls. Truncation
// refuses these.
func requestCarriesToolProtocol(body map[string]any, msgs []any) bool {
	if t, ok := body["tools"].([]any); ok && len(t) > 0 {
		return true
	}
	if _, ok := body["tool_choice"]; ok {
		return true
	}
	if _, ok := body["functions"]; ok {
		return true
	}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "tool" {
			return true
		}
		if _, has := m["tool_calls"]; has {
			return true
		}
	}
	return false
}

// windowMessages keeps every system message plus the most recent keepTurns
// non-system messages, preserving the original order. When nothing is
// dropped the original slice is returned with changed=false.
func windowMessages(msgs []any, keepTurns int) ([]any, bool) {
	turnIdx := make([]int, 0, len(msgs))
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue // not a message map — keep it like a system entry
		}
		if role, _ := m["role"].(string); role != "system" {
			turnIdx = append(turnIdx, i)
		}
	}
	if len(turnIdx) <= keepTurns {
		return msgs, false
	}
	drop := make(map[int]bool, len(turnIdx)-keepTurns)
	for _, i := range turnIdx[:len(turnIdx)-keepTurns] {
		drop[i] = true
	}
	kept := make([]any, 0, len(msgs)-len(drop))
	for i, raw := range msgs {
		if drop[i] {
			continue
		}
		kept = append(kept, raw)
	}
	return kept, true
}

// trimOversizedMessages repeatedly head+tail-trims the longest
// string-content message until the estimate fits the ceiling or nothing
// trimmable is left. Every edited message map is copied first — the input
// slice's maps are shared with the caller's request.
func trimOversizedMessages(msgs []any, ceiling int) ([]any, bool) {
	const charsPerToken = 4
	anyTrimmed := false
	// The loop is bounded: each pass either reaches the ceiling, runs out
	// of trimmable messages, or hits the no-progress guard below.
	for pass := 0; pass <= len(msgs); pass++ {
		if estimateTokensAny(msgs) <= ceiling {
			return msgs, anyTrimmed
		}
		longest := -1
		longestLen := 0
		for i, raw := range msgs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			s, ok := m["content"].(string)
			if !ok || len(s) <= truncationMinMessageChars+len(truncationMarker) {
				continue
			}
			if len(s) > longestLen {
				longest, longestLen = i, len(s)
			}
		}
		if longest < 0 {
			return msgs, anyTrimmed // nothing left worth trimming
		}
		// Budget: shrink the message so the total estimate lands under
		// the ceiling, bounded below by the floor size. The marker counts
		// INSIDE the budget — otherwise each pass would re-add it and the
		// size would oscillate forever just above the ceiling.
		over := estimateTokensAny(msgs) - ceiling
		budgetChars := longestLen - over*charsPerToken
		if min := truncationMinMessageChars; budgetChars < min {
			budgetChars = min
		}
		head := (budgetChars - len(truncationMarker)) * 2 / 3
		tail := budgetChars - len(truncationMarker) - head
		if head <= 0 || tail <= 0 || head+len(truncationMarker)+tail >= longestLen {
			// No progress possible (already at the floor): keep the
			// prompt over the ceiling rather than gutting the message.
			return msgs, anyTrimmed
		}
		src := msgs[longest].(map[string]any)["content"].(string)
		msgs[longest] = copyMessageWithContent(msgs[longest],
			src[:head]+truncationMarker+src[len(src)-tail:])
		anyTrimmed = true
	}
	return msgs, anyTrimmed
}

// copyMessageWithContent clones a message map and swaps its string content.
// The original map (shared with req.Raw) is left untouched.
func copyMessageWithContent(msg any, content string) map[string]any {
	m, ok := msg.(map[string]any)
	if !ok {
		return map[string]any{"content": content}
	}
	cp := make(map[string]any, len(m)+1)
	for k, v := range m {
		cp[k] = v
	}
	cp["content"] = content
	return cp
}

// estimateTokensAny gives a rough token count for a decoded message list
// (4 chars ≈ 1 token — the same rule estimateTokens applies to typed
// messages). String content counts; array content counts its text parts.
func estimateTokensAny(msgs []any) int {
	total := 0
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			total += len(c) / 4
		case []any:
			for _, part := range c {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := pm["text"].(string); ok {
					total += len(t) / 4
				}
			}
		}
	}
	return total
}

package proxy

import (
	"strings"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// truncTestConfig returns a config with truncation enabled at test-sized
// knobs.
func truncTestConfig(ceiling, keepTurns int) *config.Config {
	cfg := &config.Config{}
	cfg.Fallback.ContextTruncation.Enabled = true
	cfg.Fallback.ContextTruncation.PromptTokenCeiling = ceiling
	cfg.Fallback.ContextTruncation.KeepRecentTurns = keepTurns
	return cfg
}

func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func roles(msgs []any) []string {
	out := make([]string, 0, len(msgs))
	for _, raw := range msgs {
		m := raw.(map[string]any)
		out = append(out, m["role"].(string))
	}
	return out
}

func TestTruncateNoopUnderCeiling(t *testing.T) {
	body := map[string]any{"messages": []any{msg("user", "hi")}}
	if applyContextTruncation(truncTestConfig(8000, 6), body) {
		t.Fatal("truncated a prompt under the ceiling")
	}
}

func TestTruncateDisabledNoop(t *testing.T) {
	cfg := truncTestConfig(100, 6)
	cfg.Fallback.ContextTruncation.Enabled = false
	body := map[string]any{"messages": []any{msg("user", strings.Repeat("x", 10000))}}
	if applyContextTruncation(cfg, body) {
		t.Fatal("truncated with the feature disabled")
	}
}

func TestTruncateWindowKeepsSystemAndRecentTurns(t *testing.T) {
	cfg := truncTestConfig(1000, 4)
	msgs := []any{msg("system", "You are helpful.")}
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, msg(role, strings.Repeat("word ", 30))) // ~7 tokens each → 20 turns ≈ 150 tokens
	}
	// The estimate is way over the 1000 ceiling? No — make it over:
	// rebuild with bigger messages so the window actually fires.
	msgs = []any{msg("system", "You are helpful.")}
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, msg(role, strings.Repeat("w", 400))) // 100 tokens/turn → 2000 est
	}
	body := map[string]any{"messages": msgs}
	if !applyContextTruncation(cfg, body) {
		t.Fatal("over-ceiling history was not truncated")
	}
	kept := body["messages"].([]any)
	// system + last 4 turns
	if len(kept) != 5 {
		t.Fatalf("kept %d messages, want 5 (system + 4 recent turns)", len(kept))
	}
	if roles(kept)[0] != "system" {
		t.Fatalf("system message dropped: %v", roles(kept))
	}
	// The last turn must survive, in order.
	if roles(kept)[4] != "assistant" {
		t.Fatalf("recent turns out of order or wrong: %v", roles(kept))
	}
}

func TestTruncateSkipsToolRequests(t *testing.T) {
	cfg := truncTestConfig(1000, 4)
	big := strings.Repeat("x", 10000)
	for name, body := range map[string]map[string]any{
		"top-level tools": {
			"tools":    []any{map[string]any{"type": "function"}},
			"messages": []any{msg("user", big)},
		},
		"tool role": {
			"messages": []any{msg("user", big), map[string]any{"role": "tool", "content": "result"}},
		},
		"assistant tool_calls": {
			"messages": []any{msg("user", big), map[string]any{"role": "assistant", "tool_calls": []any{}}},
		},
	} {
		before := len(body["messages"].([]any))
		if applyContextTruncation(cfg, body) {
			t.Fatalf("%s: tool-calling request was truncated", name)
		}
		if len(body["messages"].([]any)) != before {
			t.Fatalf("%s: messages changed", name)
		}
	}
}

func TestTruncateTrimsOversizedSingleMessage(t *testing.T) {
	cfg := truncTestConfig(1000, 6) // 4000 chars
	original := strings.Repeat("a", 40000)
	m := msg("user", original)
	body := map[string]any{"messages": []any{m}}
	if !applyContextTruncation(cfg, body) {
		t.Fatal("oversized single message was not trimmed")
	}
	kept := body["messages"].([]any)
	if len(kept) != 1 {
		t.Fatalf("message dropped by trimming: %d", len(kept))
	}
	content := kept[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, truncationMarker) {
		t.Fatal("trimmed content carries no truncation marker")
	}
	// Head+tail: starts and ends with the original text.
	if !strings.HasPrefix(content, "aaa") || !strings.HasSuffix(content, "aaa") {
		t.Fatal("head/tail not preserved")
	}
	// The ORIGINAL map (shared with req.Raw) must be untouched.
	if m["content"].(string) != original {
		t.Fatal("caller's message map was mutated in place")
	}
}

// TestBuildBodyAppliesTruncation pins the wiring: the ranked walk's
// buildBody trims the outgoing payload when the policy is enabled.
func TestBuildBodyAppliesTruncation(t *testing.T) {
	fc := &FallbackChain{Cfg: truncTestConfig(1000, 6)}
	raw := map[string]any{
		"model":    "m",
		"messages": []any{msg("user", strings.Repeat("w", 40000))},
	}
	body := fc.buildBody("prov", raw, "upstream-model")
	if !applyContextTruncationChanged(body) {
		t.Fatal("buildBody did not trim the oversized history")
	}
	// The raw request (and its shared messages slice) is untouched.
	if len(raw["messages"].([]any)) != 1 {
		t.Fatal("raw request messages changed")
	}
}

// applyContextTruncationChanged reports whether body's messages were
// trimmed by comparing the estimate against the input size.
func applyContextTruncationChanged(body map[string]any) bool {
	msgs := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	return len(content) < 40000
}

package scan

import (
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

func TestIsNonChatModel(t *testing.T) {
	nonChat := []string{
		"whisper-large-v3-turbo",
		"meta-llama/llama-prompt-guard-2-22m",
		"meta-llama/llama-guard-4-12b",
		"openai/gpt-oss-safeguard-20b",
		"canopylabs/orpheus-v1-english",
		"playai-tts",
		"nvidia/nv-embedqa-e5-v5",
		"gemini-nano-banana-2.1",
		"gemini-3-pro-image",
		"lyria-3.5",
		"gemini-3.8-flash-tts",
		"gemini-3.5-transcribe",
		"gemini-robotics-er-2-preview",
		"gemini-2.5-computer-use-preview-10-2025",
		"deep-research-preview-04-2026",
		"antigravity-preview-latest",
	}
	for _, id := range nonChat {
		if !isNonChatModel(id) {
			t.Errorf("isNonChatModel(%q) = false, want true", id)
		}
	}
	chat := []string{
		"openai/gpt-oss-120b",
		"llama-3.3-70b-versatile",
		"allam-2-7b",
		"qwen/qwen3.8-27b",
		"gemini-2.5-flash",
		"gemini-3.5-flash",
		"gemma-4-31b-it",
	}
	for _, id := range chat {
		if isNonChatModel(id) {
			t.Errorf("isNonChatModel(%q) = true, want false", id)
		}
	}
}

func TestFreeByDiscovery(t *testing.T) {
	kiloPaid := map[string]any{
		"id":     "anthropic/claude-opus-5.5",
		"isFree": false,
	}
	kiloFree := map[string]any{
		"id":     "kilo-auto/free",
		"isFree": true,
	}
	zenPaid := map[string]any{
		"id":   "gpt-5.5",
		"cost": map[string]any{"text": map[string]any{"input": map[string]any{"price": 5.0}}},
	}
	zenFree := map[string]any{
		"id":   "big-pickle",
		"cost": map[string]any{"text": map[string]any{"input": map[string]any{"price": 0.0}}},
	}

	t.Run("no_knobs_all_free", func(t *testing.T) {
		cfg := config.ProviderConfig{ID: "x"}
		if !freeByDiscovery(cfg, "any-model", kiloPaid) {
			t.Error("without discovery knobs every model is eligible (historical behavior)")
		}
	})

	t.Run("free_detect_field_dot_path", func(t *testing.T) {
		cfg := config.ProviderConfig{ID: "kilo", Discovery: config.DiscoveryConfig{
			FreeDetectField: "isFree",
			FreeDetectValue: "true",
		}}
		if !freeByDiscovery(cfg, "kilo-auto/free", kiloFree) {
			t.Error("isFree=true model should pass")
		}
		if freeByDiscovery(cfg, "anthropic/claude-opus-5.5", kiloPaid) {
			t.Error("isFree=false model must be rejected")
		}
	})

	t.Run("free_markers_suffix", func(t *testing.T) {
		cfg := config.ProviderConfig{ID: "zen", Discovery: config.DiscoveryConfig{
			FreeMarkers: []string{"-free"},
		}}
		if !freeByDiscovery(cfg, "deepseek-v4.1-flash-free", zenFree) {
			t.Error("-free suffix should pass")
		}
		if freeByDiscovery(cfg, "gpt-5.5", zenPaid) {
			t.Error("paid model without marker must be rejected")
		}
	})

	t.Run("marker_or_detect", func(t *testing.T) {
		cfg := config.ProviderConfig{ID: "zen", Discovery: config.DiscoveryConfig{
			FreeMarkers:     []string{"-free"},
			FreeDetectField: "cost.text.input.price",
			FreeDetectValue: "0",
		}}
		if !freeByDiscovery(cfg, "big-pickle", zenFree) {
			t.Error("zero-cost model passes via free_detect_field even without the marker")
		}
		if freeByDiscovery(cfg, "gpt-5.5", zenPaid) {
			t.Error("paid model must fail both detections")
		}
	})
}

func TestDotPath(t *testing.T) {
	m := map[string]any{"a": map[string]any{"b": map[string]any{"c": "x"}}, "n": 42.0}
	if got := dotPath(m, "a.b.c"); got != "x" {
		t.Errorf("dotPath(a.b.c) = %v, want x", got)
	}
	if got := dotPath(m, "n"); got != 42.0 {
		t.Errorf("dotPath(n) = %v, want 42", got)
	}
	if got := dotPath(m, "a.b.missing"); got != nil {
		t.Errorf("dotPath(a.b.missing) = %v, want nil", got)
	}
	if got := dotPath(m, "a.b.c.deep"); got != nil {
		t.Errorf("dotPath past a leaf = %v, want nil", got)
	}
}

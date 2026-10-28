package scan

import "testing"

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

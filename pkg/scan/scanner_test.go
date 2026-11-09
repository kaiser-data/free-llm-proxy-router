package scan

import (
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
)

// Every scanner's output passes the non-chat filter, including scanners
// (OpenRouter, HuggingFace, GitHub) that do not filter themselves.
func TestDropNonChat(t *testing.T) {
	in := []catalog.CatalogEntry{
		{ProviderID: "openrouter", ModelID: "meta-llama/llama-guard-4-12b"},
		{ProviderID: "openrouter", ModelID: "qwen/qwen3-coder:free"},
	}
	got := dropNonChat(in)
	if len(got) != 1 || got[0].ModelID != "qwen/qwen3-coder:free" {
		t.Errorf("dropNonChat = %+v", got)
	}
}

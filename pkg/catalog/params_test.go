package catalog

import (
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
)

// Precomputed sizes equal parsing on demand, and copies keep them.
func TestPrecomputeParams_MatchesExtractParams(t *testing.T) {
	entries := []CatalogEntry{
		{ModelID: "mixtral-8x7b"}, {ModelID: "llama-3.1-8b"}, {ModelID: "gemini-2.0-flash"},
		{ModelID: "phi-3-mini"}, {ModelID: "GPT-oss-120B"},
	}
	for i := range entries {
		if got, want := entries[i].ParamScale(), models.ExtractParams(entries[i].ModelID); got != want {
			t.Errorf("%s on demand: %+v, want %+v", entries[i].ModelID, got, want)
		}
	}
	PrecomputeParams(entries)
	for _, e := range entries { // e is a copy
		if !e.hasParams {
			t.Fatalf("%s: precomputed size lost in copy", e.ModelID)
		}
		if got, want := e.ParamScale(), models.ExtractParams(e.ModelID); got != want {
			t.Errorf("%s precomputed: %+v, want %+v", e.ModelID, got, want)
		}
	}
}

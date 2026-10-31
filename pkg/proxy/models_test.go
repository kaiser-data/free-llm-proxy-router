package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

func TestHandleModels_StrategiesProfilesDedupe(t *testing.T) {
	s := &Server{strategyReg: strategy.NewRegistry(nil, nil, "", "", 0, 0)}
	cfg := &config.Config{}
	cfg.Proxy.Agents = map[string]config.AgentProfile{"my-agent": {}}
	s.cfg.Store(cfg)
	s.catalog.Store(&catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "groq", ModelID: "llama-x", IsFree: true},
		{ProviderID: "openrouter", ModelID: "llama-x", IsFree: true}, // duplicate ID
		{ProviderID: "gemini", ModelID: "gemma-y", IsFree: true},
		{ProviderID: "gemini", ModelID: "paid-z", IsFree: false},
	}})

	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	owners := map[string]string{}
	count := map[string]int{}
	for _, m := range body.Data {
		owners[m.ID] = m.OwnedBy
		count[m.ID]++
	}
	if body.Data[0].ID != "auto" {
		t.Errorf("first model = %q, want auto", body.Data[0].ID)
	}
	for _, id := range []string{"auto", "speed", "coding", "my-agent", "llama-x", "gemma-y"} {
		if count[id] != 1 {
			t.Errorf("%q listed %d times, want 1", id, count[id])
		}
	}
	if owners["llama-x"] != "groq" {
		t.Errorf("llama-x owned_by = %q, want groq (first provider)", owners["llama-x"])
	}
	if _, ok := owners["paid-z"]; ok {
		t.Error("non-free model listed")
	}
}

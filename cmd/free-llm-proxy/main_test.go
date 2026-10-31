package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

func TestRescanCatalog_FailedProviderKeepsPreviousEntries(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"llama-new"}]}`))
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()

	path := filepath.Join(t.TempDir(), "catalog.json")
	prev := &catalog.Catalog{Version: 1, Entries: []catalog.CatalogEntry{
		{ProviderID: "cerebras", ModelID: "llama-old", IsFree: true},
		{ProviderID: "mistral", ModelID: "mistral-small", IsFree: true},
	}}
	if err := catalog.Save(prev, path); err != nil {
		t.Fatal(err)
	}

	providers := []config.ProviderConfig{
		{ID: "cerebras", BaseURL: up.URL, Enabled: true},
		{ID: "mistral", BaseURL: down.URL, Enabled: true},
	}
	if err := rescanCatalog(context.Background(), providers, path); err != nil {
		t.Fatal(err)
	}

	got, err := catalog.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range got.Entries {
		have[e.ProviderID+"/"+e.ModelID] = true
	}
	if !have["cerebras/llama-new"] || have["cerebras/llama-old"] {
		t.Errorf("cerebras should be replaced by the new scan: %v", have)
	}
	if !have["mistral/mistral-small"] {
		t.Errorf("mistral scan failed, its previous entries must be kept: %v", have)
	}
}

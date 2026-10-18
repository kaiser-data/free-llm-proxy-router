package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// TestHandleModelsAdvertisesCanonicalAliases verifies that /v1/models lists
// both the raw upstream IDs and the canonical alias names derived from them.
func TestHandleModelsAdvertisesCanonicalAliases(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true

	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "openrouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true},
		{ProviderID: "huggingface", ModelID: "Qwen/Qwen3-32B", IsFree: true},
		{ProviderID: "huggingface", ModelID: "paid/model", IsFree: false}, // excluded
	}}

	s := NewServer(cfg, cat, nil, nil, nil, nil)

	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Object != "list" {
		t.Errorf("object = %q, want %q", body.Object, "list")
	}

	ids := map[string]bool{}
	for _, m := range body.Data {
		if m.Object != "model" {
			t.Errorf("entry %q: object = %q, want %q", m.ID, m.Object, "model")
		}
		ids[m.ID] = true
	}

	// Raw upstream IDs stay listed unchanged.
	for _, raw := range []string{"qwen/qwen3-235b-a22b:free", "Qwen/Qwen3-32B"} {
		if !ids[raw] {
			t.Errorf("raw model %q missing from /v1/models", raw)
		}
	}
	// Canonical aliases are advertised.
	if !ids["qwen3"] {
		t.Errorf("canonical alias %q missing from /v1/models; got %v", "qwen3", ids)
	}
	// Non-free catalog entries must not appear.
	if ids["paid/model"] {
		t.Errorf("non-free model %q must not be listed", "paid/model")
	}
}

// TestHandleModelsWithoutAliases verifies raw-only listing when the alias
// layer is disabled.
func TestHandleModelsWithoutAliases(t *testing.T) {
	cfg := &config.Config{} // canonicalization disabled → no aliases
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "huggingface", ModelID: "Qwen/Qwen3-32B", IsFree: true},
	}}
	s := NewServer(cfg, cat, nil, nil, nil, nil)

	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "Qwen/Qwen3-32B" {
		t.Fatalf("data = %+v, want only the raw model", body.Data)
	}
}

// newExposureServer builds a Server with one free upstream entry and its
// canonical group, with the given exposure flags.
func newExposureServer(exposeRaw, exposeCanonical bool) *Server {
	cfg := &config.Config{}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.ExposeRaw = exposeRaw
	cfg.Models.ExposeCanonical = exposeCanonical
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "huggingface", ModelID: "Qwen/Qwen3-32B", IsFree: true},
	}}
	return NewServer(cfg, cat, nil, nil, nil, nil)
}

func handleModelsIDs(s *Server) (map[string]bool, int) {
	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	ids := map[string]bool{}
	for _, m := range body.Data {
		ids[m.ID] = true
	}
	return ids, len(body.Data)
}

// TestHandleModelsExposureCanonicalOnly verifies expose_raw: false +
// expose_canonical: true advertises canonical alias names only, while the
// raw registry stays intact internally (the raw ID is still reserved and
// requestable).
func TestHandleModelsExposureCanonicalOnly(t *testing.T) {
	s := newExposureServer(false, true)
	ids, n := handleModelsIDs(s)
	if !ids["qwen3"] {
		t.Errorf("canonical alias %q missing from /v1/models; got %v", "qwen3", ids)
	}
	if ids["Qwen/Qwen3-32B"] {
		t.Errorf("raw model %q must not be listed when expose_raw is false", "Qwen/Qwen3-32B")
	}
	if n != 1 {
		t.Errorf("listing has %d entries, want exactly the canonical name", n)
	}
}

// TestHandleModelsExposureRawOnly verifies expose_raw: true +
// expose_canonical: false advertises raw upstream IDs only.
func TestHandleModelsExposureRawOnly(t *testing.T) {
	s := newExposureServer(true, false)
	ids, n := handleModelsIDs(s)
	if !ids["Qwen/Qwen3-32B"] {
		t.Errorf("raw model %q missing from /v1/models; got %v", "Qwen/Qwen3-32B", ids)
	}
	if ids["qwen3"] {
		t.Errorf("canonical alias %q must not be listed when expose_canonical is false", "qwen3")
	}
	if n != 1 {
		t.Errorf("listing has %d entries, want exactly the raw model", n)
	}
}

// TestHandleModelsExposureBoth verifies the explicit both-true configuration
// advertises raw IDs and canonical names.
func TestHandleModelsExposureBoth(t *testing.T) {
	s := newExposureServer(true, true)
	ids, n := handleModelsIDs(s)
	if !ids["Qwen/Qwen3-32B"] || !ids["qwen3"] {
		t.Errorf("expected both raw and canonical entries; got %v", ids)
	}
	if n != 2 {
		t.Errorf("listing has %d entries, want 2", n)
	}
}

// TestHandleModelsExposureNoneFallsBackToBoth verifies the misconfiguration
// guard: disabling both flags falls back to exposing both so a bad edit can
// never leave clients with an empty discovery list.
func TestHandleModelsExposureNoneFallsBackToBoth(t *testing.T) {
	s := newExposureServer(false, false)
	ids, n := handleModelsIDs(s)
	if !ids["Qwen/Qwen3-32B"] || !ids["qwen3"] {
		t.Errorf("both-false must fall back to exposing both; got %v", ids)
	}
	if n != 2 {
		t.Errorf("listing has %d entries, want 2", n)
	}
}

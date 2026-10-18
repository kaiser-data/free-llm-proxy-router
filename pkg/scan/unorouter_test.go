package scan

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// TestUnorouterScanParsesModels feeds a realistic /v1/models payload —
// vendor-prefixed and bare free IDs, priced models, one with pricing
// metadata — and checks the catalog entries, free flags, metadata capture,
// and paid/embedding filtering.
func TestUnorouterScanParsesModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "qwen/qwen3-235b-a22b:free", "object": "model", "created": 1735689600, "owned_by": "qwen", "context_length": 131072},
				{"id": "qwen/qwen3-30b-a3b:free", "object": "model"},
				{"id": "space-bunny-alpha:free", "object": "model"},
				{"id": "aura-1:free", "object": "model", "supported_endpoint_types": ["openai"]},
				{"id": "claude-sonnet-5-5", "object": "model", "pricing": {"prompt": "0.0005", "completion": "0.002"}},
				{"id": "gpt-6-luna", "object": "model", "pricing": {"input": 0.05, "output": 0.10}},
				{"id": "mimo-v2.6-pro", "object": "model"},
				{"id": "horde-diffusion:free", "object": "model", "supported_endpoint_types": ["aihorde"]},
				{"id": "embed-free-large:free", "object": "model", "supported_endpoint_types": ["openai", "embedding"]},
				{"id": "free-embed-mini", "object": "model"}
			]
		}`))
	}))
	defer srv.Close()

	cfg := config.ProviderConfig{
		ID:      "unorouter",
		BaseURL: srv.URL + "/v1",
		Enabled: true,
	}
	s := &UnorouterScanner{Client: srv.Client()}
	entries, err := s.ScanFreeModels(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ScanFreeModels: %v", err)
	}

	var got []string
	byID := map[string]catalog.CatalogEntry{}
	for _, e := range entries {
		got = append(got, e.ModelID)
		byID[e.ModelID] = e
	}
	want := []string{"qwen/qwen3-235b-a22b:free", "qwen/qwen3-30b-a3b:free", "space-bunny-alpha:free", "aura-1:free"}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want exactly %v (paid, embedding, and non-chat models must be excluded)", got, want)
	}
	for _, id := range want {
		e, ok := byID[id]
		if !ok {
			t.Errorf("free model %q missing from entries", id)
			continue
		}
		if !e.IsFree || e.TierType != "free" {
			t.Errorf("%s: IsFree=%v TierType=%q, want true/free", id, e.IsFree, e.TierType)
		}
		if e.ProviderID != "unorouter" {
			t.Errorf("%s: ProviderID = %q, want unorouter", id, e.ProviderID)
		}
	}
	// Metadata capture: vendor + context window + pricing where present.
	if e := byID["qwen/qwen3-235b-a22b:free"]; e.ContextWindow != 131072 {
		t.Errorf("context window = %d, want 131072", e.ContextWindow)
	} else if e.Metadata["owned_by"] != "qwen" {
		t.Errorf("metadata owned_by = %v, want qwen", e.Metadata["owned_by"])
	}
}

// TestUnorouterFreeDetection locks in the free-tier decision table: pricing
// metadata wins over the ID marker, and without metadata only the documented
// ":free" suffix counts.
func TestUnorouterFreeDetection(t *testing.T) {
	tf, ff := true, false
	p := func(prompt, completion string) *unoPricing {
		return &unoPricing{Prompt: &prompt, Completion: &completion}
	}
	cases := []struct {
		name  string
		model unoModel
		want  bool
	}{
		{"marker suffix, no metadata", unoModel{ID: "qwen/qwen3-235b-a22b:free"}, true},
		{"bare paid id, no metadata", unoModel{ID: "mimo-v2.6-pro"}, false},
		{"string pricing zero", unoModel{ID: "a", Pricing: p("0", "0")}, true},
		{"string pricing nonzero", unoModel{ID: "a", Pricing: p("0.0005", "0.002")}, false},
		{"metadata wins over marker (paid)", unoModel{ID: "a:free", Pricing: p("0.01", "0")}, false},
		{"metadata wins over marker (free)", unoModel{ID: "a", Pricing: p("0", "0")}, true},
		{"float pricing zero", unoModel{ID: "a", Pricing: &unoPricing{Input: ptrF(0), Output: ptrF(0)}}, true},
		{"float pricing nonzero", unoModel{ID: "a", Pricing: &unoPricing{Input: ptrF(0.05), Output: ptrF(0.10)}}, false},
		{"explicit is_free true", unoModel{ID: "a", Pricing: &unoPricing{IsFree: &tf}}, true},
		{"explicit is_free false", unoModel{ID: "a:free", Pricing: &unoPricing{IsFree: &ff}}, false},
		{"top-level is_free true", unoModel{ID: "a", IsFree: &tf}, true},
		{"partial pricing: prompt zero only", unoModel{ID: "a", Pricing: &unoPricing{Prompt: ptrS("0")}}, true},
		{"unparsable pricing falls back to marker", unoModel{ID: "a:free", Pricing: p("", "")}, true},
	}
	for _, c := range cases {
		if got := unoIsFree(c.model, nil); got != c.want {
			t.Errorf("%s: unoIsFree = %v, want %v", c.name, got, c.want)
		}
	}
	// Custom markers replace the default suffix.
	if !unoIsFree(unoModel{ID: "a-gratis"}, []string{"-gratis"}) {
		t.Error("custom marker -gratis must match")
	}
	if unoIsFree(unoModel{ID: "a:free"}, []string{"-gratis"}) {
		t.Error("custom markers must replace the :free default")
	}
}

// TestUnorouterChatCapability: endpoint metadata decides chat eligibility —
// "openai" marks the OpenAI-compatible chat endpoint; "embedding",
// "image-generation", and "aihorde" models cannot serve chat; entries
// without the field defer to the name-based filter.
func TestUnorouterChatCapability(t *testing.T) {
	cases := []struct {
		name  string
		types []string
		want  bool
	}{
		{"openai only", []string{"openai"}, true},
		{"anthropic+openai", []string{"anthropic", "openai"}, true},
		{"embedding excluded", []string{"openai", "embedding"}, false},
		{"aihorde excluded", []string{"aihorde"}, false},
		{"image-generation excluded", []string{"image-generation"}, false},
		{"unknown defers to name filter", nil, true},
	}
	for _, c := range cases {
		if got := unoChatCapable(unoModel{ID: "m", SupportedEndpointTypes: c.types}); got != c.want {
			t.Errorf("%s: unoChatCapable = %v, want %v", c.name, got, c.want)
		}
	}
}

func ptrF(f float64) *float64 { return &f }
func ptrS(s string) *string   { return &s }

// TestUnorouterScanSendsBearerAuth verifies the Authorization header is
// built from api_key_env (the documented Bearer scheme) and that a 401 is
// reported as an error instead of silently yielding zero entries.
func TestUnorouterScanSendsBearerAuth(t *testing.T) {
	t.Setenv("UNO_SCAN_TEST_KEY", "uno-key-for-tests")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer uno-key-for-tests" {
			t.Errorf("Authorization = %q, want Bearer uno-key-for-tests", got)
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"space-bunny-alpha:free"}]}`))
	}))
	defer srv.Close()

	cfg := config.ProviderConfig{
		ID:        "unorouter",
		BaseURL:   srv.URL + "/v1",
		APIKeyEnv: "UNO_SCAN_TEST_KEY",
		Enabled:   true,
	}
	entries, err := (&UnorouterScanner{Client: srv.Client()}).ScanFreeModels(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ScanFreeModels: %v", err)
	}
	if len(entries) != 1 || entries[0].ModelID != "space-bunny-alpha:free" {
		t.Errorf("entries = %+v, want one space-bunny-alpha:free", entries)
	}
}

// TestUnorouterScanAuthFailureIsError: upstream 401 must surface as an error.
func TestUnorouterScanAuthFailureIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	cfg := config.ProviderConfig{ID: "unorouter", BaseURL: srv.URL + "/v1", Enabled: true}
	if _, err := (&UnorouterScanner{Client: srv.Client()}).ScanFreeModels(context.Background(), cfg); err == nil {
		t.Fatal("expected an error on 401, got nil")
	}
}

// TestUnorouterScannerDispatch verifies the dispatcher maps provider id
// "unorouter" to its own scanner — not the OpenRouter or generic one.
func TestUnorouterScannerDispatch(t *testing.T) {
	d := NewDispatcher()
	s := d.scannerFor("unorouter")
	if _, ok := s.(*UnorouterScanner); !ok {
		t.Errorf("scannerFor(unorouter) = %T, want *UnorouterScanner", s)
	}
}

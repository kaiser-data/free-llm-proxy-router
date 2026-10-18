package scan

// Provider: UnoRouter
// Docs: https://unorouter.com/docs
// Base URL: https://api.unorouter.com/v1
// Last verified: 2026-10-05 (live /v1/models + chat/completions E2E)
// Free tier: free models carry a ":free" suffix on the public model ID
//            (e.g. "qwen/qwen3-235b-a22b:free", "space-bunny-alpha:free");
//            paid models have real per-token prices. Model availability
//            depends on the API key.
// Auth: Authorization: Bearer (standard OpenAI-compatible)
//
// UnoRouter is an INDEPENDENT OpenAI-compatible aggregator — it is not
// OpenRouter, HuggingFace, Groq, or any other provider in this repo. It has
// no models[] native fallback, no required referer headers, and no Hub API.
// This scanner is UnoRouter's own implementation; it only shares the
// generic OpenAI wire format (GET /v1/models, POST /v1/chat/completions).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// UnorouterScanner discovers free models on UnoRouter via the standard
// OpenAI-compatible GET /v1/models endpoint. Only free-tier models are
// returned: UnoRouter also sells paid models, and assuming every listed
// model were free would silently bill paid calls.
type UnorouterScanner struct {
	Client *http.Client
}

// unoPricing is the optional per-model pricing block. UnoRouter documents
// free models through the ":free" ID suffix; when the API additionally
// exposes pricing metadata, the metadata wins over the suffix heuristic.
type unoPricing struct {
	// OpenRouter-style decimal-string prices ("0", "0.000001", …).
	Prompt     *string `json:"prompt"`
	Completion *string `json:"completion"`
	// Numeric per-token prices.
	Input  *float64 `json:"input"`
	Output *float64 `json:"output"`
	// Explicit free flag when the API exposes one.
	IsFree *bool `json:"is_free"`
}

// unoModel is one entry of the /v1/models "data" array. All fields beyond
// ID are optional — the endpoint is OpenAI-compatible but the exact
// metadata returned depends on the API key.
type unoModel struct {
	ID                     string      `json:"id"`
	Object                 string      `json:"object"`
	Created                int64       `json:"created"`
	OwnedBy                string      `json:"owned_by"`
	ContextLength          int         `json:"context_length"`
	SupportedEndpointTypes []string    `json:"supported_endpoint_types"`
	Pricing                *unoPricing `json:"pricing"`
	IsFree                 *bool       `json:"is_free"`
}

type unoModelsResponse struct {
	Object string     `json:"object"`
	Data   []unoModel `json:"data"`
}

// unoDefaultFreeMarkers is used when the provider config lists no
// discovery.free_markers. UnoRouter documents ":free" as the free-tier
// suffix.
var unoDefaultFreeMarkers = []string{":free"}

func (s *UnorouterScanner) ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error) {
	endpoint := cfg.Discovery.ModelsEndpoint
	if endpoint == "" {
		endpoint = "/models"
	}
	url := strings.TrimRight(cfg.BaseURL, "/") + endpoint

	req, err := makeRequest(ctx, url, cfg)
	if err != nil {
		return nil, err
	}

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unorouter models request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading unorouter response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unorouter models: status %d: %s", resp.StatusCode, body)
	}

	var ur unoModelsResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return nil, fmt.Errorf("parsing unorouter models: %w", err)
	}

	tierType := cfg.TierType
	if tierType == "" {
		tierType = "free"
	}

	var entries []catalog.CatalogEntry
	for _, m := range ur.Data {
		if m.ID == "" {
			continue
		}
		// Chat-capable check: endpoint metadata wins when exposed
		// ("embedding"/"image-generation"/"aihorde" models cannot serve
		// chat); otherwise the name-based filter below still applies.
		if !unoChatCapable(m) {
			continue
		}
		if !isEligibleModel(cfg.ID, m.ID, "", nil) {
			continue
		}
		if !unoIsFree(m, cfg.Discovery.FreeMarkers) {
			continue // paid model — not a free-tier candidate
		}
		entries = append(entries, catalog.CatalogEntry{
			ProviderID:     cfg.ID,
			ModelID:        m.ID,
			IsFree:         true,
			TierType:       tierType,
			ContextWindow:  m.ContextLength,
			DiscoveredAt:   time.Now(),
			LastVerifiedAt: time.Now(),
			Metadata:       unoMetadata(m),
		})
	}
	return entries, nil
}

// unoChatCapable reports whether a discovered model can serve
// OpenAI-compatible /chat/completions calls. UnoRouter publishes
// supported_endpoint_types per model: "openai" marks chat capability,
// while "embedding", "image-generation", "aihorde", … do not. Entries
// without the field fall through to the name-based isEligibleModel filter.
func unoChatCapable(m unoModel) bool {
	if len(m.SupportedEndpointTypes) == 0 {
		return true // unknown — decide by name in isEligibleModel
	}
	for _, t := range m.SupportedEndpointTypes {
		if strings.EqualFold(t, "embedding") {
			return false
		}
	}
	for _, t := range m.SupportedEndpointTypes {
		if strings.EqualFold(t, "openai") {
			return true
		}
	}
	return false
}

// unoIsFree decides whether a discovered UnoRouter model is on the free
// tier. Pricing metadata is preferred when the API exposes it — an explicit
// is_free flag, or prompt+completion / input+output prices — and a clearly
// non-zero price marks the model paid even when the ID carries a marker.
// Without metadata, the documented ":free" ID suffix (or the markers
// configured via discovery.free_markers) is the signal. Anything else is
// treated as paid: not every UnoRouter model is free.
func unoIsFree(m unoModel, markers []string) bool {
	flag := m.IsFree
	if m.Pricing != nil && m.Pricing.IsFree != nil {
		flag = m.Pricing.IsFree
	}
	if flag != nil {
		return *flag
	}
	if m.Pricing != nil {
		known, zero := 0, 0
		for _, s := range []*string{m.Pricing.Prompt, m.Pricing.Completion} {
			if s == nil {
				continue
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(*s), 64); err == nil {
				known++
				if f == 0 {
					zero++
				} else {
					return false // clearly priced — metadata wins
				}
			}
		}
		for _, f := range []*float64{m.Pricing.Input, m.Pricing.Output} {
			if f == nil {
				continue
			}
			known++
			if *f == 0 {
				zero++
			} else {
				return false // clearly priced — metadata wins
			}
		}
		if known > 0 {
			return known == zero // free only when every known price is zero
		}
	}
	return unoHasFreeMarker(m.ID, markers)
}

// unoHasFreeMarker reports whether the model ID ends with one of the
// configured free markers, defaulting to the documented ":free" suffix.
func unoHasFreeMarker(id string, markers []string) bool {
	if len(markers) == 0 {
		markers = unoDefaultFreeMarkers
	}
	for _, marker := range markers {
		if marker != "" && strings.HasSuffix(id, marker) {
			return true
		}
	}
	return false
}

// unoMetadata captures provider-specific fields for the catalog entry so
// pricing/ownership stays available to routing and debugging tools.
func unoMetadata(m unoModel) map[string]any {
	meta := map[string]any{}
	if m.OwnedBy != "" {
		meta["owned_by"] = m.OwnedBy
	}
	if len(m.SupportedEndpointTypes) > 0 {
		meta["supported_endpoint_types"] = append([]string(nil), m.SupportedEndpointTypes...)
	}
	if m.IsFree != nil {
		meta["is_free"] = *m.IsFree
	}
	if p := m.Pricing; p != nil {
		pricing := map[string]any{}
		if p.Prompt != nil {
			pricing["prompt"] = *p.Prompt
		}
		if p.Completion != nil {
			pricing["completion"] = *p.Completion
		}
		if p.Input != nil {
			pricing["input"] = *p.Input
		}
		if p.Output != nil {
			pricing["output"] = *p.Output
		}
		if p.IsFree != nil {
			pricing["is_free"] = *p.IsFree
		}
		if len(pricing) > 0 {
			meta["pricing"] = pricing
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

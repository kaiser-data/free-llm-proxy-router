package scan

// Provider: Hugging Face Inference API
// Docs: https://huggingface.co/docs/api-inference/quicktour
//       https://huggingface.co/docs/huggingface_hub/guides/inference
// Last verified: 2026-02-28
// Free tier: 300 calls/hour/token (resets hourly)
// Auth: Authorization: Bearer (standard)
// Discovery: HF Hub API (NOT /v1/models — that has 100k+ models)
//   GET https://huggingface.co/api/models?pipeline_tag=text-generation&inference=warm&gated=false&limit=50
// Special: 503 + "loading" = cold start; wait estimated_time and retry SAME provider
// Note: Only models supporting /v1/chat/completions are in catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/config"
)

// HuggingFaceScanner uses the HF Hub API to find warm, non-gated chat models.
type HuggingFaceScanner struct {
	Client *http.Client
}

type hfModel struct {
	ModelID   string   `json:"modelId"`
	ID        string   `json:"id"` // alias
	Gated     bool     `json:"gated"`
	Tags      []string `json:"tags"`
	PipelineTag string `json:"pipeline_tag"`
}

const hfHubBase = "https://huggingface.co/api/models"

func (s *HuggingFaceScanner) ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error) {
	// Use Hub API with filters for warm, non-gated text-generation models
	filters := cfg.Discovery.HubAPIFilters
	if filters == "" {
		filters = "pipeline_tag=text-generation&inference=warm&gated=false&limit=50"
	}
	url := hfHubBase + "?" + filters

	apiKey := cfg.ResolvedAPIKey()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building hf request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hf hub request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading hf response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hf hub: status %d: %s", resp.StatusCode, body)
	}

	var models []hfModel
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, fmt.Errorf("parsing hf models: %w", err)
	}

	var entries []catalog.CatalogEntry
	for _, m := range models {
		id := m.ModelID
		if id == "" {
			id = m.ID
		}
		if id == "" || m.Gated {
			continue
		}
		// Only include models that support the chat completions endpoint
		if !hasTextGenTag(m.Tags) && m.PipelineTag != "text-generation" {
			continue
		}

		entries = append(entries, catalog.CatalogEntry{
			ProviderID:     cfg.ID,
			ModelID:        id,
			IsFree:         true,
			TierType:       "free",
			DiscoveredAt:   time.Now(),
			LastVerifiedAt: time.Now(),
			Metadata: map[string]any{
				"pipeline_tag": m.PipelineTag,
			},
		})
	}
	return entries, nil
}

func hasTextGenTag(tags []string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, "text-generation") || strings.EqualFold(t, "conversational") {
			return true
		}
	}
	return false
}

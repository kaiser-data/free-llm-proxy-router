package scan

// Provider: GitHub Models
// Docs: https://docs.github.com/en/github-models
// Last verified: 2026-03-02
//
// Free tier: Truly free — just a GitHub account (no credit card).
// Rate limits by plan and rate_limit_tier:
//   - Free/Pro: low/medium: 15-20 req/min, 150-300 req/day
//   - Free/Pro: high: 8-10 req/min, 50-100 req/day
//   See: https://docs.github.com/en/github-models/prototyping-with-ai-models#rate-limits
//
// Auth:     Authorization: Bearer {GITHUB_TOKEN}  (any GitHub PAT)
// Base URL: https://models.inference.ai.azure.com
// API:      OpenAI-compatible (Azure AI Inference protocol)
// Multi:    vision (GPT-4.1, Llama Vision, Phi-4 multimodal) + audio input
//
// Discovery: GET https://models.github.ai/catalog/models
//
// Model ID rules (catalog id → API model parameter):
//   Catalog id field is "publisher/slug" (e.g. "meta/llama-3.3-70b-instruct").
//   The inference API uses just the slug (part after "/"):
//     "openai/gpt-4.1"                      → "gpt-4.1"
//     "meta/llama-3.3-70b-instruct"         → "llama-3.3-70b-instruct"
//     "mistral-ai/codestral-2501"           → "codestral-2501"
//   Exceptions (hardcoded below — don't follow the strip-prefix rule):
//     "ai21-labs/ai21-jamba-1.5-large"      → not available on inference endpoint

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

// githubModelSkip lists catalog IDs that are not available on the inference endpoint.
// These are blocklisted at scan time so they never enter the catalog.
var githubModelSkip = map[string]bool{
	"ai21-labs/ai21-jamba-1.5-large":   true, // not found on inference endpoint
	"meta/meta-llama-3.1-405b-instruct": true, // not found on inference endpoint
}

// GitHubScanner fetches the GitHub Models marketplace catalog.
type GitHubScanner struct {
	Client *http.Client
}

const githubModelsCatalogURL = "https://models.github.ai/catalog/models"

type githubCatalogModel struct {
	ID                        string   `json:"id"`
	Name                      string   `json:"name"` // display name, not API id
	Publisher                 string   `json:"publisher"`
	Registry                  string   `json:"registry"`
	RateLimitTier             string   `json:"rate_limit_tier"`
	SupportedInputModalities  []string `json:"supported_input_modalities"`
	SupportedOutputModalities []string `json:"supported_output_modalities"`
	Tags                      []string `json:"tags"`
	Capabilities              []string `json:"capabilities"`
}

// apiModelID derives the model name to pass in API calls from the catalog entry.
// Rule: strip "publisher/" prefix from catalog id.
//   "openai/gpt-4.1" → "gpt-4.1"
//   "meta/llama-3.3-70b-instruct" → "llama-3.3-70b-instruct"
func apiModelID(catalogID string) string {
	if idx := strings.IndexByte(catalogID, '/'); idx >= 0 {
		return catalogID[idx+1:]
	}
	return catalogID
}

func (s *GitHubScanner) ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubModelsCatalogURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building github catalog request: %w", err)
	}
	apiKey := cfg.ResolvedAPIKey()
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github catalog request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading github catalog: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github catalog: status %d: %s", resp.StatusCode, body)
	}

	// The response is a JSON array of model objects.
	var models []githubCatalogModel
	if err := json.Unmarshal(body, &models); err != nil {
		// Try wrapped object fallback: {"models": [...]}
		var wrapped struct {
			Models []githubCatalogModel `json:"models"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return nil, fmt.Errorf("github catalog: parsing response: %w", err)
		}
		models = wrapped.Models
	}

	var entries []catalog.CatalogEntry
	for _, m := range models {
		// Skip embedding models — not chat/text-generation
		if m.RateLimitTier == "embeddings" {
			continue
		}
		// Skip models with no text output (e.g. pure image generation)
		hasTextOutput := len(m.SupportedOutputModalities) == 0 // empty = assume text
		for _, mod := range m.SupportedOutputModalities {
			if mod == "text" {
				hasTextOutput = true
				break
			}
		}
		if !hasTextOutput {
			continue
		}
		// Skip known-broken models (not available on inference endpoint)
		if githubModelSkip[m.ID] {
			continue
		}

		// Derive the API model name (what to pass as model= in API calls)
		modelID := apiModelID(m.ID)

		// Detect vision capability
		supportsVision := false
		for _, mod := range m.SupportedInputModalities {
			if mod == "image" {
				supportsVision = true
				break
			}
		}
		for _, tag := range m.Tags {
			low := strings.ToLower(tag)
			if strings.Contains(low, "multimodal") || strings.Contains(low, "vision") {
				supportsVision = true
				break
			}
		}

		// Detect audio input capability
		supportsAudioInput := false
		for _, mod := range m.SupportedInputModalities {
			if mod == "audio" {
				supportsAudioInput = true
				break
			}
		}

		meta := map[string]any{
			"catalog_id":      m.ID,
			"publisher":       m.Publisher,
			"registry":        m.Registry,
			"rate_limit_tier": m.RateLimitTier,
		}
		if supportsVision {
			meta["supports_vision"] = true
		}
		if supportsAudioInput {
			meta["supports_audio_input"] = true
		}

		entries = append(entries, catalog.CatalogEntry{
			ProviderID:     cfg.ID,
			ModelID:        modelID,
			IsFree:         true,
			TierType:       "free",
			DiscoveredAt:   time.Now(),
			LastVerifiedAt: time.Now(),
			Metadata:       meta,
		})
	}
	return entries, nil
}

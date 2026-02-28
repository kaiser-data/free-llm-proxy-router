package scan

// Provider: Google AI Studio (Gemini)
// Docs: https://ai.google.dev/gemini-api/docs/models
//       https://ai.google.dev/gemini-api/docs/rate-limits
// Last verified: 2026-02-28
// Free tier: available per model (RPM/TPM/RPD/IPM per model); limits cut Dec 2025
// Auth: x-goog-api-key header (NOT Authorization: Bearer)
// Discovery: GET /v1beta/models (different base URL from inference)
// Note: supportedGenerationMethods must include "generateContent"
// Note: RPD resets at midnight Pacific Time

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

// GeminiScanner discovers chat-capable free models from Google AI Studio.
type GeminiScanner struct {
	Client *http.Client
}

type geminiModelsResponse struct {
	Models        []geminiModel `json:"models"`
	NextPageToken string        `json:"nextPageToken"`
}

type geminiModel struct {
	Name                       string   `json:"name"` // "models/gemini-1.5-flash"
	BaseModelID                string   `json:"baseModelId"`
	Version                    string   `json:"version"`
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

// geminiDiscoveryBase is the discovery endpoint (different from inference base URL).
const geminiDiscoveryBase = "https://generativelanguage.googleapis.com"

func (s *GeminiScanner) ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error) {
	// Use the discovery endpoint from config or the default
	discoveryURL := geminiDiscoveryBase + "/v1beta/models"
	if cfg.Discovery.ModelsEndpoint != "" {
		discoveryURL = cfg.Discovery.ModelsEndpoint
	}

	// Gemini uses x-goog-api-key, not Authorization: Bearer
	apiKey := cfg.APIKey
	if cfg.APIKeyEnv != "" {
		if v := resolveEnv(cfg.APIKeyEnv); v != "" {
			apiKey = v
		}
	}

	var allModels []geminiModel
	pageToken := ""

	for {
		url := discoveryURL
		params := "?"
		if apiKey != "" {
			params += "key=" + apiKey
		}
		if pageToken != "" {
			if params != "?" {
				params += "&"
			}
			params += "pageToken=" + pageToken
		}
		if params != "?" {
			url += params
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("building gemini request: %w", err)
		}

		resp, err := s.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gemini models request: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading gemini response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("gemini models: status %d: %s", resp.StatusCode, body)
		}

		var gr geminiModelsResponse
		if err := json.Unmarshal(body, &gr); err != nil {
			return nil, fmt.Errorf("parsing gemini models: %w", err)
		}

		allModels = append(allModels, gr.Models...)
		if gr.NextPageToken == "" {
			break
		}
		pageToken = gr.NextPageToken
	}

	var entries []catalog.CatalogEntry
	for _, m := range allModels {
		if !supportsGenerateContent(m.SupportedGenerationMethods) {
			continue
		}
		// Normalise the model ID: strip "models/" prefix
		modelID := strings.TrimPrefix(m.Name, "models/")
		if modelID == "" {
			continue
		}
		// Skip model IDs that signal paid-only tiers
		if isGeminiPaidOnly(modelID) {
			continue
		}

		contextWindow := m.InputTokenLimit
		entries = append(entries, catalog.CatalogEntry{
			ProviderID:     cfg.ID,
			ModelID:        modelID,
			IsFree:         true,
			TierType:       "free",
			ContextWindow:  contextWindow,
			DiscoveredAt:   time.Now(),
			LastVerifiedAt: time.Now(),
		})
	}
	return entries, nil
}

func supportsGenerateContent(methods []string) bool {
	for _, m := range methods {
		if m == "generateContent" {
			return true
		}
	}
	return false
}

// isGeminiPaidOnly returns true for model IDs that require billing.
// This is a conservative heuristic; run picoclaw-scan probe to confirm.
func isGeminiPaidOnly(modelID string) bool {
	lower := strings.ToLower(modelID)
	for _, skip := range []string{"ultra", "bison", "gecko"} {
		if strings.Contains(lower, skip) {
			return true
		}
	}
	return false
}

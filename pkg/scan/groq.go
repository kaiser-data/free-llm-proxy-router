package scan

// Provider: Groq
// Docs: https://console.groq.com/docs/models
//       https://console.groq.com/docs/rate-limits
// Last verified: 2026-02-28
// Free tier: all listed models are free on developer tier
// Auth: Authorization: Bearer (standard)
// Special headers: none; per-model RPM/TPM/TPD limits
// Note: No Retry-After header; use exponential backoff
// Note: x-ratelimit-reset-* headers use Go duration strings ("1m0s", "6.566s")

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// GroqScanner discovers free models on Groq.
// All models returned by the API are free on the developer tier.
type GroqScanner struct {
	Client *http.Client
}

type groqModelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		// context_window may be present in some responses
		ContextWindow int `json:"context_window"`
	} `json:"data"`
}

// audioModelIDs contains model IDs that are audio-only (not chat).
// These are excluded from the free chat catalog.
var audioModelIDs = map[string]bool{
	"whisper-large-v3":       true,
	"whisper-large-v3-turbo": true,
	"distil-whisper-large-v3-en": true,
}

func (s *GroqScanner) ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error) {
	endpoint := cfg.Discovery.ModelsEndpoint
	if endpoint == "" {
		endpoint = "/openai/v1/models"
	}
	// Groq's models endpoint uses a different path from the base URL
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	// If base_url already contains /openai/v1, strip duplicated prefix
	url := baseURL + "/models"
	if strings.Contains(endpoint, "/openai/v1") {
		url = "https://api.groq.com" + endpoint
	}

	req, err := makeRequest(ctx, url, cfg)
	if err != nil {
		return nil, err
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("groq models request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading groq response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("groq models: status %d: %s", resp.StatusCode, body)
	}

	var gr groqModelsResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("parsing groq models: %w", err)
	}

	var entries []catalog.CatalogEntry
	for _, m := range gr.Data {
		// Skip audio-only models
		if audioModelIDs[m.ID] {
			continue
		}
		entries = append(entries, catalog.CatalogEntry{
			ProviderID:     cfg.ID,
			ModelID:        m.ID,
			IsFree:         true,
			TierType:       "free",
			ContextWindow:  m.ContextWindow,
			DiscoveredAt:   time.Now(),
			LastVerifiedAt: time.Now(),
		})
	}
	return entries, nil
}

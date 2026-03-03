package scan

// Provider: OpenRouter
// Docs: https://openrouter.ai/docs/quickstart
//       https://openrouter.ai/docs/guides/routing/model-fallbacks
//       https://openrouter.ai/docs/guides/routing/routers/free-models-router
// Last verified: 2026-02-28
// Free tier: 50 req/day (1000 req/day with $10+ balance)
// Native features: models[] fallback array, openrouter/free router
// Auth: Authorization: Bearer (standard)
// Special headers: HTTP-Referer and X-Title required on all requests

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

// OpenRouterScanner discovers free models on OpenRouter.
// Free models are detected by pricing.prompt == "0" AND pricing.completion == "0".
// The :free suffix is used as a secondary confirmation signal.
type OpenRouterScanner struct {
	Client *http.Client
}

type orModel struct {
	ID      string `json:"id"`
	Context int    `json:"context_length"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

type orModelsResponse struct {
	Data []orModel `json:"data"`
}

func (s *OpenRouterScanner) ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error) {
	endpoint := cfg.Discovery.ModelsEndpoint
	if endpoint == "" {
		endpoint = "/models"
	}
	url := strings.TrimRight(cfg.BaseURL, "/") + endpoint

	req, err := makeRequest(ctx, url, cfg)
	if err != nil {
		return nil, err
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter models request: %w", err)
	}
	defer resp.Body.Close()

	// OpenRouter paginates via x-total-count header — handle if present
	totalCount := 0
	if tc := resp.Header.Get("x-total-count"); tc != "" {
		totalCount, _ = strconv.Atoi(tc)
		_ = totalCount // future: paginate if needed
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading openrouter response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter models: status %d: %s", resp.StatusCode, body)
	}

	var or orModelsResponse
	if err := json.Unmarshal(body, &or); err != nil {
		return nil, fmt.Errorf("parsing openrouter models: %w", err)
	}

	var entries []catalog.CatalogEntry
	for _, m := range or.Data {
		isFreeByPricing := m.Pricing.Prompt == "0" && m.Pricing.Completion == "0"
		isFreeByMarker := strings.HasSuffix(m.ID, ":free")

		if !isFreeByPricing && !isFreeByMarker {
			continue
		}

		entries = append(entries, catalog.CatalogEntry{
			ProviderID:     cfg.ID,
			ModelID:        m.ID,
			IsFree:         true,
			TierType:       "free",
			ContextWindow:  m.Context,
			DiscoveredAt:   time.Now(),
			LastVerifiedAt: time.Now(),
		})
	}
	return entries, nil
}

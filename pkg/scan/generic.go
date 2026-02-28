package scan

// GenericScanner uses the standard OpenAI-compatible /v1/models endpoint.
// Used for: Mistral, Cerebras, Cohere, NVIDIA NIM, Together AI, DeepSeek.
//
// Provider: Mistral
// Docs: https://docs.mistral.ai/api/
// Last verified: 2026-02-28
// Free tier: 2 RPM, 500K TPM; workspace-level limits
// Auth: Authorization: Bearer
//
// Provider: Cerebras
// Docs: https://inference-docs.cerebras.ai/api-reference/chat-completions
// Last verified: 2026-02-28
// Free tier: all models, RPS limits
// Auth: Authorization: Bearer
//
// Provider: Cohere
// Docs: https://docs.cohere.com/reference/rate-limits
// Last verified: 2026-02-28
// Free tier: 1000 req/month on Trial key; required Cohere-Version header
// Auth: Authorization: Bearer
//
// Provider: NVIDIA NIM
// Docs: https://docs.api.nvidia.com/nim/reference/limits
// Last verified: 2026-02-28
// Free tier: ~1000 credits on signup; 40 RPM hard limit
// Auth: Authorization: Bearer
//
// Provider: Together AI
// Docs: https://docs.together.ai/docs/rate-limits
// Last verified: 2026-02-28
// Free tier: $25 credit (credit-based)
// Auth: Authorization: Bearer
//
// Provider: DeepSeek
// Docs: https://platform.deepseek.com/docs
// Last verified: 2026-02-28
// Free tier: ~$5 credit (credit-based); reasoning_content field in R1 response
// Auth: Authorization: Bearer

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

// GenericScanner implements ProviderScanner for providers with standard /v1/models.
type GenericScanner struct {
	Client *http.Client
}

type genericModelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		// Some providers include context_length in the model list
		ContextLength int `json:"context_length"`
		// Cohere includes a "endpoints" field listing supported operations
		Endpoints []string `json:"endpoints"`
		// Together includes a "type" field
		Type string `json:"type"`
	} `json:"data"`
}

func (s *GenericScanner) ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error) {
	endpoint := cfg.Discovery.ModelsEndpoint
	if endpoint == "" {
		endpoint = "/v1/models"
	}
	url := strings.TrimRight(cfg.BaseURL, "/") + endpoint

	req, err := makeRequest(ctx, url, cfg)
	if err != nil {
		return nil, err
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s models request: %w", cfg.ID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", cfg.ID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s models: status %d: %s", cfg.ID, resp.StatusCode, body)
	}

	var gr genericModelsResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("parsing %s models: %w", cfg.ID, err)
	}

	tierType := cfg.TierType
	if tierType == "" {
		tierType = "free"
	}

	var entries []catalog.CatalogEntry
	for _, m := range gr.Data {
		if !isEligibleModel(cfg.ID, m.ID, m.Type, m.Endpoints) {
			continue
		}
		entries = append(entries, catalog.CatalogEntry{
			ProviderID:     cfg.ID,
			ModelID:        m.ID,
			IsFree:         true,
			TierType:       tierType,
			ContextWindow:  m.ContextLength,
			DiscoveredAt:   time.Now(),
			LastVerifiedAt: time.Now(),
		})
	}
	return entries, nil
}

// isEligibleModel filters out models that are not chat-capable or paid-only.
func isEligibleModel(providerID, modelID, modelType string, endpoints []string) bool {
	lower := strings.ToLower(modelID)

	switch providerID {
	case "together":
		// Filter for chat/language models only
		if modelType != "" && !strings.EqualFold(modelType, "chat") &&
			!strings.EqualFold(modelType, "language") {
			return false
		}
	case "cohere":
		// Cohere's Trial key only covers command-r and related models
		if len(endpoints) > 0 && !hasEndpoint(endpoints, "chat") {
			return false
		}
	}

	// Skip embedding/reranking/image models by name pattern
	for _, skip := range []string{"embed", "rerank", "image", "vision-only", "tts", "stt", "whisper"} {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	return true
}

func hasEndpoint(endpoints []string, target string) bool {
	for _, e := range endpoints {
		if strings.EqualFold(e, target) {
			return true
		}
	}
	return false
}

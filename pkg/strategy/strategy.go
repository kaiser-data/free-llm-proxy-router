// Package strategy implements the 13 model-selection strategies.
package strategy

import (
	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
)

// Request holds the information needed to rank models.
type Request struct {
	// Messages is the list of messages in the chat request.
	Messages []map[string]any
	// EstimatedPromptTokens is the approximate token count of the input.
	EstimatedPromptTokens int
	// MaxTokens is the requested output token limit.
	MaxTokens int
	// Stream indicates a streaming request.
	Stream bool
	// Model is the requested model (or "auto").
	Model string
}

// RankedEntry pairs a catalog entry with a priority score (lower = higher priority).
type RankedEntry struct {
	Entry    catalog.CatalogEntry
	Score    float64
	ProviderID string
	ModelID  string
}

// Strategy is the interface implemented by all 13 strategies.
type Strategy interface {
	// Name returns the strategy identifier used in config.
	Name() string
	// Rank returns an ordered list of model candidates for the given request.
	// Input: free entries from the catalog + enriched metadata.
	// Output: ordered RankedEntry slice (index 0 = most preferred).
	Rank(req Request, entries []catalog.CatalogEntry, enriched *models.EnrichedCatalog) []RankedEntry
}

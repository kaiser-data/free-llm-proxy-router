// Package scan implements provider-specific free-model discovery.
package scan

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// ProviderScanner is implemented by each provider's scanner.
type ProviderScanner interface {
	ScanFreeModels(ctx context.Context, cfg config.ProviderConfig) ([]catalog.CatalogEntry, error)
}

// Dispatcher selects the correct ProviderScanner for each provider.
type Dispatcher struct {
	client *http.Client
}

// NewDispatcher creates a Dispatcher with a default HTTP client.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ScanAll runs all enabled provider scanners and merges the results.
func (d *Dispatcher) ScanAll(ctx context.Context, providers []config.ProviderConfig) (*catalog.Catalog, error) {
	cat := &catalog.Catalog{
		Version:   1,
		UpdatedAt: time.Now(),
	}

	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		scanner := d.scannerFor(p.ID)
		if scanner == nil {
			log.Printf("scan: no scanner registered for provider %q — skipping", p.ID)
			continue
		}

		log.Printf("scan: scanning provider %s ...", p.ID)
		entries, err := scanner.ScanFreeModels(ctx, p)
		if err != nil {
			log.Printf("scan: %s error: %v — continuing", p.ID, err)
			continue
		}
		cat.Entries = append(cat.Entries, entries...)
		log.Printf("scan: %s → %d entries", p.ID, len(entries))
	}
	return cat, nil
}

// scannerFor returns the ProviderScanner for the given provider ID.
func (d *Dispatcher) scannerFor(providerID string) ProviderScanner {
	switch providerID {
	case "openrouter":
		return &OpenRouterScanner{Client: d.client}
	case "groq":
		return &GroqScanner{Client: d.client}
	case "gemini":
		return &GeminiScanner{Client: d.client}
	case "huggingface":
		return &HuggingFaceScanner{Client: d.client}
	case "github-models":
		return &GitHubScanner{Client: d.client}
	default:
		return &GenericScanner{Client: d.client}
	}
}

// buildAuthHeader returns the (header-name, value) pair for a provider.
func buildAuthHeader(cfg config.ProviderConfig) (string, string) {
	return cfg.ResolvedAuth()
}

// makeRequest creates a GET request with provider auth headers applied.
func makeRequest(ctx context.Context, url string, cfg config.ProviderConfig) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	h, v := buildAuthHeader(cfg)
	if v != "" && v != "Bearer " {
		req.Header.Set(h, v)
	}

	for k, val := range cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}
	return req, nil
}

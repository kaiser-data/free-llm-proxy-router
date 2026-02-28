package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/config"
)

// Prober tests whether a specific model is actually accessible for free
// by sending a minimal chat completions request (max_tokens=1).
type Prober struct {
	Client *http.Client
}

// ProbeResult holds the outcome of a probe call.
type ProbeResult struct {
	ProviderID string
	ModelID    string
	IsAccessible bool
	StatusCode   int
	ErrMsg       string
}

// Probe sends a minimal request to verify a model's free access status.
func (p *Prober) Probe(ctx context.Context, entry catalog.CatalogEntry, cfg config.ProviderConfig) ProbeResult {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/v1/chat/completions"

	payload := map[string]any{
		"model":      entry.ModelID,
		"max_tokens": 1,
		"messages": []map[string]any{
			{"role": "user", "content": "Hi"},
		},
	}
	if cfg.NativeFeatures.ServiceTier != "" {
		payload["service_tier"] = cfg.NativeFeatures.ServiceTier
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return ProbeResult{ProviderID: cfg.ID, ModelID: entry.ModelID, ErrMsg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	h, v := cfg.ResolvedAuth()
	if v != "" && v != "Bearer " {
		req.Header.Set(h, v)
	}
	for k, val := range cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}

	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req = req.WithContext(ctx2)

	resp, err := p.Client.Do(req)
	if err != nil {
		return ProbeResult{ProviderID: cfg.ID, ModelID: entry.ModelID, ErrMsg: err.Error()}
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// 200 or streaming 200 = accessible
	if resp.StatusCode == http.StatusOK {
		return ProbeResult{
			ProviderID:   cfg.ID,
			ModelID:      entry.ModelID,
			IsAccessible: true,
			StatusCode:   resp.StatusCode,
		}
	}

	// 429 on a "free" model → needs reverification (might be rate-limited, not paid)
	// 402/403 → not free
	return ProbeResult{
		ProviderID:   cfg.ID,
		ModelID:      entry.ModelID,
		IsAccessible: false,
		StatusCode:   resp.StatusCode,
		ErrMsg:       string(body),
	}
}

// resolveEnv looks up an environment variable.
// Placed here to be accessible to all scanner files in the package.
func resolveEnv(key string) string {
	// Import-free env lookup via the config package convention
	// Actual implementation uses os.Getenv
	return envLookup(key)
}

// envLookup is the package-level env lookup (wraps os.Getenv).
// Tests can replace this to inject values without touching real env vars.
var envLookup = osGetenv

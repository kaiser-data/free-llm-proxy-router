package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// RefreshDiff is the structured output from an LLM-powered catalog refresh.
// The LLM returns a diff, never a full replacement of the catalog.
type RefreshDiff struct {
	// Changed is a list of entries whose free status has changed.
	Changed []catalog.CatalogEntry `json:"changed"`
	// Added is a list of newly discovered free models.
	Added []catalog.CatalogEntry `json:"added"`
	// RemovedConfidence is a list of models the LLM thinks may no longer be free.
	// These are flagged for human review — NOT auto-deleted.
	RemovedConfidence []RemovedEntry `json:"removed_confidence"`
}

// RemovedEntry flags a model for human review (not auto-deleted).
type RemovedEntry struct {
	ProviderID string  `json:"provider_id"`
	ModelID    string  `json:"model_id"`
	Confidence float64 `json:"confidence"` // 0.0–1.0 that it's no longer free
	Reason     string  `json:"reason"`
}

// LLMRefresher uses an LLM to diff the catalog against live provider information.
type LLMRefresher struct {
	Client     *http.Client
	PromptFile string
	Cfg        *config.RefreshConfig
}

// Refresh calls the configured LLM with the refresh prompt, parses the diff,
// and merges it conservatively into the existing catalog.
func (r *LLMRefresher) Refresh(ctx context.Context, cat *catalog.Catalog, providers []config.ProviderConfig) (*RefreshDiff, error) {
	prompt, err := r.buildPrompt(cat)
	if err != nil {
		return nil, fmt.Errorf("building refresh prompt: %w", err)
	}

	// Find the configured refresh provider
	var refreshProvider config.ProviderConfig
	found := false
	for _, p := range providers {
		if p.ID == r.Cfg.Provider {
			refreshProvider = p
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("refresh provider %q not found in config", r.Cfg.Provider)
	}

	// Call the LLM
	response, err := r.callLLM(ctx, refreshProvider, r.Cfg.Model, prompt)
	if err != nil {
		return nil, fmt.Errorf("calling refresh LLM: %w", err)
	}

	// Parse the diff from the response
	diff, err := parseDiff(response)
	if err != nil {
		return nil, fmt.Errorf("parsing refresh diff: %w", err)
	}

	return diff, nil
}

// MergeDiff applies a RefreshDiff to a catalog conservatively:
// - Changed entries are updated
// - Added entries are appended
// - RemovedConfidence entries are flagged for reverification, NOT deleted
func MergeDiff(cat *catalog.Catalog, diff *RefreshDiff) {
	for _, changed := range diff.Changed {
		found := false
		for i := range cat.Entries {
			if cat.Entries[i].ProviderID == changed.ProviderID &&
				cat.Entries[i].ModelID == changed.ModelID {
				cat.Entries[i] = changed
				found = true
				break
			}
		}
		if !found {
			cat.Entries = append(cat.Entries, changed)
		}
	}

	for _, added := range diff.Added {
		if cat.Find(added.ProviderID, added.ModelID) == nil {
			added.DiscoveredAt = time.Now()
			cat.Entries = append(cat.Entries, added)
		}
	}

	// Flag removed-confidence entries for reverification
	for _, rc := range diff.RemovedConfidence {
		cat.MarkNeedsReverification(rc.ProviderID, rc.ModelID)
	}
}

func (r *LLMRefresher) buildPrompt(cat *catalog.Catalog) (string, error) {
	templateData, err := os.ReadFile(r.PromptFile)
	if err != nil {
		return "", fmt.Errorf("reading prompt file %s: %w", r.PromptFile, err)
	}

	// Summarise the current catalog for the LLM
	summary := catalogSummary(cat)
	prompt := strings.ReplaceAll(string(templateData), "{{CATALOG_SUMMARY}}", summary)
	return prompt, nil
}

func catalogSummary(cat *catalog.Catalog) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Current catalog has %d entries:\n", len(cat.Entries)))
	for _, e := range cat.Entries {
		sb.WriteString(fmt.Sprintf("- %s/%s (free=%v, tier=%s)\n",
			e.ProviderID, e.ModelID, e.IsFree, e.TierType))
	}
	return sb.String()
}

func (r *LLMRefresher) callLLM(ctx context.Context, provider config.ProviderConfig, model, prompt string) (string, error) {
	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/v1/chat/completions"

	payload := map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	h, v := provider.ResolvedAuth()
	if v != "" && v != "Bearer " {
		req.Header.Set(h, v)
	}
	for k, val := range provider.ExtraHeaders {
		req.Header.Set(k, val)
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM call failed: status %d: %s", resp.StatusCode, body)
	}

	var llmResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &llmResp); err != nil {
		return "", fmt.Errorf("parsing LLM response: %w", err)
	}
	if len(llmResp.Choices) == 0 {
		return "", fmt.Errorf("no choices in LLM response")
	}
	return llmResp.Choices[0].Message.Content, nil
}

// parseDiff extracts a RefreshDiff from an LLM response.
// The LLM is expected to return JSON, but we scan for the first JSON block.
func parseDiff(content string) (*RefreshDiff, error) {
	// Find JSON object in the response
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("no JSON found in LLM response")
	}
	jsonStr := content[start : end+1]

	var diff RefreshDiff
	if err := json.Unmarshal([]byte(jsonStr), &diff); err != nil {
		return nil, fmt.Errorf("parsing diff JSON: %w", err)
	}
	return &diff, nil
}

package ratelimit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// UsageSnapshot is a serialisable record of per-provider usage, stored in usage.json.
// It survives proxy restarts.
type UsageSnapshot struct {
	SavedAt   time.Time                     `json:"saved_at"`
	Providers map[string]ProviderUsage       `json:"providers"`
}

// ProviderUsage holds the saved window counts for a provider.
type ProviderUsage struct {
	// ModelMinuteCounts: model_id → request count within the current minute window
	ModelMinuteCounts map[string]int `json:"model_minute_counts"`
	// ModelDayCounts: model_id → request count within the current day window
	ModelDayCounts map[string]int `json:"model_day_counts"`
	// WindowEndMinute: end of the current minute window (Unix seconds)
	WindowEndMinute int64 `json:"window_end_minute"`
	// WindowEndDay: end of the current day window (Unix seconds)
	WindowEndDay int64 `json:"window_end_day"`
}

// LoadUsage reads a UsageSnapshot from the given path.
func LoadUsage(path string) (*UsageSnapshot, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		if os.IsNotExist(err) {
			return &UsageSnapshot{Providers: make(map[string]ProviderUsage)}, nil
		}
		return nil, fmt.Errorf("reading usage %s: %w", path, err)
	}
	var u UsageSnapshot
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("parsing usage %s: %w", path, err)
	}
	return &u, nil
}

// SaveUsage atomically writes a UsageSnapshot to the given path.
func SaveUsage(u *UsageSnapshot, path string) error {
	expanded := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return fmt.Errorf("creating usage dir: %w", err)
	}
	u.SavedAt = time.Now()
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding usage: %w", err)
	}
	tmp := expanded + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing usage tmp: %w", err)
	}
	return os.Rename(tmp, expanded)
}

func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

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
	// ModelDayEnds: model_id → end of that model's day window (Unix seconds).
	// Day windows start at each model's first request, so they differ per model.
	ModelDayEnds map[string]int64 `json:"model_day_ends,omitempty"`
}

// Snapshot captures the per-model daily request counts of every provider.
// Minute and second windows are not worth persisting: they have expired by
// the time the proxy restarts.
func (g *GlobalTracker) Snapshot() *UsageSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	u := &UsageSnapshot{Providers: make(map[string]ProviderUsage)}
	now := time.Now()
	for id, t := range g.trackers {
		t.mu.Lock()
		pu := ProviderUsage{ModelDayCounts: map[string]int{}, ModelDayEnds: map[string]int64{}}
		for model, w := range t.perModelDay {
			if w.Count == 0 || now.After(w.WindowEnd) {
				continue
			}
			pu.ModelDayCounts[model] = w.Count
			pu.ModelDayEnds[model] = w.WindowEnd.Unix()
			if end := w.WindowEnd.Unix(); end > pu.WindowEndDay {
				pu.WindowEndDay = end
			}
		}
		t.mu.Unlock()
		if len(pu.ModelDayCounts) > 0 {
			u.Providers[id] = pu
		}
	}
	return u
}

// Restore loads daily counts from a snapshot, skipping windows that have
// already ended, so daily quotas keep counting across restarts.
func (g *GlobalTracker) Restore(u *UsageSnapshot) {
	if u == nil {
		return
	}
	now := time.Now()
	for id, pu := range u.Providers {
		t := g.Provider(id)
		t.mu.Lock()
		for model, count := range pu.ModelDayCounts {
			end := pu.ModelDayEnds[model]
			if end == 0 {
				end = pu.WindowEndDay
			}
			windowEnd := time.Unix(end, 0)
			if count <= 0 || !windowEnd.After(now) {
				continue
			}
			t.perModelDay[model] = &Window{Count: count, WindowEnd: windowEnd, Size: 24 * time.Hour}
		}
		t.mu.Unlock()
	}
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

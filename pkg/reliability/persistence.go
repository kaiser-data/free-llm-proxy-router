package reliability

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ReliabilityFile is the on-disk format for reliability.json.
type ReliabilityFile struct {
	SavedAt time.Time      `json:"saved_at"`
	Stats   []ProviderStats `json:"stats"`
}

// Load reads reliability stats from a JSON file and applies them to the tracker.
func Load(t *Tracker, path string) error {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no file yet — not an error
		}
		return fmt.Errorf("reading reliability %s: %w", path, err)
	}
	var f ReliabilityFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parsing reliability %s: %w", path, err)
	}
	t.LoadFrom(f.Stats)
	return nil
}

// Save writes the tracker's current stats to a JSON file atomically.
func Save(t *Tracker, path string) error {
	expanded := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return fmt.Errorf("creating reliability dir: %w", err)
	}
	f := ReliabilityFile{
		SavedAt: time.Now(),
		Stats:   t.All(),
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding reliability: %w", err)
	}
	tmp := expanded + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing reliability tmp: %w", err)
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

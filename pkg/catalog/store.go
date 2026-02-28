package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/models"
)

var mu sync.RWMutex

// Load reads a Catalog from a JSON file.
func Load(path string) (*Catalog, error) {
	mu.RLock()
	defer mu.RUnlock()

	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		if os.IsNotExist(err) {
			return &Catalog{Version: 1}, nil
		}
		return nil, fmt.Errorf("reading catalog %s: %w", path, err)
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing catalog %s: %w", path, err)
	}
	return &c, nil
}

// Save atomically writes a Catalog to a JSON file.
func Save(c *Catalog, path string) error {
	mu.Lock()
	defer mu.Unlock()

	expanded := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return fmt.Errorf("creating catalog dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding catalog: %w", err)
	}

	tmp := expanded + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing catalog tmp: %w", err)
	}
	return os.Rename(tmp, expanded)
}

// LoadEnriched reads an EnrichedCatalog from a JSON file.
func LoadEnriched(path string) (*models.EnrichedCatalog, error) {
	mu.RLock()
	defer mu.RUnlock()

	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		if os.IsNotExist(err) {
			return &models.EnrichedCatalog{Version: 1}, nil
		}
		return nil, fmt.Errorf("reading enriched catalog %s: %w", path, err)
	}
	var ec models.EnrichedCatalog
	if err := json.Unmarshal(data, &ec); err != nil {
		return nil, fmt.Errorf("parsing enriched catalog %s: %w", path, err)
	}
	return &ec, nil
}

// SaveEnriched atomically writes an EnrichedCatalog to a JSON file.
func SaveEnriched(ec *models.EnrichedCatalog, path string) error {
	mu.Lock()
	defer mu.Unlock()

	expanded := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return fmt.Errorf("creating enriched dir: %w", err)
	}

	data, err := json.MarshalIndent(ec, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding enriched catalog: %w", err)
	}

	tmp := expanded + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing enriched tmp: %w", err)
	}
	return os.Rename(tmp, expanded)
}

// expandHome replaces ~ with the user's home directory.
func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

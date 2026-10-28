package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_FallsBackToBundledExample(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	writeFile(t, filepath.Join("configs", "config.example.yaml"),
		"proxy:\n  port: 9191\nproviders:\n  - id: groq\n    enabled: true\n")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.Port != 9191 {
		t.Errorf("port = %d, want 9191 from bundled example", cfg.Proxy.Port)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "groq" {
		t.Errorf("providers = %+v, want [groq] from bundled example", cfg.Providers)
	}
}

func TestLoad_UserConfigBeatsBundledExample(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	writeFile(t, filepath.Join("configs", "config.example.yaml"), "proxy:\n  port: 9191\n")
	writeFile(t, "config.yaml", "proxy:\n  port: 7070\n")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.Port != 7070 {
		t.Errorf("port = %d, want 7070 from user config.yaml", cfg.Proxy.Port)
	}
}

func TestCatalogReadPath(t *testing.T) {
	chdir(t, t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	userPath := "~/.free-llm-proxy-router/catalog.json"

	// Neither exists: configured path is returned unchanged.
	if got := CatalogReadPath(userPath, ""); got != userPath {
		t.Errorf("no catalogs: got %q, want %q", got, userPath)
	}

	// Only the bundled catalog exists: use it.
	bundled := filepath.Join("configs", "catalog.json")
	writeFile(t, bundled, "{}")
	if got := CatalogReadPath(userPath, ""); got != bundled {
		t.Errorf("bundled only: got %q, want %q", got, bundled)
	}

	// User catalog exists (after a scan): it wins over the bundled one.
	writeFile(t, filepath.Join(home, ".free-llm-proxy-router", "catalog.json"), "{}")
	if got := CatalogReadPath(userPath, ""); got != userPath {
		t.Errorf("user catalog present: got %q, want %q", got, userPath)
	}
}

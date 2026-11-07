package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWatch_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet", "created")
	stop, err := Watch(filepath.Join(dir, "catalog.json"), func(*Catalog) {})
	if err != nil {
		t.Fatalf("Watch on missing dir: %v", err)
	}
	defer stop()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("watch dir not created: %v", err)
	}
}

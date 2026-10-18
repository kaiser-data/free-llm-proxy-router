package config

import (
	"os"
	"path/filepath"
	"testing"
)

// chdir is t.Chdir for Go 1.23 (t.Chdir arrived in 1.24).
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestFindDataFile_NextToConfig(t *testing.T) {
	chdir(t, t.TempDir()) // no ./configs here
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	want := filepath.Join(dir, "capabilities.yaml")
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FindDataFile("capabilities.yaml", filepath.Join(dir, "config.yaml")); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFindDataFile_CwdWins(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll("configs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("configs", "families.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "families.yaml"), []byte("y"), 0o644)

	if got := FindDataFile("families.yaml", filepath.Join(other, "config.yaml")); got != filepath.Join("configs", "families.yaml") {
		t.Errorf("got %q, want ./configs/families.yaml", got)
	}
}

func TestFindDataFile_NotFound(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if got := FindDataFile("missing.yaml", ""); got != filepath.Join("configs", "missing.yaml") {
		t.Errorf("got %q", got)
	}
}

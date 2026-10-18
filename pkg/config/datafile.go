package config

import (
	"os"
	"path/filepath"
)

// FindDataFile locates a bundled data file (families.yaml, capabilities.yaml,
// refresh-prompt.txt, …) so the binaries work when run from any directory.
//
// Search order, first existing file wins:
//  1. ./configs/<name>              (running from a source checkout)
//  2. <configPath dir>/<name>       (next to an explicit --config file)
//  3. ~/.free-llm-proxy-router/<name>
//  4. <executable dir>/configs/<name> (release archive layout)
//
// If nothing is found, "configs/<name>" is returned so callers report the
// conventional path in their error message.
func FindDataFile(name, configPath string) string {
	candidates := []string{filepath.Join("configs", name)}
	if configPath != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(configPath), name))
	}
	candidates = append(candidates, filepath.Join(expandHome("~/.free-llm-proxy-router"), name))
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "configs", name))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return filepath.Join("configs", name)
}

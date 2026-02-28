package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// dotenvVars holds key=value pairs loaded from .env / .secrets files.
// Real environment variables always take precedence.
var (
	dotenvMu   sync.RWMutex
	dotenvVars = map[string]string{}
)

// loadDotenv reads one or more env files in order. Later files do NOT
// override earlier ones (first-wins). Real env vars always win over all files.
// Accepted syntax per line:
//
//	KEY=value
//	KEY="value with spaces"
//	KEY='value'
//	# comment
//	(blank lines ignored)
func loadDotenv(paths ...string) {
	merged := map[string]string{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		p = expandHome(p)
		f, err := os.Open(p)
		if err != nil {
			continue // file absent is fine
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := parseLine(line)
			if !ok {
				continue
			}
			if _, exists := merged[k]; !exists {
				merged[k] = v // first file wins
			}
		}
		f.Close()
	}

	dotenvMu.Lock()
	dotenvVars = merged
	dotenvMu.Unlock()
}

// parseLine splits "KEY=value" and strips surrounding quotes from value.
func parseLine(line string) (key, value string, ok bool) {
	idx := strings.IndexByte(line, '=')
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])

	// Strip surrounding quotes (" or ')
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}

	// Strip inline comment (only outside quotes)
	if !strings.HasPrefix(line[idx+1:], `"`) && !strings.HasPrefix(line[idx+1:], `'`) {
		if ci := strings.IndexByte(value, '#'); ci >= 0 {
			value = strings.TrimSpace(value[:ci])
		}
	}

	return key, value, key != ""
}

// dotenvLookup returns the value for key from loaded env files.
// Returns "" if not present.
func dotenvLookup(key string) string {
	dotenvMu.RLock()
	defer dotenvMu.RUnlock()
	return dotenvVars[key]
}

// defaultEnvPaths returns the standard search locations for env files.
// configDir is the directory of the loaded config file (may be "").
func defaultEnvPaths(configDir string) []string {
	home, _ := os.UserHomeDir()
	picoclaw := filepath.Join(home, ".picoclaw-free-llm")

	paths := []string{
		// Project-local (highest priority among files)
		".env",
		".secrets",
	}
	// Config-adjacent files
	if configDir != "" && configDir != "." {
		paths = append(paths,
			filepath.Join(configDir, ".env"),
			filepath.Join(configDir, ".secrets"),
		)
	}
	// User data directory
	paths = append(paths,
		filepath.Join(picoclaw, ".env"),
		filepath.Join(picoclaw, ".secrets"),
	)
	return paths
}

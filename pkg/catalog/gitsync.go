package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitSyncConfig controls catalog synchronisation via git or a raw remote URL.
//
// Push side (scanner machine):
//
//	After picoclaw-scan update, set auto_push: true to commit and push
//	data/catalog.json to the configured git remote.
//
// Pull side (proxy / other machines):
//
//	Set remote_url to the raw GitHub URL of catalog.json.  The proxy fetches
//	it every pull_interval and writes it to catalog.path — the existing
//	fsnotify watcher on that file triggers an automatic hot-reload.
//
//	Alternatively, set repo_path and the proxy will run `git pull` on that
//	directory instead (requires git and SSH/token access on every machine).
type GitSyncConfig struct {
	Enabled bool `mapstructure:"enabled"`

	// Push settings (scanner machine)
	RepoPath      string `mapstructure:"repo_path"`       // path to local git repo; defaults to "."
	CatalogInRepo string `mapstructure:"catalog_in_repo"` // relative path inside repo; defaults to "data/catalog.json"
	AutoPush      bool   `mapstructure:"auto_push"`

	// Pull settings (proxy / other machines)
	PullInterval string `mapstructure:"pull_interval"` // e.g. "1h", "30m"; 0 = disabled
	RemoteURL    string `mapstructure:"remote_url"`    // raw URL to fetch catalog.json (no git needed)
}

// PushAfterScan copies localCatalogPath into the git repo and pushes.
// It is a no-op when cfg.Enabled or cfg.AutoPush is false.
func PushAfterScan(localCatalogPath string, cfg GitSyncConfig) error {
	if !cfg.Enabled || !cfg.AutoPush {
		return nil
	}

	repoPath := cfg.RepoPath
	if repoPath == "" {
		repoPath = "."
	}
	catalogInRepo := cfg.CatalogInRepo
	if catalogInRepo == "" {
		catalogInRepo = "data/catalog.json"
	}

	dst := filepath.Join(repoPath, catalogInRepo)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("gitsync: mkdir %s: %w", filepath.Dir(dst), err)
	}

	// Count entries for the commit message.
	count, _ := countEntries(localCatalogPath)

	if err := copyFile(localCatalogPath, dst); err != nil {
		return fmt.Errorf("gitsync: copy catalog: %w", err)
	}
	log.Printf("gitsync: copied catalog → %s (%d entries)", dst, count)

	ts := time.Now().UTC().Format("2006-01-02T15:04Z")
	msg := fmt.Sprintf("chore: catalog update %s (%d models)", ts, count)

	for _, args := range [][]string{
		{"add", catalogInRepo},
		{"commit", "--allow-empty", "-m", msg},
		{"push"},
	} {
		out, err := gitCmd(repoPath, args...)
		if err != nil {
			return fmt.Errorf("gitsync: git %s: %w\n%s", args[0], err, out)
		}
		log.Printf("gitsync: git %s ok", args[0])
	}
	return nil
}

// StartAutoPull begins a background goroutine that periodically syncs the
// catalog from a remote source.  It returns immediately.
//
//   - If cfg.RemoteURL is set: fetches the URL and overwrites localCatalogPath.
//   - Otherwise if cfg.RepoPath is set: runs `git pull` in that directory then
//     copies catalogInRepo → localCatalogPath if the file changed.
//
// The existing fsnotify watcher on localCatalogPath handles the hot-reload.
func StartAutoPull(ctx context.Context, localCatalogPath string, cfg GitSyncConfig) {
	if !cfg.Enabled {
		return
	}
	interval, err := time.ParseDuration(cfg.PullInterval)
	if err != nil || interval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		log.Printf("gitsync: auto-pull every %s", interval)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := pullOnce(localCatalogPath, cfg); err != nil {
					log.Printf("gitsync: pull error: %v", err)
				}
			}
		}
	}()
}

// pullOnce performs a single pull cycle.
func pullOnce(localCatalogPath string, cfg GitSyncConfig) error {
	if cfg.RemoteURL != "" {
		return fetchURL(cfg.RemoteURL, localCatalogPath)
	}

	// git pull fallback
	repoPath := cfg.RepoPath
	if repoPath == "" {
		return nil
	}
	catalogInRepo := cfg.CatalogInRepo
	if catalogInRepo == "" {
		catalogInRepo = "data/catalog.json"
	}

	out, err := gitCmd(repoPath, "pull", "--ff-only")
	if err != nil {
		return fmt.Errorf("git pull: %w\n%s", err, out)
	}
	if strings.Contains(out, "Already up to date") {
		return nil
	}
	log.Printf("gitsync: git pull: %s", strings.TrimSpace(out))

	src := filepath.Join(repoPath, catalogInRepo)
	return copyFile(src, localCatalogPath)
}

// fetchURL downloads url and atomically replaces localPath.
func fetchURL(url, localPath string) error {
	resp, err := http.Get(url) //nolint:gosec // URL from user config
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}

	// Write to a temp file then rename (atomic on POSIX).
	tmp := localPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()

	if err := os.Rename(tmp, localPath); err != nil {
		os.Remove(tmp)
		return err
	}

	count, _ := countEntries(localPath)
	log.Printf("gitsync: fetched %s → %s (%d entries)", url, localPath, count)
	return nil
}

// gitCmd runs a git command in dir and returns combined output.
func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// copyFile copies src to dst, creating parent dirs as needed.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	return os.Rename(tmp, dst)
}

// countEntries returns the number of entries in a catalog JSON file.
func countEntries(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var cat struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &cat); err != nil {
		return 0, err
	}
	return len(cat.Entries), nil
}

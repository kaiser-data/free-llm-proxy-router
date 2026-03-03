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
	"time"
)

// GitSyncConfig defines one of two mutually exclusive roles:
//
// SCANNER (one machine only):
//
//	auto_push: true
//	scan_interval: "168h"     ← runs free-llm-scan update every week, then git push
//	repo_path: "."
//	catalog_in_repo: "data/catalog.json"
//
// REPLICA (all other machines):
//
//	remote_url: "https://raw.githubusercontent.com/.../data/catalog.json"
//	pull_interval: "168h"     ← fetches the URL weekly; proxy hot-reloads on change
//
// Setting both auto_push and remote_url on the same machine is not supported.
type GitSyncConfig struct {
	Enabled bool `mapstructure:"enabled"`

	// Scanner settings — only set on the machine that runs free-llm-scan.
	RepoPath      string `mapstructure:"repo_path"`        // path to local git repo; defaults to "."
	CatalogInRepo string `mapstructure:"catalog_in_repo"`  // file path inside repo; defaults to "data/catalog.json"
	AutoPush      bool   `mapstructure:"auto_push"`        // commit+push after every scan
	ScanInterval  string `mapstructure:"scan_interval"`    // run a full scan every N hours; e.g. "168h"

	// Replica settings — set on every other machine.
	RemoteURL    string `mapstructure:"remote_url"`     // raw GitHub URL of data/catalog.json
	PullInterval string `mapstructure:"pull_interval"`  // how often to fetch; e.g. "168h"
}

// ScanFunc is the callback StartSync uses to trigger a full provider scan.
// It must save the updated catalog to localCatalogPath before returning.
type ScanFunc func(ctx context.Context) error

// StartSync starts the appropriate background goroutine based on config:
//   - Scanner role (auto_push + scan_interval): runs scanFn every scan_interval, then pushes.
//   - Replica role (remote_url + pull_interval): fetches remote_url every pull_interval.
//
// The function returns immediately. The fsnotify watcher on localCatalogPath
// handles hot-reload whenever the file changes.
func StartSync(ctx context.Context, localCatalogPath string, cfg GitSyncConfig, scanFn ScanFunc) {
	if !cfg.Enabled {
		return
	}

	if cfg.AutoPush && cfg.ScanInterval != "" {
		startScannerLoop(ctx, localCatalogPath, cfg, scanFn)
		return
	}

	if cfg.RemoteURL != "" && cfg.PullInterval != "" {
		startReplicaLoop(ctx, localCatalogPath, cfg)
		return
	}

	log.Printf("gitsync: enabled but no valid role configured — set scan_interval+auto_push (scanner) or remote_url+pull_interval (replica)")
}

// PushAfterScan copies localCatalogPath into the git repo and pushes.
// Called by free-llm-scan after a manual `update` run when auto_push is true.
func PushAfterScan(localCatalogPath string, cfg GitSyncConfig) error {
	if !cfg.Enabled || !cfg.AutoPush {
		return nil
	}
	return gitCommitPush(localCatalogPath, cfg)
}

// startScannerLoop runs scanFn every scan_interval, then pushes to git.
func startScannerLoop(ctx context.Context, localCatalogPath string, cfg GitSyncConfig, scanFn ScanFunc) {
	interval, err := time.ParseDuration(cfg.ScanInterval)
	if err != nil || interval <= 0 {
		log.Printf("gitsync: invalid scan_interval %q — scanner disabled", cfg.ScanInterval)
		return
	}

	go func() {
		// Check if catalog is already fresh enough to skip the first scan.
		age := catalogAge(localCatalogPath)
		firstScan := interval
		if age >= interval {
			firstScan = 0 // overdue — scan immediately
		} else {
			firstScan = interval - age
		}

		log.Printf("gitsync: scanner role — next scan in %s, then every %s", firstScan.Round(time.Minute), interval)

		timer := time.NewTimer(firstScan)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				log.Printf("gitsync: starting scheduled scan...")
				scanCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				if err := scanFn(scanCtx); err != nil {
					log.Printf("gitsync: scan failed: %v", err)
				} else if err := gitCommitPush(localCatalogPath, cfg); err != nil {
					log.Printf("gitsync: push failed: %v", err)
				}
				cancel()
				timer.Reset(interval)
			}
		}
	}()
}

// startReplicaLoop fetches remote_url every pull_interval.
func startReplicaLoop(ctx context.Context, localCatalogPath string, cfg GitSyncConfig) {
	interval, err := time.ParseDuration(cfg.PullInterval)
	if err != nil || interval <= 0 {
		log.Printf("gitsync: invalid pull_interval %q — replica disabled", cfg.PullInterval)
		return
	}

	// Also check catalog age to stagger the first pull.
	age := catalogAge(localCatalogPath)
	firstPull := interval
	if age >= interval {
		firstPull = 0
	} else {
		firstPull = interval - age
	}

	log.Printf("gitsync: replica role — next pull in %s, then every %s", firstPull.Round(time.Minute), interval)

	go func() {
		timer := time.NewTimer(firstPull)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if err := fetchURL(cfg.RemoteURL, localCatalogPath); err != nil {
					log.Printf("gitsync: pull error: %v", err)
				}
				timer.Reset(interval)
			}
		}
	}()
}

// gitCommitPush copies localCatalogPath into the repo and runs git add/commit/push.
func gitCommitPush(localCatalogPath string, cfg GitSyncConfig) error {
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
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}

	count, _ := countEntries(localCatalogPath)
	if err := copyFile(localCatalogPath, dst); err != nil {
		return fmt.Errorf("copy catalog: %w", err)
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04Z")
	msg := fmt.Sprintf("chore: catalog update %s (%d models)", ts, count)

	for _, args := range [][]string{
		{"add", catalogInRepo},
		{"commit", "--allow-empty", "-m", msg},
		{"push"},
	} {
		out, err := gitCmd(repoPath, args...)
		if err != nil {
			return fmt.Errorf("git %s: %w\n%s", args[0], err, out)
		}
		log.Printf("gitsync: git %s ok", args[0])
	}
	log.Printf("gitsync: pushed %d models to %s:%s", count, repoPath, catalogInRepo)
	return nil
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
	log.Printf("gitsync: pulled %d models from %s", count, url)
	return nil
}

// catalogAge returns how long ago the catalog file was last modified.
func catalogAge(path string) time.Duration {
	info, err := os.Stat(expandHome(path))
	if err != nil {
		return 999 * time.Hour // treat missing as very old
	}
	return time.Since(info.ModTime())
}

// gitCmd runs a git command in dir and returns combined output.
func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// copyFile copies src to dst atomically.
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
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return 0, err
	}
	var c struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return 0, err
	}
	return len(c.Entries), nil
}

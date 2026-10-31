package catalog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const validCatalog = `{"version":1,"entries":[{"provider_id":"groq","model_id":"llama","is_free":true}]}`

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFetchURL_RejectsBadPayloadAndKeepsLocalFile(t *testing.T) {
	cases := map[string]string{
		"not json":   "<html>rate limited</html>",
		"no entries": `{"version":1,"entries":[]}`,
		"too large":  `{"entries":[` + strings.Repeat(" ", maxRemoteCatalogBytes) + `]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(payload))
			}))
			defer srv.Close()

			local := filepath.Join(t.TempDir(), "catalog.json")
			writeFile(t, local, validCatalog)
			if err := fetchURL(srv.URL, local); err == nil {
				t.Fatal("fetchURL accepted a bad payload")
			}
			got, _ := os.ReadFile(local)
			if string(got) != validCatalog {
				t.Errorf("local catalog was overwritten: %.80s", got)
			}
		})
	}
}

func TestFetchURL_ReplacesLocalFileWithValidCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(validCatalog))
	}))
	defer srv.Close()

	local := filepath.Join(t.TempDir(), "catalog.json")
	writeFile(t, local, `{"entries":[]}`)
	if err := fetchURL(srv.URL, local); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(local); string(got) != validCatalog {
		t.Errorf("local = %s", got)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCmd(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// A scheduled scan must not commit whatever else happens to be staged.
func TestGitCommitPush_CommitsOnlyTheCatalog(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	remote, repo := t.TempDir(), t.TempDir()
	git(t, remote, "init", "-q", "--bare")
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "test")
	git(t, repo, "remote", "add", "origin", remote)
	writeFile(t, filepath.Join(repo, "README"), "x")
	git(t, repo, "add", "README")
	git(t, repo, "commit", "-q", "-m", "init")
	git(t, repo, "push", "-q", "-u", "origin", "HEAD")

	// Unrelated work in progress, already staged.
	writeFile(t, filepath.Join(repo, "wip.txt"), "KEY=1")
	git(t, repo, "add", "wip.txt")

	local := filepath.Join(t.TempDir(), "catalog.json")
	writeFile(t, local, validCatalog)
	cfg := GitSyncConfig{Enabled: true, AutoPush: true, RepoPath: repo}
	if err := gitCommitPush(local, cfg); err != nil {
		t.Fatal(err)
	}

	files := git(t, repo, "show", "--name-only", "--format=", "HEAD")
	if strings.TrimSpace(files) != "data/catalog.json" {
		t.Errorf("commit contains %q, want only data/catalog.json", files)
	}
	if staged := git(t, repo, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "wip.txt" {
		t.Errorf("wip.txt should stay staged, got %q", staged)
	}

	// A second run with an unchanged catalog must not fail.
	if err := gitCommitPush(local, cfg); err != nil {
		t.Errorf("unchanged catalog: %v", err)
	}
}

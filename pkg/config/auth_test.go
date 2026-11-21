package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAuthConfig writes a minimal config with a literal auth_token and
// returns its path.
func writeAuthConfig(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "proxy:\n  auth_token: \"" + token + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestAuthTokenEnvOverride(t *testing.T) {
	path := writeAuthConfig(t, "from-file")

	t.Setenv("PROXY_AUTH_TOKEN", "from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.AuthToken != "from-env" {
		t.Errorf("auth_token = %q, want env value %q", cfg.Proxy.AuthToken, "from-env")
	}
}

func TestAuthTokenFromFileWithoutEnv(t *testing.T) {
	path := writeAuthConfig(t, "from-file")

	t.Setenv("PROXY_AUTH_TOKEN", "") // ensure no ambient value interferes
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.AuthToken != "from-file" {
		t.Errorf("auth_token = %q, want file value %q", cfg.Proxy.AuthToken, "from-file")
	}
}

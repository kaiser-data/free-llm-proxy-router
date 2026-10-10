package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// TestUpstreamClientTimeoutsFromConfig locks in the G5 wiring: the buffered
// upstream client and the streaming client read their timeouts from config
// (previously hardcoded 120s at two sites), the stream timeout inherits the
// provider timeout when unset, zero/negative config values fall back to the
// 120s default, and an injected chainHTTPClient still wins for tests.
func TestUpstreamClientTimeoutsFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Fallback.ProviderTimeoutSeconds = 7
	cfg.Fallback.StreamTimeoutSeconds = 11
	s := NewServer(cfg, &catalog.Catalog{}, nil, nil, nil, nil)

	if got := s.upstreamHTTPClient().Timeout; got != 7*time.Second {
		t.Errorf("buffered client timeout = %v, want 7s (provider_timeout_seconds)", got)
	}
	if got := s.streamHTTPClient().Timeout; got != 11*time.Second {
		t.Errorf("stream client timeout = %v, want 11s (stream_timeout_seconds)", got)
	}

	// stream_timeout_seconds = 0 → inherit the provider timeout (previous
	// behavior: one 120s client bounded both buffered calls and streams).
	cfg2 := &config.Config{}
	cfg2.Fallback.ProviderTimeoutSeconds = 9
	s2 := NewServer(cfg2, &catalog.Catalog{}, nil, nil, nil, nil)
	if got := s2.streamHTTPClient().Timeout; got != 9*time.Second {
		t.Errorf("stream client timeout = %v, want inherited 9s", got)
	}

	// Zero-valued config falls back to the 120s default.
	s3 := NewServer(&config.Config{}, &catalog.Catalog{}, nil, nil, nil, nil)
	if got := s3.upstreamHTTPClient().Timeout; got != 120*time.Second {
		t.Errorf("default buffered timeout = %v, want 120s", got)
	}
	if got := s3.streamHTTPClient().Timeout; got != 120*time.Second {
		t.Errorf("default stream timeout = %v, want 120s", got)
	}

	// Test injection still wins over config.
	s.chainHTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	if got := s.upstreamHTTPClient().Timeout; got != 50*time.Millisecond {
		t.Errorf("injected client timeout = %v, want 50ms", got)
	}
	if got := s.streamHTTPClient().Timeout; got != 50*time.Millisecond {
		t.Errorf("injected stream client timeout = %v, want 50ms", got)
	}
}

// TestProviderConfigResolvedTimeout: providers[].timeout is parsed into a
// per-attempt Duration; unset/invalid values yield 0 so callers fall back to
// the global timeout. The field existed but was never read before.
func TestProviderConfigResolvedTimeout(t *testing.T) {
	cases := map[string]time.Duration{
		"":      0,
		"30s":   30 * time.Second,
		"90s":   90 * time.Second,
		"1m30s": 90 * time.Second,
		"bogus": 0,
		"-5s":   0,
		"0s":    0,
	}
	for in, want := range cases {
		p := config.ProviderConfig{Timeout: in}
		if got := p.ResolvedTimeout(); got != want {
			t.Errorf("ResolvedTimeout(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestProviderTimeoutBoundsUpstreamCall proves the per-provider deadline
// actually aborts a hanging upstream even when the shared client has no
// timeout of its own.
func TestProviderTimeoutBoundsUpstreamCall(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hang until the test releases it
	}))
	defer srv.Close()
	defer close(block) // runs before srv.Close (LIFO) so Close can finish

	fc, _, _ := newTestChain(srv.URL, "testprovider", "test-model")
	fc.Cfg.Providers[0].Timeout = "100ms"
	fc.HTTPClient = &http.Client{} // no client-level timeout — only the ctx deadline

	body := map[string]any{
		"model":    "test-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	_, err := fc.callProvider(context.Background(), fc.Cfg.Providers[0], body)
	if err == nil {
		t.Fatal("expected a timeout error from the bounded upstream call")
	}
}

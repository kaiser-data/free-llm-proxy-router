package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// A 400 about the request (context too long) must not take the model away
// from other clients, and the client gets the 400 rather than a 503 to retry.
func TestFallbackChain_RequestShaped400PassesThroughWithoutCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "testprovider", "m")
	resp, err := fc.Execute(context.Background(), hiReq())
	if err != nil || resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(resp.Body), "context length") {
		t.Fatalf("want the upstream 400 passed through, got resp=%v err=%v", resp, err)
	}
	if rl.IsOnCooldown("testprovider/m") {
		t.Error("a request-shaped 400 cooled the model down for everyone")
	}
}

// A 400 from one provider and a 5xx from another is not the client's fault:
// that stays a 503.
func TestFallbackChain_MixedFailuresStay503(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"unsupported parameter"}}`, http.StatusBadRequest)
	}))
	defer bad.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
	}))
	defer down.Close()

	fc := twoProviderChain(bad.URL, down.URL)
	fc.Stream = false
	if resp, err := fc.Execute(context.Background(), hiReq()); err == nil {
		t.Fatalf("want exhaustion error, got status %d", resp.StatusCode)
	}
}

func TestIsModelSpecific400(t *testing.T) {
	for body, want := range map[string]bool{
		`{"error":{"message":"This model only supports Interactions API."}}`:      true,
		`{"error":{"message":"The model llama-3.1-70b has been decommissioned"}}`: true,
		`{"error":{"code":"model_not_found"}}`:                                    true,
		`{"error":{"message":"maximum context length is 8192 tokens"}}`:           false,
		`{"error":{"message":"tools[0].function.parameters is invalid"}}`:         false,
		`{"error":{"message":"'stream_options' is only allowed when stream"}}`:    false,
	} {
		if got := isModelSpecific400([]byte(body)); got != want {
			t.Errorf("isModelSpecific400(%s) = %v, want %v", body, got, want)
		}
	}
}

// A revoked first key must not take the provider down while key 2 works.
func TestFallbackChain_401OnOneKeyUsesNextKey(t *testing.T) {
	t.Setenv("FLPR_TEST_AUTH_KEY", "revoked")
	t.Setenv("FLPR_TEST_AUTH_KEY_2", "good")
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer revoked" {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		io.WriteString(w, okBody)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "testprovider", "m")
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_TEST_AUTH_KEY"
	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatalf("key 2 should have answered: %v (calls %v)", err, calls)
	}
	if rl.IsOnCooldown("testprovider") {
		t.Error("one bad key put the whole provider on cooldown")
	}
	calls = nil
	if _, err := fc.Execute(context.Background(), hiReq()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "Bearer good" {
		t.Errorf("second request tried %v, want only the good key", calls)
	}
}

func TestFallbackChain_5xxCoolsDownModelBriefly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	fc, rl, _ := newTestChain(srv.URL, "testprovider", "m")
	fc.Execute(context.Background(), hiReq())
	if !rl.IsOnCooldown("testprovider/m") {
		t.Error("a 5xx model should rest briefly")
	}
	if rate := fc.ReliabilityTracker.SuccessRate("testprovider"); rate >= 1 {
		t.Errorf("5xx not counted against reliability (rate %v)", rate)
	}
}

func TestLogRequestsAndWriteTimeout(t *testing.T) {
	for level, want := range map[string]bool{"": true, "info": true, "debug": true, "warn": false, "ERROR": false} {
		if logRequests(level) != want {
			t.Errorf("logRequests(%q) != %v", level, want)
		}
	}
	if writeTimeout(0) != noDeadlineWriteTimeout || writeTimeout(90*time.Second) != 135*time.Second {
		t.Error("writeTimeout must follow request_timeout")
	}
}

// SIGTERM (ctx cancel) must let an in-flight request finish before Start returns.
func TestServer_ShutdownDrainsInFlightRequests(t *testing.T) {
	var started atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Store(true)
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, okBody)
	}))
	defer upstream.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "alpha", BaseURL: upstream.URL, Enabled: true}},
		Fallback:  config.FallbackConfig{MaxAttempts: 3},
	}
	cfg.Proxy.Strategy, cfg.Proxy.Host, cfg.Proxy.Port = "adaptive", "127.0.0.1", port
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{{ProviderID: "alpha", ModelID: "m", IsFree: true}}}
	rl, rel := ratelimit.NewGlobalTracker(), reliability.New()
	s := NewServer(cfg, cat, strategy.NewRegistry(rel, rl, "", "", 3, 5), rl, nil, rel)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- s.Start(ctx) }()
	url := "http://127.0.0.1:" + strconv.Itoa(port)
	for i := 0; i < 100; i++ {
		if r, err := http.Get(url + "/health"); err == nil {
			r.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	result := make(chan int, 1)
	go func() {
		r, err := http.Post(url+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			result <- 0
			return
		}
		r.Body.Close()
		result <- r.StatusCode
	}()
	for !started.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case <-stopped:
		t.Fatal("Start returned while a request was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if code := <-result; code != http.StatusOK {
		t.Errorf("in-flight request got %d, want 200", code)
	}
	if err := <-stopped; err != nil {
		t.Errorf("Start: %v", err)
	}
}

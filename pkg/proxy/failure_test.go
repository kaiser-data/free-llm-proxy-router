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
	s, err := NewServer(cfg, cat, strategy.NewRegistry(rel, rl, "", "", 3, 5), rl, nil, rel)
	if err != nil {
		t.Fatal(err)
	}

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

// False positives here block a model for every client for an hour, so the
// classifier must stay conservative: when unsure, it is the request's fault.
func TestIsModelSpecific400_RequestShapedErrorsAreNotModelErrors(t *testing.T) {
	requestShaped := []string{
		`{"error":{"message":"tools is not supported by this model"}}`,         // client sent tools
		`{"error":{"message":"This model only supports text input"}}`,          // client sent an image
		`{"error":{"message":"This model only supports up to 8192 tokens"}}`,   // prompt too long
		`{"error":{"message":"'functions' has been deprecated, use 'tools'"}}`, // deprecated parameter
		`{"error":{"message":"response_format is not supported for this model"}}`,
		`{"error":{"message":"Invalid model input: messages[1].content"}}`,
		`{"error":{"message":"Image input is not supported by this model"}}`,
		`{"error":{"message":"This model only supports a single image per request"}}`,
		`{"error":{"message":"'logprobs' is no longer supported"}}`,
		`{"error":{"message":"Invalid model parameters: temperature must be <= 1"}}`,
	}
	for _, body := range requestShaped {
		if isModelSpecific400([]byte(body)) {
			t.Errorf("request-shaped 400 classified as model-specific: %s", body)
		}
	}
	modelShaped := []string{
		`{"error":{"code":400,"message":"This model only supports Interactions API."}}`,
		`{"error":{"message":"The model llama-3.1-70b-versatile has been decommissioned and is no longer supported."}}`,
		`{"error":{"message":"The model ` + "`gpt-x`" + ` does not exist or you do not have access to it.","code":"model_not_found"}}`,
		`{"error":{"message":"This model is only supported in v1/responses and not in v1/chat/completions."}}`,
		`{"error":{"message":"foo/bar is not a valid model ID"}}`,
	}
	for _, body := range modelShaped {
		if !isModelSpecific400([]byte(body)) {
			t.Errorf("model-specific 400 not recognised: %s", body)
		}
	}
}

// Once every key of a provider is known bad, later requests skip the provider
// outright: no calls, and its other models are not walked one by one.
func TestFallbackChain_AllKeysBadSkipsProvider(t *testing.T) {
	t.Setenv("FLPR_TEST_BAD_KEY", "bad1")
	t.Setenv("FLPR_TEST_BAD_KEY_2", "bad2")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	fc, _, _ := newTestChain(srv.URL, "testprovider", "a")
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_TEST_BAD_KEY"
	fc.Execute(context.Background(), hiReq())
	if n := calls.Load(); n != 2 {
		t.Fatalf("first request made %d calls, want 2 (one per key)", n)
	}

	res := fc.attempt(context.Background(), "testprovider", "b", fc.buildBody("testprovider", nil, "b"), 0)
	if res.outcome != outSkipProvider || res.calls != 0 {
		t.Errorf("attempt with every key bad = outcome %v, %d calls; want outSkipProvider, 0 calls", res.outcome, res.calls)
	}
}

// A 401 on key 1 and a 429 on key 2: the provider has no usable key right
// now, the request moves on, and neither key is retried in this request.
func TestFallbackChain_Mixed401And429MovesOn(t *testing.T) {
	t.Setenv("FLPR_TEST_MIX_KEY", "revoked")
	t.Setenv("FLPR_TEST_MIX_KEY_2", "limited")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer revoked" {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Retry-After", "60")
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, okBody) }))
	defer good.Close()

	fc := twoProviderChain(srv.URL, good.URL)
	fc.Stream = false
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_TEST_MIX_KEY"
	resp, err := fc.Execute(context.Background(), hiReq())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("beta should answer: resp=%v err=%v", resp, err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("alpha got %d calls, want 2 (each key once)", n)
	}
}

// A model-specific 400 on the only model: not the client's fault, so a 503
// (the next request reaches another model), and the model sits out.
func TestFallbackChain_ModelSpecific400IsNotPassedThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"The model m has been decommissioned"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	fc, rl, _ := newTestChain(srv.URL, "p", "m")
	if resp, err := fc.Execute(context.Background(), hiReq()); err == nil {
		t.Fatalf("got status %d, want exhaustion (503)", resp.StatusCode)
	}
	if !rl.IsOnCooldown("p/m") {
		t.Error("decommissioned model not cooled down")
	}
}

// One provider rejects the request for its own quirk while a working provider
// is only on cooldown: retrying later can succeed, so the client gets a 503.
func TestFallbackChain_400StaysRetryableWhileAProviderIsOnCooldown(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"'tool_choice' value 'required' is not supported"}}`, 400)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody)
	}))
	defer good.Close()
	fc := twoProviderChain(good.URL, bad.URL)
	fc.Stream = false
	fc.RateLimiter.SetCooldown("alpha", time.Now().Add(time.Minute)) // e.g. 429 cooldown from another request
	resp, err := fc.Execute(context.Background(), hiReq())
	if err == nil {
		t.Errorf("client gets status %d (non-retryable) while a working provider is only on cooldown: %s", resp.StatusCode, resp.Body)
	}
}

// Provider: UnoRouter (verified 2026-11-20): a 403 consent_required means
// the account has not consented to this model's upstream provider yet. The
// key is fine and models behind already-consented providers still answer,
// so only the model sits out — a provider-wide cooldown would hide them all.
func TestFallbackChain_Consent403CoolsModelNotProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"consent_required","message":"m:free needs your consent for these providers: cfp (United States). Give it once at https://unorouter.com/consents"}}`, http.StatusForbidden)
	}))
	defer srv.Close()
	fc, rl, _ := newTestChain(srv.URL, "unorouter", "m:free")
	if resp, err := fc.Execute(context.Background(), hiReq()); err == nil {
		t.Fatalf("got status %d, want exhaustion (503)", resp.StatusCode)
	}
	if !rl.IsOnCooldown("unorouter/m:free") {
		t.Error("consent-gated model not cooled down")
	}
	if rl.IsOnCooldown("unorouter") {
		t.Error("consent 403 put the whole provider on auth cooldown")
	}
}

// A consent 403 is not a key rejection: the same key serves models whose
// upstream providers are already consented.
func TestIsKeyRejection_Consent403IsNotKeyRejection(t *testing.T) {
	consent := []byte(`{"error":{"code":"consent_required","message":"m needs your consent for these providers: cfp"}}`)
	if isKeyRejection(&Response{StatusCode: http.StatusForbidden, Body: consent}) {
		t.Error("consent_required 403 classified as key rejection")
	}
	if !isConsent403(consent) {
		t.Error("isConsent403 did not recognise the consent body")
	}
	credit := []byte(`{"error":{"message":"This provider ran out of credit on their side. That is on us, not you: retry and the request goes to another provider."}}`)
	if isKeyRejection(&Response{StatusCode: http.StatusForbidden, Body: credit}) {
		t.Error("upstream out-of-credit 403 classified as key rejection")
	}
	if !isUpstreamCredit403(credit) {
		t.Error("isUpstreamCredit403 did not recognise the credit body")
	}
	if !isKeyRejection(&Response{StatusCode: http.StatusForbidden, Body: []byte(`{"error":{"message":"invalid api key"}}`)}) {
		t.Error("plain 403 not classified as key rejection")
	}
}

// An upstream out-of-credit 403 is upstream capacity, not the key: the
// model rests briefly like after a 5xx, and the key stays usable for the
// models whose upstream providers are fine.
func TestFallbackChain_UpstreamCredit403CoolsModelNotKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"This provider ran out of credit on their side. That is on us, not you: retry and the request goes to another provider."}}`, http.StatusForbidden)
	}))
	defer srv.Close()
	fc, rl, _ := newTestChain(srv.URL, "unorouter", "m:free")
	if resp, err := fc.Execute(context.Background(), hiReq()); err == nil {
		t.Fatalf("got status %d, want exhaustion (503)", resp.StatusCode)
	}
	if !rl.IsOnCooldown("unorouter/m:free") {
		t.Error("upstream out-of-credit model not on cooldown")
	}
	if rl.IsOnCooldown("unorouter") {
		t.Error("upstream out-of-credit 403 cooled the key/provider")
	}
}

// OpenRouter moderation ("input was flagged") rejects the request, not the
// key: no key may be cooled down for every client because of one prompt.
func TestFallbackChain_Moderation403DoesNotCoolKeys(t *testing.T) {
	t.Setenv("FLPR_R5", "k1")
	t.Setenv("FLPR_R5_2", "k2")
	t.Setenv("FLPR_R5_3", "k3")
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":{"code":403,"message":"Your chosen model requires moderation and your input was flagged"}}`, 403)
	}))
	defer srv.Close()
	fc, rl, _ := newTestChain(srv.URL, "openrouter2", "m")
	fc.Cfg.Providers[0].APIKeyEnv = "FLPR_R5"
	fc.Execute(context.Background(), hiReq())
	n := 0
	for s := 0; s < 3; s++ {
		if rl.IsOnCooldown(authCooldownKey("openrouter2", s, 3)) {
			n++
		}
	}
	t.Logf("calls=%d keys cooled=%d", calls, n)
	if n == 3 {
		t.Errorf("one flagged prompt put all 3 keys on 10min cooldown for every client")
	}
}

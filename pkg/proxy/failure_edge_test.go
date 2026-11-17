package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

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

package proxy

import (
	"context"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClassifyFailure(t *testing.T) {
	resp := func(status int, body string) *Response {
		return &Response{StatusCode: status, Body: []byte(body), Header: http.Header{}}
	}
	cases := []struct {
		name string
		resp *Response
		want failureVerdict
	}{
		{"transport error", nil,
			failureVerdict{outcome: outModelFail, modelCooldown: transportModelCooldown, countFailure: true, coolIfUnreliable: true}},
		{"401 key rejected", resp(401, `{"error":"invalid api key"}`),
			failureVerdict{outcome: outProviderFail, countFailure: true, coolIfUnreliable: true}},
		{"403 key rejected", resp(403, `{"error":"forbidden"}`),
			failureVerdict{outcome: outProviderFail, countFailure: true, coolIfUnreliable: true}},
		{"403 consent", resp(403, `{"error":{"code":"consent_required"}}`),
			failureVerdict{outcome: outModelFail, modelCooldown: consentCooldown}},
		{"403 upstream credit", resp(403, `This provider ran out of credit on their side`),
			failureVerdict{outcome: outModelFail, modelCooldown: serverErrorCooldown, countFailure: true}},
		{"403 moderation", resp(403, `input was flagged by moderation`),
			failureVerdict{outcome: outProviderFail}},
		{"400 names the model", resp(400, `model has been decommissioned`),
			failureVerdict{outcome: outModelFail, modelCooldown: badRequestCooldown}},
		{"400 blames the request", resp(400, `context length exceeded`),
			failureVerdict{outcome: outProviderFail}},
		{"413", resp(413, ``), failureVerdict{outcome: outProviderFail}},
		{"422", resp(422, ``), failureVerdict{outcome: outProviderFail}},
		{"404", resp(404, ``), failureVerdict{outcome: outModelFail, modelCooldown: missingModelCooldown}},
		{"429", resp(429, ``), failureVerdict{outcome: outModelFail}},
		{"502", resp(502, ``),
			failureVerdict{outcome: outModelFail, modelCooldown: serverErrorCooldown, countFailure: true, coolIfUnreliable: true}},
		{"other 4xx", resp(409, ``), failureVerdict{outcome: outModelFail}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyFailure(c.resp)
			if got.reason == "" {
				t.Error("verdict has no log reason")
			}
			got.reason = ""
			if got != c.want {
				t.Errorf("classifyFailure = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestQuirks_HuggingFaceRetriesOnlyALoading503(t *testing.T) {
	loading := &Response{StatusCode: 503, Body: []byte(`{"error":"Model is loading","estimated_time":1.5}`)}
	if got := huggingFaceRetryWait(loading); got != 1500*time.Millisecond {
		t.Errorf("loading 503: wait %s, want 1.5s", got)
	}
	if got := huggingFaceRetryWait(&Response{StatusCode: 503, Body: []byte(`overloaded`)}); got != 0 {
		t.Errorf("plain 503: wait %s, want no retry", got)
	}
	if got := huggingFaceRetryWait(&Response{StatusCode: 500, Body: []byte(`loading`)}); got != 0 {
		t.Errorf("500: wait %s, want no retry", got)
	}
}

func TestQuirks_OpenRouterCountsTheModelItUsed(t *testing.T) {
	r := &Response{UsedModel: "meta-llama/llama-3.3-70b-instruct:free"}
	if got := openRouterUsageModel("qwen/qwen3:free", r); got != r.UsedModel {
		t.Errorf("got %q, want the used model", got)
	}
	if got := openRouterUsageModel("openrouter/free", r); got != "openrouter/free" {
		t.Errorf("openrouter/free router: got %q, want itself", got)
	}
	if got := openRouterUsageModel("qwen/qwen3:free", &Response{}); got != "qwen/qwen3:free" {
		t.Errorf("no used model: got %q", got)
	}
}

func TestQuirks_DefaultRateLimit(t *testing.T) {
	q := quirksFor("some-generic-provider")
	zero := &Response{StatusCode: 429, Header: http.Header{}, Body: []byte(`quota exceeded, limit: 0, model: x`)}
	if wait, scope := q.rateLimitCooldown("some-generic-provider", zero); wait != zeroQuotaCooldown || scope != scopeKeySlot {
		t.Errorf("zero quota: got %s scope=%v, want %s for the key slot", wait, scope, zeroQuotaCooldown)
	}
	h := http.Header{}
	h.Set("Retry-After", "42")
	if wait, scope := q.rateLimitCooldown("some-generic-provider", &Response{StatusCode: 429, Header: h}); wait < 42*time.Second || wait > 42*time.Second+4200*time.Millisecond || scope != scopeKeySlot {
		t.Errorf("Retry-After 42: got %s scope=%v, want 42s plus up to 10%% jitter for the key slot", wait, scope)
	}
}

// A provider whose 429 means "this key's daily quota is gone for every
// model" is expressed as a quirk alone: the shared walk then skips the
// provider without trying its other models.
func TestQuirks_RateLimitCanCoolTheWholeProvider(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"daily limit"}`))
	}))
	defer srv.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer ok.Close()

	providerQuirksByID["alpha"] = providerQuirks{
		rateLimit: func(*Response) (time.Duration, rateLimitScope) { return time.Hour, scopeProvider },
	}
	defer delete(providerQuirksByID, "alpha")

	fc := twoProviderChain(srv.URL, ok.URL)
	fc.Stream = false
	fc.Strategy = listStrategy{
		{ProviderID: "alpha", ModelID: "m1"},
		{ProviderID: "alpha", ModelID: "m2"},
		{ProviderID: "beta", ModelID: "m"},
	}
	req := streamReq()
	req.Stream = false
	delete(req.Raw, "stream")
	delete(req.Raw, "stream_options")
	resp, err := fc.Execute(context.Background(), req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("Execute: %v %+v", err, resp)
	}
	if len(calls) != 1 {
		t.Errorf("alpha called %d times, want once: the 429 cools every model", len(calls))
	}
	if !fc.RateLimiter.IsOnCooldown("alpha") {
		t.Error("alpha is not on a provider cooldown")
	}
}

// An account-wide quota on one key cools every model on that key slot and
// leaves the provider's other key slots usable.
func TestCooldown429_KeySlotAllModelsScope(t *testing.T) {
	providerQuirksByID["alpha"] = providerQuirks{
		rateLimit: func(*Response) (time.Duration, rateLimitScope) { return time.Hour, scopeKeySlotAllModels },
	}
	defer delete(providerQuirksByID, "alpha")

	fc := twoProviderChain("http://unused", "http://unused")
	fc.Catalog = &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "alpha", ModelID: "m1"},
		{ProviderID: "alpha", ModelID: "m2"},
		{ProviderID: "beta", ModelID: "m1"},
	}}
	fc.cooldown429("alpha", "m1", 0, 2, &Response{StatusCode: 429, Header: http.Header{}})

	for key, want := range map[string]bool{
		"alpha/m1#1": true, "alpha/m2#1": true, // every model on the spent key
		"alpha/m1#2": false, "alpha/m2#2": false, // the other key's account
		"alpha": false, "beta/m1": false,
	} {
		if got := fc.RateLimiter.IsOnCooldown(key); got != want {
			t.Errorf("IsOnCooldown(%q) = %v, want %v", key, got, want)
		}
	}
}

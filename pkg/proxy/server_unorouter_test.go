package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
)

// unoCatalogEntries are the free candidates a real UnoRouter discovery would
// produce (vendor-prefixed raw IDs, all ":free").
func unoCatalogEntries(providerID string) []catalog.CatalogEntry {
	return []catalog.CatalogEntry{
		{ProviderID: providerID, ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true, TierType: "free"},
		{ProviderID: providerID, ModelID: "qwen/qwen3-30b-a3b:free", IsFree: true, TierType: "free"},
	}
}

// newUnoAliasServer builds a Server whose catalog contains two UnoRouter
// free candidates, both canonicalizing to "qwen3", served by the given
// upstream URL. authKey sets the provider credential sent upstream.
func newUnoAliasServer(t *testing.T, upstreamURL, authKey string, extraEntries ...catalog.CatalogEntry) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Providers = []config.ProviderConfig{
		{ID: "unorouter", BaseURL: upstreamURL, APIKey: authKey, Enabled: true},
	}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Canonicalization.FreeOnly = true
	cat := &catalog.Catalog{Entries: append(unoCatalogEntries("unorouter"), extraEntries...)}
	return NewServer(cfg, cat, nil, nil, nil, nil)
}

// unoAliasRequest drives serveAliasModel with a canonical "qwen3" request,
// mirroring the production routing path (handleChatCompletions →
// serveDirectModel → serveAliasModel).
func unoAliasRequest(s *Server) *httptest.ResponseRecorder {
	req := Request{
		Model:    "qwen3",
		Messages: []map[string]any{{"role": "user", "content": "Reply exactly: UNO_LIVE_OK"}},
	}
	raw := map[string]any{
		"model":    "qwen3",
		"messages": []any{map[string]any{"role": "user", "content": "Reply exactly: UNO_LIVE_OK"}},
		"stream":   false,
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.serveAliasModel(rec, httpReq, s.cfg.Load(), s.catalog.Load(), req, raw, s.aliasRes.Load())
	return rec
}

// unoUpstreamHandler records the Authorization header, requested model, and
// hit count (atomic — the handler runs on its own goroutine). Call n==1
// replies with firstStatus (when > 0); every later call replies with a valid
// completion naming the second candidate.
type unoUpstreamRecord struct {
	hits           atomic.Int32
	authHeader     atomic.Value // string
	requestedModel atomic.Value // string
}

func (u *unoUpstreamRecord) Hits() int32          { return u.hits.Load() }
func (u *unoUpstreamRecord) Auth() string         { s, _ := u.authHeader.Load().(string); return s }
func (u *unoUpstreamRecord) Model() string        { s, _ := u.requestedModel.Load().(string); return s }

func unoUpstreamHandler(firstStatus int) (http.HandlerFunc, *unoUpstreamRecord) {
	rec := &unoUpstreamRecord{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rec.hits.Add(1)
		rec.authHeader.Store(r.Header.Get("Authorization"))
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		rec.requestedModel.Store(body.Model)
		if n == 1 && firstStatus > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(firstStatus)
			fmt.Fprintf(w, `{"error":{"message":"unorouter upstream rejected with %d"}}`, firstStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Echo the requested raw model ID back, like a real OpenAI-compatible upstream.
		fmt.Fprintf(w, `{"id":"uno-ok","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"UNO_LIVE_OK"},"finish_reason":"stop"}]}`, body.Model)
	}), rec
}

// TestUnoAliasRouteForwardsRawModelAndAuth: a client request for the
// canonical "qwen3" must reach UnoRouter as the exact raw free model ID with
// Bearer credentials, and the response body must come back verbatim with
// the canonical name in X-Used-Model and the raw ID in
// X-Free-Router-Upstream-Model.
func TestUnoAliasRouteForwardsRawModelAndAuth(t *testing.T) {
	handler, rec := unoUpstreamHandler(0)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	s := newUnoAliasServer(t, srv.URL, "uno-secret-key")

	recResp := unoAliasRequest(s)
	if recResp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recResp.Code, recResp.Body.String())
	}
	if got := rec.Hits(); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
	if got := rec.Auth(); got != "Bearer uno-secret-key" {
		t.Errorf("upstream Authorization = %q, want Bearer uno-secret-key", got)
	}
	// The client must never see — or send — the UnoRouter model ID, but the
	// upstream must receive the exact raw free ID.
	if got := rec.Model(); got != "qwen/qwen3-235b-a22b:free" {
		t.Errorf("upstream model = %q, want qwen/qwen3-235b-a22b:free", got)
	}
	if !strings.Contains(recResp.Body.String(), "UNO_LIVE_OK") {
		t.Errorf("body %q missing upstream completion content", recResp.Body.String())
	}
	if !strings.Contains(recResp.Body.String(), "qwen/qwen3-235b-a22b:free") {
		t.Errorf("body %q should carry the raw upstream model (verbatim response)", recResp.Body.String())
	}
	if got := recResp.Header().Get("X-Used-Model"); got != "qwen3" {
		t.Errorf("X-Used-Model = %q, want qwen3 (canonical)", got)
	}
	if got := recResp.Header().Get("X-Free-Router-Upstream-Provider"); got != "unorouter" {
		t.Errorf("X-Free-Router-Upstream-Provider = %q, want unorouter", got)
	}
	if got := recResp.Header().Get("X-Free-Router-Upstream-Model"); got != "qwen/qwen3-235b-a22b:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want qwen/qwen3-235b-a22b:free", got)
	}
}

// TestUnoAliasDuplicateCandidatesGrouped: multiple free UnoRouter models
// mapping to the same canonical alias form one candidate set (duplicates
// deduped per provider, same raw ID on another provider kept separate), and
// all candidates flow into the existing alias route.
func TestUnoAliasDuplicateCandidatesGrouped(t *testing.T) {
	s := newUnoAliasServer(t, "http://unused", "", catalog.CatalogEntry{
		ProviderID: "openrouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true,
	})
	res := s.aliasRes.Load()
	if !res.IsCanonical("qwen3") {
		t.Fatal("qwen3 must be canonical from UnoRouter catalog entries")
	}
	ups := res.Upstreams("qwen3")
	if len(ups) != 3 {
		t.Fatalf("Upstreams(qwen3) = %+v, want 3 distinct candidates (2 unorouter + 1 openrouter)", ups)
	}
	unoCount := 0
	for _, u := range ups {
		if u.ProviderID != "unorouter" {
			continue
		}
		unoCount++
		if u.ModelID != "qwen/qwen3-235b-a22b:free" && u.ModelID != "qwen/qwen3-30b-a3b:free" {
			t.Errorf("unexpected unorouter candidate %q", u.ModelID)
		}
	}
	if unoCount != 2 {
		t.Errorf("unorouter candidates = %d, want 2 (one per free model)", unoCount)
	}
}

// TestUnoAliasClientErrorSurfacesImmediately: a UnoRouter 400 ClassifyFailure
// cannot map to a candidate-shaped fix is a client/request error — surface
// verbatim, exactly one attempt, never the exhaustion response. 401/403 are
// NOT in this table anymore — task H moved them to the cooldown+fallback
// contract shared by every route (TestUnoAliasAuthErrorsCooldownAndFallback).
func TestUnoAliasClientErrorSurfacesImmediately(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			handler, urec := unoUpstreamHandler(status)
			srv := httptest.NewServer(handler)
			defer srv.Close()
			s := newUnoAliasServer(t, srv.URL, "uno-secret-key")

			rec := unoAliasRequest(s)
			if rec.Code != status {
				t.Errorf("status = %d, want %d (upstream error must surface as-is)", rec.Code, status)
			}
			if got := urec.Hits(); got != 1 {
				t.Errorf("upstream hits = %d, want 1 (non-retryable must not walk further candidates)", got)
			}
			if strings.Contains(rec.Body.String(), "temporarily unavailable") {
				t.Errorf("canned exhaustion response leaked: %q", rec.Body.String())
			}
		})
	}
}

// TestUnoAliasAuthErrorsCooldownAndFallback: a UnoRouter 401/403 follows the
// same contract as every route (task H) — never surfaced, the provider goes
// on cooldown, and the walk continues to the next candidate.
func TestUnoAliasAuthErrorsCooldownAndFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			handler, urec := unoUpstreamHandler(status)
			srv := httptest.NewServer(handler)
			defer srv.Close()
			s := newUnoAliasServer(t, srv.URL, "uno-secret-key")
			s.rateLimiter = ratelimit.NewGlobalTracker() // the harness passes nil — the cooldown assertions need the real tracker

			rec := unoAliasRequest(s)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (401/403 must walk to the next candidate, task H) — body: %s", rec.Code, rec.Body.String())
			}
			if got := urec.Hits(); got != 2 {
				t.Errorf("upstream hits = %d, want 2 (auth failure walks, next candidate serves)", got)
			}
			if !strings.Contains(rec.Body.String(), "UNO_LIVE_OK") {
				t.Errorf("body %q missing second-candidate content", rec.Body.String())
			}
			if !s.rateLimiter.IsOnCooldown("unorouter") {
				t.Error("401/403 must put the provider on cooldown")
			}
		})
	}
}

// TestUnoAliasRetryableFailoverAcrossCandidates: a retryable 503 from the
// first free candidate walks to the second candidate of the same canonical
// alias, which serves the request (candidate A → failure → candidate B).
func TestUnoAliasRetryableFailoverAcrossCandidates(t *testing.T) {
	handler, urec := unoUpstreamHandler(http.StatusServiceUnavailable)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	s := newUnoAliasServer(t, srv.URL, "uno-secret-key")

	rec := unoAliasRequest(s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover (body: %s)", rec.Code, rec.Body.String())
	}
	if got := urec.Hits(); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (first candidate failed, second served)", got)
	}
	if !strings.Contains(rec.Body.String(), "UNO_LIVE_OK") {
		t.Errorf("body %q missing second-candidate content", rec.Body.String())
	}
	if got := rec.Header().Get("X-Free-Router-Upstream-Model"); got != "qwen/qwen3-30b-a3b:free" {
		t.Errorf("X-Free-Router-Upstream-Model = %q, want the second candidate's raw ID", got)
	}
}

// TestUnoModelsExposureCanonicalOnly: with expose_raw=false /
// expose_canonical=true the listing shows canonical names only — raw
// UnoRouter IDs stay internal but remain routable.
func TestUnoModelsExposureCanonicalOnly(t *testing.T) {
	s := newUnoAliasServer(t, "http://unused", "", catalog.CatalogEntry{
		ProviderID: "unorouter", ModelID: "deepseek/deepseek-r1:free", IsFree: true,
	})
	cfg := *s.cfg.Load()
	cfg.Models.ExposeRaw = false
	cfg.Models.ExposeCanonical = true
	s.UpdateConfig(&cfg)

	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range body.Data {
		ids[m.ID] = true
	}
	for _, canonical := range []string{"qwen3", "deepseek-r1"} {
		if !ids[canonical] {
			t.Errorf("canonical %q missing from /v1/models", canonical)
		}
	}
	for _, raw := range []string{"qwen/qwen3-235b-a22b:free", "qwen/qwen3-30b-a3b:free", "deepseek/deepseek-r1:free"} {
		if ids[raw] {
			t.Errorf("raw UnoRouter ID %q must not be listed in canonical-only mode", raw)
		}
	}
}

// TestUnoAPIKeyEnvRefExpansion: api_key: "${UNOROUTER_API_KEY}" resolves
// from the environment and flows into the upstream Authorization header.
func TestUnoAPIKeyEnvRefExpansion(t *testing.T) {
	t.Setenv("UNOROUTER_API_KEY", "expanded-uno-value")
	handler, urec := unoUpstreamHandler(0)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	s := newUnoAliasServer(t, srv.URL, "${UNOROUTER_API_KEY}")

	rec := unoAliasRequest(s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := urec.Auth(); got != "Bearer expanded-uno-value" {
		t.Errorf("upstream Authorization = %q, want Bearer expanded-uno-value", got)
	}
}
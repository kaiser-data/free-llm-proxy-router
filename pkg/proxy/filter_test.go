package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// passStrategy ranks exactly the candidates it is given, in order, so a test
// sees what the filters let through.
type passStrategy struct{}

func (passStrategy) Name() string { return "pass" }
func (passStrategy) Rank(_ strategy.Request, es []catalog.CatalogEntry, _ *models.EnrichedCatalog) []strategy.RankedEntry {
	out := make([]strategy.RankedEntry, len(es))
	for i, e := range es {
		out[i] = strategy.RankedEntry{ProviderID: e.ProviderID, ModelID: e.ModelID}
	}
	return out
}

// recorder is an upstream that answers OK and records each request body.
type recorder struct {
	mu     sync.Mutex
	bodies []map[string]any
	*httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		w.Write([]byte(`{"model":"` + body["model"].(string) + `","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *recorder) models() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, b := range r.bodies {
		out = append(out, b["model"].(string))
	}
	return out
}

func filterChain(t *testing.T, entries []catalog.CatalogEntry, providers []config.ProviderConfig, filters ...config.FilterConfig) *FallbackChain {
	t.Helper()
	fs, err := strategy.BuildFilters(filters)
	if err != nil {
		t.Fatal(err)
	}
	return &FallbackChain{
		Cfg:                &config.Config{Providers: providers, Fallback: config.FallbackConfig{MaxAttempts: 5}},
		Strategy:           passStrategy{},
		Catalog:            &catalog.Catalog{Entries: entries},
		RateLimiter:        ratelimit.NewGlobalTracker(),
		ReliabilityTracker: reliability.New(),
		HTTPClient:         &http.Client{},
		Filters:            fs,
	}
}

func toolReq() Request {
	msgs := []map[string]any{{"role": "user", "content": "weather?"}}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "weather"}}}
	return Request{Messages: msgs, Raw: map[string]any{
		"messages": []any{msgs[0]},
		"tools":    tools,
		"provider": map[string]any{"sort": "price"},
	}}
}

// A model OpenRouter lists without "tools" is never sent a tool request; a
// model without metadata still is (unknown: keep).
func TestFilters_ToolRequestSkipsModelsWithoutTools(t *testing.T) {
	or, groq := newRecorder(t), newRecorder(t)
	fc := filterChain(t,
		[]catalog.CatalogEntry{
			{ProviderID: "openrouter", ModelID: "no-tools", IsFree: true, Metadata: map[string]any{"supported_parameters": []any{"temperature"}}},
			{ProviderID: "groq", ModelID: "fast", IsFree: true},
		},
		[]config.ProviderConfig{{ID: "openrouter", BaseURL: or.URL, Enabled: true}, {ID: "groq", BaseURL: groq.URL, Enabled: true}},
		config.FilterConfig{Name: "tool-calling"},
	)
	resp, err := fc.Execute(context.Background(), toolReq())
	if err != nil || resp.UsedModel != "fast" {
		t.Fatalf("want groq/fast to answer, got resp=%v err=%v", resp, err)
	}
	if got := or.models(); len(got) != 0 {
		t.Errorf("openrouter was sent %v although its only model cannot call tools", got)
	}
}

// When the filters leave nothing, openrouter/free is still judged by them:
// with unknown: keep it runs, restricted by require_parameters; with
// unknown: drop it is skipped and the client gets ErrNoCandidates.
func TestFilters_OpenRouterFreeFollowsUnknownPolicy(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "openrouter", ModelID: "no-tools", IsFree: true, ContextWindow: 8000, Metadata: map[string]any{"supported_parameters": []any{"temperature"}}},
	}

	t.Run("keep", func(t *testing.T) {
		or := newRecorder(t)
		fc := filterChain(t, entries, []config.ProviderConfig{{ID: "openrouter", BaseURL: or.URL, Enabled: true}},
			config.FilterConfig{Name: "tool-calling"})
		req := toolReq()
		resp, err := fc.Execute(context.Background(), req)
		if err != nil || resp.UsedModel != "openrouter/free" {
			t.Fatalf("want openrouter/free to answer, got resp=%v err=%v", resp, err)
		}
		if got := strings.Join(or.models(), ","); got != "openrouter/free" {
			t.Fatalf("openrouter calls = %s, want only openrouter/free", got)
		}
		prov, _ := or.bodies[0]["provider"].(map[string]any)
		if prov["require_parameters"] != true || prov["sort"] != "price" {
			t.Errorf("provider = %v, want require_parameters added to the client's sort", prov)
		}
		if _, leaked := req.Raw["provider"].(map[string]any)["require_parameters"]; leaked {
			t.Error("require_parameters was written into the client's own request map")
		}
	})

	t.Run("drop", func(t *testing.T) {
		or := newRecorder(t)
		fc := filterChain(t, entries, []config.ProviderConfig{{ID: "openrouter", BaseURL: or.URL, Enabled: true}},
			config.FilterConfig{Name: "tool-calling", Settings: map[string]any{"unknown": "drop"}})
		_, err := fc.Execute(context.Background(), toolReq())
		if !errors.Is(err, strategy.ErrNoCandidates) {
			t.Fatalf("err = %v, want ErrNoCandidates", err)
		}
		if got := or.models(); len(got) != 0 {
			t.Errorf("openrouter was called (%v) although unknown: drop", got)
		}
	})
}

// No model fits: the client gets a 400 naming the filters, not a 503 that
// SDKs would retry forever, and no upstream is called.
func TestServer_NoModelFitsIs400(t *testing.T) {
	up := newRecorder(t)
	s := singleProviderServer(up.URL, func(c *config.Config) {
		c.Proxy.Filters = []config.FilterConfig{{Name: "context-fit", Settings: map[string]any{"unknown": "drop"}}}
	})
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	if msg, code := decodeOpenAIError(t, rec); code != "no_model_fits_request" || !strings.Contains(msg, "context-fit") {
		t.Errorf("error = %q (%s), want no_model_fits_request naming context-fit", msg, code)
	}
	if got := up.models(); len(got) != 0 {
		t.Errorf("upstream called: %v", got)
	}
}

// A typo in a reloaded config is rejected and the running config stays.
func TestServer_UpdateConfigRejectsBadFilters(t *testing.T) {
	s := singleProviderServer("http://127.0.0.1:1", nil)
	before := s.config()
	bad := *before
	bad.Proxy.Filters = []config.FilterConfig{{Name: "context-fits"}}
	if err := s.UpdateConfig(&bad); err == nil {
		t.Fatal("UpdateConfig accepted an unknown filter")
	}
	if s.config() != before || len(s.conf.Load().filters) != 0 {
		t.Error("a rejected reload replaced the running config or filters")
	}
}

// Config and filters are swapped together: a request loading the state while
// reloads run never pairs one config's filters with another config.
func TestServer_ConfigAndFiltersReloadTogether(t *testing.T) {
	s := singleProviderServer("http://127.0.0.1:1", nil)
	with := *s.config()
	with.Proxy.Filters = []config.FilterConfig{{Name: "context-fit"}, {Name: "tool-calling"}}
	without := *s.config()
	without.Proxy.Filters = nil

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			c := &with
			if i%2 == 1 {
				c = &without
			}
			if err := s.UpdateConfig(c); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		st := s.conf.Load()
		if len(st.filters) != len(st.cfg.Proxy.Filters) {
			t.Fatalf("filters %v built from another config (%d configured)", st.filters.Names(), len(st.cfg.Proxy.Filters))
		}
	}
}

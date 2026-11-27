package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// fixedRT answers every upstream call in memory, so the benchmark measures
// the proxy's own work, not the loopback network.
type fixedRT struct{ body string }

func (f fixedRT) RoundTrip(r *http.Request) (*http.Response, error) {
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Request:    r,
	}, nil
}

// benchServer builds a server over a catalog the size of the shipped one
// (~3000 entries over 10 providers), every provider answering at once.
func benchServer(b *testing.B, entries int) *Server {
	b.Helper()
	providers := []string{"groq", "cerebras", "mistral", "nvidia-nim", "github-models", "huggingface", "kilo", "requesty", "api-airforce", "unorouter"}
	cfg := &config.Config{Fallback: config.FallbackConfig{MaxAttempts: 5}}
	for _, p := range providers {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: p, BaseURL: "http://upstream.invalid/v1", Enabled: true})
	}
	cfg.Proxy.Strategy = "adaptive"
	cat := &catalog.Catalog{}
	for i := 0; i < entries; i++ {
		cat.Entries = append(cat.Entries, catalog.CatalogEntry{
			ProviderID:    providers[i%len(providers)],
			ModelID:       fmt.Sprintf("vendor/model-%d-instruct", i),
			IsFree:        i%5 != 0,
			TierType:      "free",
			ContextWindow: 32768,
		})
	}
	rl, rel := ratelimit.NewGlobalTracker(), reliability.New()
	reg := strategy.NewRegistry(rel, rl, "", "", 3, 5, strategy.LongContextOptions{})
	s, err := NewServer(cfg, cat, reg, rl, nil, rel)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkChatCompletions_Buffered(b *testing.B) {
	orig := upstreamClient
	upstreamClient = &http.Client{Transport: fixedRT{body: `{"id":"x","model":"vendor/model-1-instruct","choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat("token ", 200) + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":200}}`}}
	defer func() { upstreamClient = orig }()

	s := benchServer(b, 3000)
	reqBody := `{"model":"auto","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"` + strings.Repeat("lorem ipsum ", 300) + `"}]}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody)))
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
}

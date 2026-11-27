package proxy

// End-to-end failover benchmark: how long a client waits for its first token
// when the first provider fails, compared to when it answers.
//
//	FAILOVER_BENCH=1 go test ./pkg/proxy -run TestFailoverBenchmark -v
//	make bench-failover
//
// Knobs (env): FAILOVER_BENCH_N requests per row (default 400),
// FAILOVER_BENCH_CONC concurrent clients (16), FAILOVER_BENCH_TIMEOUT the
// first provider's timeout (2s), FAILOVER_BENCH_UPSTREAM_TTFT the healthy
// upstream's delay before its first token (25ms), FAILOVER_BENCH_OUT a file
// to write the markdown table to (relative paths resolve in pkg/proxy).
//
// Two upstreams on loopback: "alpha" is tried first and fails as the
// scenario says, "beta" always answers. Clients talk real HTTP to the
// proxy's chat handler, so the numbers include request parsing, strategy
// ranking, the fallback walk, SSE peeking and the client's own read.
//
// Modes:
//   - cold: a fresh proxy (empty cooldowns, empty reliability history) per
//     request, so every request pays the full fallback. The worst case.
//   - warm: one proxy for the whole row, as in a running deployment. After
//     the first failures alpha is cooled down and later requests skip it.

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

type failoverScenario struct {
	name  string
	alpha func(ttft time.Duration) http.HandlerFunc
}

// healthyUpstream streams a two-chunk answer, the first after ttft.
func healthyUpstream(ttft time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(ttft):
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, sseRole+sseHello)
		w.(http.Flusher).Flush()
		io.WriteString(w, sseWorld+sseDone)
	}
}

var failoverScenarios = []failoverScenario{
	{"healthy first provider", healthyUpstream},
	{"first provider 429", func(time.Duration) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"Rate limit reached","type":"rate_limit_exceeded"}}`)
		}
	}},
	{"first provider stalls before first token", func(time.Duration) http.HandlerFunc {
		// Headers and the role chunk arrive, then nothing: the proxy cannot
		// commit and must wait for the provider timeout before falling back.
		return func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sseRole)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}},
}

type failoverSample struct {
	ok   bool
	ttft time.Duration
}

type failoverRow struct {
	mode, scenario string
	n, ok          int
	alphaHits      int64
	p50, p95, p99  time.Duration
	max            time.Duration
}

func benchEnvInt(t *testing.T, key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", key, v)
	}
	return n
}

func benchEnvDur(t *testing.T, key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q: want a positive duration", key, v)
	}
	return d
}

// failoverProxy is a proxy over alpha (ranked first) and beta.
func failoverProxy(t *testing.T, alphaURL, betaURL string, timeout time.Duration) *Server {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "alpha", BaseURL: alphaURL, Enabled: true, Timeout: timeout.String()},
			{ID: "beta", BaseURL: betaURL, Enabled: true, Timeout: timeout.String()},
		},
		Fallback: config.FallbackConfig{MaxAttempts: 5, RequestTimeout: 30 * time.Second},
	}
	cfg.Proxy.Strategy = "adaptive"
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "alpha", ModelID: "m", IsFree: true, TierType: "free", ContextWindow: 32768},
		{ProviderID: "beta", ModelID: "m", IsFree: true, TierType: "free", ContextWindow: 32768},
	}}
	rl, rel := ratelimit.NewGlobalTracker(), reliability.New()
	s, err := NewServer(cfg, cat, strategy.NewRegistry(rel, rl, "", "", 3, 5, strategy.LongContextOptions{}), rl, nil, rel)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// streamTTFT sends one streaming request and times the first content chunk.
func streamTTFT(client *http.Client, url string) failoverSample {
	start := time.Now()
	resp, err := client.Post(url, "application/json", strings.NewReader(streamBody))
	if err != nil {
		return failoverSample{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return failoverSample{}
	}
	br := bufio.NewReader(resp.Body)
	var ttft time.Duration
	var body strings.Builder
	for {
		line, err := br.ReadString('\n')
		if ttft == 0 && strings.HasPrefix(line, "data: ") && strings.Contains(line, `"content":"`) {
			ttft = time.Since(start)
		}
		body.WriteString(line)
		if err != nil {
			break
		}
	}
	b := body.String()
	ok := ttft > 0 && strings.Contains(b, `"hello"`) && strings.Contains(b, `" world"`) && strings.Contains(b, "[DONE]")
	return failoverSample{ok: ok, ttft: ttft}
}

func runFailoverRow(t *testing.T, mode, scenario string, url string, n, conc int, alphaHits *atomic.Int64) failoverRow {
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: conc, MaxConnsPerHost: conc},
	}
	defer client.CloseIdleConnections()

	// Open the client's connections first, so the first wave of requests
	// does not also time TCP setup. A GET reaches no provider: the chat
	// handler rejects it, the direct upstream answers it like a POST.
	var warm sync.WaitGroup
	for range conc {
		warm.Add(1)
		go func() {
			defer warm.Done()
			if resp, err := client.Get(url); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	warm.Wait()
	if alphaHits != nil {
		alphaHits.Store(0)
	}

	samples := make([]failoverSample, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				samples[i] = streamTTFT(client, url)
			}
		}()
	}
	wg.Wait()

	row := failoverRow{mode: mode, scenario: scenario, n: n}
	if alphaHits != nil {
		row.alphaHits = alphaHits.Load()
	}
	var ttfts []time.Duration
	for _, s := range samples {
		if s.ok {
			row.ok++
			ttfts = append(ttfts, s.ttft)
		}
	}
	if len(ttfts) > 0 {
		sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })
		pct := func(p float64) time.Duration { return ttfts[int(p*float64(len(ttfts)-1)+0.5)] }
		row.p50, row.p95, row.p99, row.max = pct(0.50), pct(0.95), pct(0.99), ttfts[len(ttfts)-1]
	}
	return row
}

func TestFailoverBenchmark(t *testing.T) {
	if os.Getenv("FAILOVER_BENCH") == "" {
		t.Skip("set FAILOVER_BENCH=1 to run the end-to-end failover benchmark")
	}
	n := benchEnvInt(t, "FAILOVER_BENCH_N", 400)
	conc := benchEnvInt(t, "FAILOVER_BENCH_CONC", 16)
	timeout := benchEnvDur(t, "FAILOVER_BENCH_TIMEOUT", 2*time.Second)
	upTTFT := benchEnvDur(t, "FAILOVER_BENCH_UPSTREAM_TTFT", 25*time.Millisecond)

	// Every fallback logs a line; at hundreds of requests that is noise and
	// measurable work on the hot path a quiet deployment (log_level: warn)
	// would not do.
	prevLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prevLog)

	beta := httptest.NewServer(healthyUpstream(upTTFT))
	defer beta.Close()

	var rows []failoverRow
	rows = append(rows, runFailoverRow(t, "direct", "healthy upstream, no proxy", beta.URL, n, conc, nil))

	for _, mode := range []string{"cold", "warm"} {
		for _, sc := range failoverScenarios {
			var hits atomic.Int64
			alphaH := sc.alpha(upTTFT)
			alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				alphaH(w, r)
			}))

			var h http.HandlerFunc
			if mode == "warm" {
				h = failoverProxy(t, alpha.URL, beta.URL, timeout).handleChatCompletions
			} else {
				h = func(w http.ResponseWriter, r *http.Request) {
					failoverProxy(t, alpha.URL, beta.URL, timeout).handleChatCompletions(w, r)
				}
			}
			proxy := httptest.NewServer(h)

			row := runFailoverRow(t, mode, sc.name, proxy.URL+"/v1/chat/completions", n, conc, &hits)
			proxy.Close()
			alpha.Close()

			// The benchmark is only meaningful if alpha really is tried first.
			if mode == "cold" && row.alphaHits != int64(n) {
				t.Fatalf("%s/%s: alpha got %d of %d requests; the strategy no longer ranks it first", mode, sc.name, row.alphaHits, n)
			}
			rows = append(rows, row)
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Failover benchmark: %d streaming requests per row, %d concurrent clients, first-provider timeout %s, healthy upstream TTFT %s, %s.\n\n",
		n, conc, timeout, upTTFT, goEnv())
	out.WriteString("| mode | scenario | completed | hit failing provider | p50 TTFT | p95 TTFT | p99 TTFT | max TTFT |\n")
	out.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		hits := "—"
		if r.mode != "direct" {
			hits = strconv.FormatInt(r.alphaHits, 10)
		}
		fmt.Fprintf(&out, "| %s | %s | %d/%d (%.1f%%) | %s | %s | %s | %s | %s |\n",
			r.mode, r.scenario, r.ok, r.n, 100*float64(r.ok)/float64(r.n), hits,
			fmtMs(r.p50), fmtMs(r.p95), fmtMs(r.p99), fmtMs(r.max))
	}
	fmt.Print("\n" + out.String())
	if path := os.Getenv("FAILOVER_BENCH_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		if r.ok != r.n {
			t.Errorf("%s/%s: %d of %d requests did not complete", r.mode, r.scenario, r.n-r.ok, r.n)
		}
	}
}

func fmtMs(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

func goEnv() string {
	return fmt.Sprintf("%s/%s, %d CPUs, %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
}

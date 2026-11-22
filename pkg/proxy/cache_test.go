package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Only requests that ask for a repeatable answer may be served from the
// cache: an agent repeating a tool request, or a client sampling with
// temperature > 0 or n > 1, expects a fresh answer.
func TestChatCompletions_CachePolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantCache bool
	}{
		{"plain", `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, true},
		{"temperature 0", `{"model":"auto","temperature":0,"messages":[{"role":"user","content":"hi"}]}`, true},
		{"temperature 0.7", `{"model":"auto","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`, false},
		{"n 2", `{"model":"auto","n":2,"messages":[{"role":"user","content":"hi"}]}`, false},
		{"tools", `{"model":"auto","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`, false},
		{"legacy functions", `{"model":"auto","functions":[{"name":"f"}],"messages":[{"role":"user","content":"hi"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				io.WriteString(w, `{"model":"gemma","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`)
			}))
			defer upstream.Close()

			s := newTestServer(upstream.URL, 300)
			hit := false
			for i := 0; i < 2; i++ {
				rec := httptest.NewRecorder()
				s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body)))
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
				hit = hit || rec.Header().Get("X-Cache") == "HIT"
			}
			if hit != tc.wantCache {
				t.Errorf("served from cache = %v, want %v (upstream calls = %d)", hit, tc.wantCache, calls)
			}
		})
	}
}

// With the cache off, Get must not serialise and hash the request.
func BenchmarkCacheGet_Disabled(b *testing.B) {
	c := NewResponseCache(0)
	raw := map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 4096)}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Get(raw)
	}
}

// Agent-profile defaults from YAML arrive as int, not float64.
func TestCacheable_ProfileInts(t *testing.T) {
	if cacheable(map[string]any{"temperature": 1}) {
		t.Error("temperature 1 (YAML int) must not be cached")
	}
	if cacheable(map[string]any{"n": 3}) {
		t.Error("n 3 (YAML int) must not be cached")
	}
	if !cacheable(map[string]any{"temperature": 0, "n": 1}) {
		t.Error("temperature 0, n 1 should be cacheable")
	}
}

func cacheReq(i int) map[string]any {
	return map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": fmt.Sprint("q", i)}}}
}

// The cache never holds more than its byte budget, replacing a key does not
// count it twice, and a single huge answer is not cached at all.
func TestResponseCache_StaysWithinByteBudget(t *testing.T) {
	c := NewResponseCache(300)
	c.maxBytes = 10 * (1000 + cacheEntryOverhead)
	body := []byte(strings.Repeat("x", 1000))
	for i := 0; i < 50; i++ {
		c.Set(cacheReq(i), body)
		if c.bytes > c.maxBytes {
			t.Fatalf("after %d sets: %d bytes, budget %d", i+1, c.bytes, c.maxBytes)
		}
	}
	if len(c.entries) != 10 {
		t.Errorf("entries = %d, want 10 (budget holds 10)", len(c.entries))
	}
	if c.Get(cacheReq(49)) == nil {
		t.Error("the newest answer must be cached")
	}

	before := c.bytes
	c.Set(cacheReq(49), body) // same key again
	if c.bytes != before {
		t.Errorf("replacing a key changed bytes %d → %d", before, c.bytes)
	}

	c.Set(cacheReq(1000), make([]byte, cacheMaxEntryBytes+1))
	if c.Get(cacheReq(1000)) != nil {
		t.Error("an answer over cacheMaxEntryBytes must not be cached")
	}

	sum := 0
	for _, e := range c.entries {
		sum += entrySize(e)
	}
	if sum != c.bytes {
		t.Errorf("tracked bytes %d, actual %d", c.bytes, sum)
	}
}

// An upstream body over maxResponseBytes is not buffered: the call fails and
// the client gets the retryable 503, not a truncated or giant answer.
func TestChatCompletions_OversizedUpstreamBodyIsRejected(t *testing.T) {
	old := maxResponseBytes
	maxResponseBytes = 1 << 10
	defer func() { maxResponseBytes = old }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"gemma","choices":[{"message":{"content":"`+strings.Repeat("x", 4<<10)+`"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	s := newTestServer(upstream.URL, 0)
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %.200s", rec.Code, rec.Body)
	}
}

package proxy

import (
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

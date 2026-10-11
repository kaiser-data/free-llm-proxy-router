package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// unoJSONCompletion is the upstream's real (buffered) completion body.
func unoJSONCompletion(model string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"real answer"},"finish_reason":"stop"}]}`, model)
}

// hitRecorder is the upstream stand-in: serves real completions and
// records the routed model ids in arrival order.
type hitRecorder struct {
	mu   sync.Mutex
	hits []string
}

func (h *hitRecorder) record(model string) {
	h.mu.Lock()
	h.hits = append(h.hits, model)
	h.mu.Unlock()
}

func (h *hitRecorder) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.hits...)
}

func (h *hitRecorder) handler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		model, _ := req["model"].(string)
		h.record(model)
		if stream, _ := req["stream"].(bool); stream {
			// Real SSE — the hold must see a usable event and commit.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n", model)
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"real answer\"}}]}\n\n", model)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, unoJSONCompletion(model))
	})
}

// queuedServer builds a Server with one rpm=1 provider and slot queueing
// enabled; the limiter window is shortened for the test.
func queuedServer(t *testing.T, waitSeconds int) (*Server, *hitRecorder) {
	t.Helper()
	rec := &hitRecorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Fallback.QueueWaitSeconds = waitSeconds
	cfg.Fallback.QueueMaxWaiters = 10
	cfg.Fallback.StreamKeepaliveAfterSeconds = 0 // keepalive is task P — off here
	cfg.Providers = []config.ProviderConfig{{
		ID: "tp", BaseURL: srv.URL, Enabled: true, RPM: 1,
	}}
	cfg.Models.Canonicalization.Enabled = true
	cfg.Models.Aliases = map[string][]string{"qwen3": {"model-a"}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "tp", ModelID: "model-a", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())
	s.slotLimit.window = 150 * time.Millisecond
	return s, rec
}

// TestSlotQueueIntegrationBufferedFIFO: three concurrent requests against
// an rpm=1 upstream — the first walks in immediately; the others queue
// FIFO and each gains a real completion only after a booking leaves the
// window (shortened here from one minute). Every request ends with a REAL
// 200 answer — the queue replaces the instant-503 path (task L0).
func TestSlotQueueIntegrationBufferedFIFO(t *testing.T) {
	s, rec := queuedServer(t, 5)

	t0 := time.Now()
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			reqBody := fmt.Sprintf(`{"model":"qwen3","messages":[{"role":"user","content":"hi %d"}]}`, n)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
			r := httptest.NewRecorder()
			s.handleChatCompletions(r, req)
			codes[n] = r.Code
		}(i)
	}
	wg.Wait()

	for n, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d must get a real 200 completion, got %d", n, c)
		}
	}
	hits := rec.list()
	if len(hits) != 3 {
		t.Fatalf("expected exactly 3 upstream hits, got %d: %v", len(hits), hits)
	}
	// Quota pressure really happened: the queued requests could only enter
	// after previous bookings left the window — the walk spans >= 2 windows.
	if elapsed := time.Since(t0); elapsed < 2*s.slotLimit.window {
		t.Fatalf("expected the queued requests to wait >= 2 windows, elapsed %v", elapsed)
	}
}

// TestSlotQueueStreamRouteRealSSEAfterWait: the streaming route under the
// same quota pressure — queued requests wait, then each client sees real
// SSE with a 200 and a real completion (no fake 200 anywhere).
func TestSlotQueueStreamRouteRealSSEAfterWait(t *testing.T) {
	s, rec := queuedServer(t, 5)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"qwen3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
			r := httptest.NewRecorder()
			s.handleChatCompletions(r, req)
			if r.Code != http.StatusOK {
				t.Errorf("stream request must get 200, got %d", r.Code)
			}
			if !strings.Contains(r.Body.String(), "data: [DONE]") {
				t.Errorf("stream body must end with the [DONE] sentinel, got %q", r.Body.String())
			}
		}()
	}
	wg.Wait()
	hits := rec.list()
	if len(hits) != 2 {
		t.Errorf("expected exactly 2 upstream hits, got %d: %v", len(hits), hits)
	}
}

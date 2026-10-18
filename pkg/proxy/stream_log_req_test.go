package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// captureStdLog redirects the standard logger into a buffer for the duration
// of the test and restores the previous writer/flags afterwards.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestStreamPathLogsCarryRequestID (follow-up 1b): the stream-path log lines
// carry req=<id> so a stuck or misfailing stream can be traced end to end —
// here through a full direct-route walk (streamOneCandidate's pre-commit
// status line) and the strategy-chain exhaustion line.
func TestStreamPathLogsCarryRequestID(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{{ID: "tp", BaseURL: fail.URL, Enabled: true}}
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "tp", ModelID: "model-a", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	buf := captureStdLog(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("X-Request-Id", "req-logtest-1")
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, req)

	logs := buf.String()
	for _, want := range []string{
		"req=req-logtest-1",
		"status 500 (stream) — trying next candidate",
		"fallback chain exhausted (stream): req=req-logtest-1",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("stream-path log missing %q\ncaptured:\n%s", want, logs)
		}
	}
}

// TestStreamPathUnrecognized400WalksToNextCandidate (round 14): the stream
// path now runs the SAME failureDecisionWalk the buffered routes run — an
// adapter-template 400 (live: gemma-4-31b "Request contains an invalid
// argument", 2026-10-14 19:31) parks the model on first sight and the walk
// continues, so the client is served by the next candidate instead of
// receiving the upstream 400 (the exact shape that surfaced during the
// round-14 live incident on the alias route).
func TestStreamPathUnrecognized400WalksToNextCandidate(t *testing.T) {
	gemmaBody := `{"error":{"code":400,"message":"Request contains an invalid argument. (request id: 202610141931116721004928268d9d6nEfaGUos)","status":"INVALID_ARGUMENT"}}`
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(gemmaBody))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"id\":\"1\",\"model\":\"model-a\",\"choices\":[{\"delta\":{\"content\":\"hi from model-b\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer good.Close()

	cfg := &config.Config{}
	cfg.Proxy.Strategy = "adaptive"
	cfg.Fallback.Unknown400Policy = policyCrossModelConfirm
	cfg.Fallback.Unknown400ConfirmN = 2
	cfg.Fallback.Unknown400DemotionSeconds = 600
	cfg.Providers = []config.ProviderConfig{
		{ID: "pa", BaseURL: bad.URL, Enabled: true},
		{ID: "pb", BaseURL: good.URL, Enabled: true},
	}
	// The direct route walks across PROVIDERS offering the SAME model
	// (matchEntry matches the requested model only) — model-a from pa
	// 400s, model-a from pb must serve.
	cat := &catalog.Catalog{Entries: []catalog.CatalogEntry{
		{ProviderID: "pa", ModelID: "model-a", IsFree: true},
		{ProviderID: "pb", ModelID: "model-a", IsFree: true},
	}}
	s := NewServer(cfg, cat, strategy.NewRegistry(nil, nil, "", "", 0, 0),
		ratelimit.NewGlobalTracker(), nil, reliability.New())

	buf := captureStdLog(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("X-Request-Id", "req-walk400-1")
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200 from model-b (walked past model-a)\nbody: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "from model-b") {
		t.Fatalf("client body missing the served SSE stream:\n%s", rec.Body.String())
	}
	if !s.demotions.isDemoted("pa", "model-a", time.Now(), 600*time.Second) {
		t.Fatal("model-a must be parked after the stream-path 400")
	}
	if logs := buf.String(); !strings.Contains(logs, "status 400 (stream) — trying next candidate") {
		t.Fatalf("stream walk log missing the next-candidate line:\n%s", logs)
	}
}

// TestPumpCommittedStreamLogsCarryRequestID: the post-commit pump's
// interruption log carries req=<id> — the one place a committed stream
// still logs (follow-up 1b).
func TestPumpCommittedStreamLogsCarryRequestID(t *testing.T) {
	buf := captureStdLog(t)
	w := httptest.NewRecorder()
	p := streamClientParams{RequestID: "req-pump-1", IdleTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pumpCommittedStream(ctx, w, errReader{}, p, cancel, false)

	logs := buf.String()
	if !strings.Contains(logs, "stream: req=req-pump-1 upstream interrupted after commit") {
		t.Fatalf("pump log missing req=id, captured:\n%s", logs)
	}
}

// eofReader simulates an upstream body that is already exhausted — the
// fast-upstream case where the whole stream landed in the commit buffer
// before the pump was invoked (round 15 regression companion).
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// Round 15 regression: when the ENTIRE stream — content, finish_reason and
// data: [DONE] — already landed in the committed buffer, the pump's first
// read of the exhausted upstream hits EOF with sawDone seeded from the
// buffer. The pump must then end silently: no spurious
// "upstream interrupted" error event and no second [DONE] after the real
// one.
func TestPumpSeesSentinelSeededFromCommittedBuffer(t *testing.T) {
	buf := captureStdLog(t)
	w := httptest.NewRecorder()
	p := streamClientParams{RequestID: "req-pump-2", IdleTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pumpCommittedStream(ctx, w, eofReader{}, p, cancel, true)

	if logs := buf.String(); strings.Contains(logs, "upstream interrupted after commit") {
		t.Fatalf("spurious interruption log with seeded sawDone:\n%s", logs)
	}
	if body := w.Body.String(); strings.Contains(body, "[DONE]") {
		t.Fatalf("second [DONE] written after the buffered one:\n%s", body)
	}
}

// errReader fails on the first read — a committed upstream that dies
// mid-stream.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

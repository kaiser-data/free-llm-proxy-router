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

// TestPumpCommittedStreamLogsCarryRequestID: the post-commit pump's
// interruption log carries req=<id> — the one place a committed stream
// still logs (follow-up 1b).
func TestPumpCommittedStreamLogsCarryRequestID(t *testing.T) {
	buf := captureStdLog(t)
	w := httptest.NewRecorder()
	p := streamClientParams{RequestID: "req-pump-1", IdleTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pumpCommittedStream(ctx, w, errReader{}, p, cancel)

	logs := buf.String()
	if !strings.Contains(logs, "stream: req=req-pump-1 upstream interrupted after commit") {
		t.Fatalf("pump log missing req=id, captured:\n%s", logs)
	}
}

// errReader fails on the first read — a committed upstream that dies
// mid-stream.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

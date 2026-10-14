package proxy

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRequestIDFor: a usable incoming X-Request-Id is reused verbatim, a
// missing/unusable one is replaced by a fresh 16-hex id, and the response
// header carries the final id either way.
func TestRequestIDFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Passthrough.
	rec := httptest.NewRecorder()
	r.Header.Set("X-Request-Id", "client-id-42")
	if got := requestIDFor(rec, r); got != "client-id-42" {
		t.Errorf("request id = %q, want the client's id", got)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "client-id-42" {
		t.Errorf("response header = %q, want the client's id", got)
	}

	// Generation.
	rec = httptest.NewRecorder()
	r.Header.Del("X-Request-Id")
	got := requestIDFor(rec, r)
	if len(got) != 16 {
		t.Errorf("generated request id = %q, want 16 hex chars", got)
	}
	if rec.Header().Get("X-Request-Id") != got {
		t.Errorf("response header = %q, want the generated id", got)
	}

	// Unusable ids are replaced: too long and non-printable.
	for _, bad := range []string{strings.Repeat("x", 100), "bad\nid"} {
		rec = httptest.NewRecorder()
		r.Header.Set("X-Request-Id", bad)
		got := requestIDFor(rec, r)
		if got == bad || len(got) != 16 {
			t.Errorf("unusable id %q not replaced: got %q", bad, got)
		}
	}
}

// TestAttemptLogCarriesRequestID: every upstream attempt is logged with the
// request id, candidate, attempt number, cause, duration and the ladder's
// decision (task F). The chain: provider-one 500s, one same-candidate retry
// (still 500), the walk falls to provider-two which serves 200.
func TestAttemptLogCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	var oneHits int32
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&oneHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
	defer okSrv.Close()

	fc, _ := newTwoProviderChain(failSrv.URL, okSrv.URL)
	fc.Cfg.Fallback.RetriesPerCandidate = 1
	fc.Cfg.Fallback.RetryOn429 = true
	fc.Cfg.Fallback.RetryOn5xx = true

	req := chainRequest()
	req.RequestID = "req-obs-1"
	resp, err := fc.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute error = %v, want provider-two's 200", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	logs := buf.String()
	for _, want := range []string{
		"req=req-obs-1",
		"candidate=provider-one/model-a",
		"candidate=provider-two/model-b",
		"decision=retry_same",
		"decision=fallback",
		"decision=ok",
		"duration=",
		"cause=",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("attempt log missing %q; logs:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "sk-Bearer") || strings.Contains(logs, "Authorization") {
		t.Errorf("attempt log leaked credential material:\n%s", logs)
	}
}

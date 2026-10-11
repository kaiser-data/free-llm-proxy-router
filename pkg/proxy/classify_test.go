package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestClassifyFailure covers every row of the retry-ladder decision table:
// the verdict must be exactly one of retry_same / fallback / fail_fast per
// failure class (second P0 pass, task C).
func TestClassifyFailure(t *testing.T) {
	// Wrap real context errors the way *url.Error does in production —
	// errors.Is/errors.As must see through them.
	resetErr := fmt.Errorf("post \"http://up\": read tcp 1.2.3.4:5->5.4.3.2:443: read: connection reset by peer")
	eofErr := fmt.Errorf("post \"http://up\": unexpected EOF")
	refusedErr := fmt.Errorf("post \"http://up\": dial tcp 1.2.3.4:443: connect: connection refused")
	canceledErr := fmt.Errorf("post \"http://up\": %w", context.Canceled)
	timeoutErr := fmt.Errorf("post \"http://up\": %w", context.DeadlineExceeded)

	tests := []struct {
		name string
		in   FailureInput
		want FailureDecision
	}{
		// --- transport failures (no response at all) ---
		{"connreset with retries left", FailureInput{Err: resetErr, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"connreset retries exhausted", FailureInput{Err: resetErr, RetriesDone: 1, MaxRetries: 1}, FallbackNext},
		{"unexpected eof with retries left", FailureInput{Err: eofErr, RetriesDone: 0, MaxRetries: 2}, RetrySame},
		{"connection refused with retries left", FailureInput{Err: refusedErr, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"client canceled", FailureInput{Err: canceledErr}, FailFast},
		{"first timeout retries same", FailureInput{Err: timeoutErr, TimeoutsDone: 0, RetryOn5xx: true, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"second timeout falls back", FailureInput{Err: timeoutErr, TimeoutsDone: 1, RetryOn5xx: true, RetriesDone: 0, MaxRetries: 2}, FallbackNext},
		{"timeout gated by retry_on_5xx", FailureInput{Err: timeoutErr, TimeoutsDone: 0, RetryOn5xx: false, RetriesDone: 0, MaxRetries: 1}, FallbackNext},
		{"timeout with no retries left", FailureInput{Err: timeoutErr, TimeoutsDone: 0, RetryOn5xx: true, RetriesDone: 1, MaxRetries: 1}, FallbackNext},

		// --- 429 ---
		{"429 flag on retries same", FailureInput{StatusCode: 429, RetryOn429: true, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"429 flag off falls back", FailureInput{StatusCode: 429, RetryOn429: false}, FallbackNext},
		{"429 retry-after longer than budget", FailureInput{StatusCode: 429, RetryOn429: true, RetryAfter: 60 * time.Second, RemainingBudget: 10 * time.Second}, FallbackNext},
		{"429 retry-after fits budget", FailureInput{StatusCode: 429, RetryOn429: true, RetryAfter: 2 * time.Second, RemainingBudget: 30 * time.Second, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"429 retries exhausted", FailureInput{StatusCode: 429, RetryOn429: true, RetriesDone: 1, MaxRetries: 1}, FallbackNext},

		// --- 408 ---
		{"408 with retries left", FailureInput{StatusCode: http.StatusRequestTimeout, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"408 retries exhausted", FailureInput{StatusCode: http.StatusRequestTimeout, RetriesDone: 1, MaxRetries: 1}, FallbackNext},

		// --- per-model / per-provider availability ---
		{"404 model gone", FailureInput{StatusCode: 404}, FallbackNext},
		{"401 auth", FailureInput{StatusCode: 401}, FallbackNext},
		{"403 auth", FailureInput{StatusCode: 403}, FallbackNext},

		// --- fail-fast payload classes ---
		{"413 payload too large", FailureInput{StatusCode: 413}, FailFast},
		{"400 genuine payload defect", FailureInput{StatusCode: 400, Body: []byte(`{"error":{"message":"messages is invalid"}}`)}, FailFast},
		{"400 empty body", FailureInput{StatusCode: 400}, FailFast},
		{"400 content policy refusal", FailureInput{StatusCode: 400, Body: []byte(`{"error":{"code":"content_policy_violation"}}`)}, FailFast},
		{"400 context length exceeded", FailureInput{StatusCode: 400, Body: []byte(`{"error":{"message":"This model's maximum context length is 8192 tokens"}}`)}, FallbackNext},
		{"400 unsupported modality", FailureInput{StatusCode: 400, Body: []byte(`{"error":{"message":"image input is not supported by this model"}}`)}, FallbackNext},
		{"409 conflict", FailureInput{StatusCode: 409}, FailFast},
		{"422 unprocessable", FailureInput{StatusCode: 422}, FailFast},

		// --- 5xx ---
		{"500 flag on retries same", FailureInput{StatusCode: 500, RetryOn5xx: true, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"502 retries same", FailureInput{StatusCode: 502, RetryOn5xx: true, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"503 retries same", FailureInput{StatusCode: 503, RetryOn5xx: true, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"504 retries same", FailureInput{StatusCode: 504, RetryOn5xx: true, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"529 overloaded retries same", FailureInput{StatusCode: 529, RetryOn5xx: true, RetriesDone: 0, MaxRetries: 1}, RetrySame},
		{"500 retries exhausted", FailureInput{StatusCode: 500, RetryOn5xx: true, RetriesDone: 1, MaxRetries: 1}, FallbackNext},
		{"500 flag off falls back", FailureInput{StatusCode: 500, RetryOn5xx: false}, FallbackNext},
		{"503 retry-after longer than budget", FailureInput{StatusCode: 503, RetryOn5xx: true, RetryAfter: 120 * time.Second, RemainingBudget: 30 * time.Second}, FallbackNext},

		// --- success is not a failure (defensive default) ---
		{"200 handled by callers", FailureInput{StatusCode: 200}, RetrySame},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyFailure(tt.in); got != tt.want {
				t.Errorf("ClassifyFailure(%+v) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseRetryAfter covers the two Retry-After formats and the invalid
// cases (all of which must yield 0, never a negative sleep).
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"delta seconds", "30", 30 * time.Second},
		{"delta seconds zero", "0", 0},
		{"delta seconds negative", "-5", 0},
		{"empty", "", 0},
		{"garbage", "soon", 0},
		{"http date future", now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{"http date past", now.Add(-90 * time.Second).UTC().Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.value, now)
			if got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// fakeNetTimeout mimics the net.Error the http.Client produces when its
// timeout cap fires (Timeout()=true, and Unwrap sees context.DeadlineExceeded
// in production).
type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "read tcp: i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

// TestIsTimeoutErr: net.Error timeouts and context deadlines classify as
// timeouts; plain transport errors and cancels do not.
func TestIsTimeoutErr(t *testing.T) {
	if !isTimeoutErr(fmt.Errorf("post: %w", fakeNetTimeout{})) {
		t.Errorf("net.Error timeout should classify as timeout")
	}
	if !isTimeoutErr(fmt.Errorf("post: %w", context.DeadlineExceeded)) {
		t.Errorf("context deadline should classify as timeout")
	}
	if isTimeoutErr(fmt.Errorf("post: %w", context.Canceled)) {
		t.Errorf("canceled is not a timeout")
	}
	if isTimeoutErr(errors.New("connection reset by peer")) {
		t.Errorf("reset is not a timeout")
	}
}

// TestFinalFailureDecision: at walk level (retries spent) every retryable
// class collapses to FallbackNext and only true client/context defects stay
// FailFast.
func TestFinalFailureDecision(t *testing.T) {
	resp := &Response{StatusCode: 500, Body: []byte("boom")}
	if got := finalFailureDecision(resp, nil); got != FallbackNext {
		t.Errorf("final 500 = %s, want fallback", got)
	}
	if got := finalFailureDecision(nil, context.Canceled); got != FailFast {
		t.Errorf("final canceled = %s, want fail_fast", got)
	}
	if got := finalFailureDecision(&Response{StatusCode: 400, Body: []byte("bad payload")}, nil); got != FailFast {
		t.Errorf("final 400 = %s, want fail_fast", got)
	}
	if got := finalFailureDecision(&Response{StatusCode: 401}, nil); got != FallbackNext {
		t.Errorf("final 401 = %s, want fallback", got)
	}
}

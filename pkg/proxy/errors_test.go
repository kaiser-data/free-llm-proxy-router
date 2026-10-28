package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeOpenAIError(t *testing.T, rec *httptest.ResponseRecorder) (msg, code string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not OpenAI-style JSON: %v: %s", err, rec.Body)
	}
	if body.Error.Message == "" || body.Error.Type == "" {
		t.Errorf("missing message/type: %s", rec.Body)
	}
	return body.Error.Message, body.Error.Code
}

func TestErrors_InvalidJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{not json"))
	(&Server{}).handleChatCompletions(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if _, code := decodeOpenAIError(t, rec); code != "invalid_json" {
		t.Errorf("code = %q, want invalid_json", code)
	}
}

func TestErrors_MessageIsEscaped(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusNotFound, "invalid_request_error", "model_not_found", `model "a\b" not found`)
	if msg, _ := decodeOpenAIError(t, rec); msg != `model "a\b" not found` {
		t.Errorf("message = %q", msg)
	}
}

func TestErrors_Unauthorized(t *testing.T) {
	h := authMiddleware("secret", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached without token")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	decodeOpenAIError(t, rec)
}

func TestErrors_BodyTooLarge(t *testing.T) {
	rec := httptest.NewRecorder()
	big := strings.NewReader(`{"model":"auto","x":"` + strings.Repeat("a", maxRequestBodyBytes) + `"}`)
	(&Server{}).handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", big))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if _, code := decodeOpenAIError(t, rec); code != "request_too_large" {
		t.Errorf("code = %q, want request_too_large", code)
	}
}

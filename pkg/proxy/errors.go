package proxy

import (
	"encoding/json"
	"net/http"
	"time"
)

// writeError sends an OpenAI-style error body, so client SDKs surface the
// message instead of failing to parse it:
//
//	{"error":{"message":"…","type":"invalid_request_error","param":null,"code":"…"}}
func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    code,
		},
	})
}

// defaultRetryAfter is suggested when no provider cooldown tells us better.
const defaultRetryAfter = 5 * time.Second

// retryAfterSeconds turns the earliest provider recovery time into a
// Retry-After value: whole seconds, rounded up, at least 1.
func retryAfterSeconds(recovery time.Time) int {
	wait := defaultRetryAfter
	if !recovery.IsZero() {
		wait = time.Until(recovery)
	}
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

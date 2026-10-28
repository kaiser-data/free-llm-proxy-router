package proxy

import (
	"encoding/json"
	"net/http"
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

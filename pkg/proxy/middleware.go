package proxy

import (
	"log"
	"net/http"
	"time"
)

// authMiddleware checks the optional proxy auth token.
// If no token is configured, all requests pass through.
// API keys from upstream providers are NEVER logged.
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got == "Bearer "+token || got == token {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	})
}

// loggingMiddleware logs request method, path, request id, status, and
// duration. It does NOT log Authorization headers, request bodies, or API
// keys. The request id is read back from the X-Request-Id response header
// (requestIDFor pins it) — "-" when the handler did not set one.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		reqID := rw.Header().Get("X-Request-Id")
		if reqID == "" {
			reqID = "-"
		}
		log.Printf("%s %s req=%s %d %v", r.Method, r.URL.Path, reqID, rw.status, time.Since(start))
	})
}

// recoveryMiddleware catches panics and returns a 500.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
// It forwards Flush so streaming handlers keep their per-read flushing
// through the middleware stack — without this, the http.Flusher type
// assertion in StreamProxy.Forward fails and SSE responses buffer until
// the handler returns.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

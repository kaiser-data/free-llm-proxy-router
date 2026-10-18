package proxy

import (
	"net/http"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// Idle-based write deadlines (task S).
//
// The old mechanism set http.Server.WriteTimeout from
// proxy.write_timeout_seconds — a TOTAL ceiling fixed once per response:
// a stream that kept flowing past it was cut mid-generation, and a queued
// request (task L0) with an effective deadline above the ceiling ended in
// a connection cut instead of a clean error.
//
// The new mechanism moves the deadline per request:
//
//   - The server-level WriteTimeout is DISABLED (0).
//   - The outer middleware sets one initial write deadline per request at
//     proxy.max_request_seconds past its start (the total ceiling; unset →
//     the automatic queue_wait + attempt + stream + idle computation).
//   - Every SUCCESSFUL write/flush refreshes the deadline to
//     now + proxy.write_idle_timeout_seconds (default 30s), clamped to the
//     request's ceiling — a flowing stream can never again be cut by a
//     fixed total write timeout, while a client that stops reading gets cut
//     after the idle window because the blocked write can no longer be
//     refreshed.
//
// All writes go through the middleware's idleRefreshWriter (it is the
// http.ResponseWriter the handler chain receives, and the keepalive writer
// (task P) delegates onto it), so keepalive comments, SSE data chunks, and
// buffered bodies all refresh the deadline the same way.

// writeDeadlineCtl owns one request's write deadline: the total ceiling and
// the idle refresh. Best-effort: transports without deadline support
// (tests' ResponseRecorder) just ignore the refresh.
type writeDeadlineCtl struct {
	rc      *http.ResponseController
	idle    time.Duration
	ceiling time.Time
}

// newWriteDeadlineCtl sets the request's initial (total-ceiling) write
// deadline and returns the controller used to refresh it after writes.
func newWriteDeadlineCtl(w http.ResponseWriter, idle time.Duration, ceiling time.Time) *writeDeadlineCtl {
	rc := http.NewResponseController(w)
	if !ceiling.IsZero() {
		_ = rc.SetWriteDeadline(ceiling)
	}
	return &writeDeadlineCtl{rc: rc, idle: idle, ceiling: ceiling}
}

// refresh moves the write deadline to now+idle (clamped to the ceiling)
// after a successful write/flush. Returns the deadline it set — zero when
// nothing was set (idle disabled); tests assert on the computed value.
func (c *writeDeadlineCtl) refresh() time.Time {
	if c == nil || c.idle <= 0 {
		return time.Time{}
	}
	d := time.Now().Add(c.idle)
	if !c.ceiling.IsZero() && d.After(c.ceiling) {
		d = c.ceiling
	}
	_ = c.rc.SetWriteDeadline(d)
	return d
}

// idleRefreshWriter is the ResponseWriter every handler writes through:
// one initial total-ceiling deadline at construction, one idle refresh per
// successful body byte or flush.
type idleRefreshWriter struct {
	http.ResponseWriter
	ctl *writeDeadlineCtl
}

func (w *idleRefreshWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		w.ctl.refresh()
	}
	return n, err
}

func (w *idleRefreshWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
		w.ctl.refresh()
	}
}

// withWriteDeadlines is the outer middleware: it installs the request's
// write-deadline controller and swaps in the refreshing writer.
func (s *Server) withWriteDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		maxReq := EffectiveMaxRequestSeconds(cfg)
		ctl := newWriteDeadlineCtl(w, WriteIdleTimeout(cfg), time.Now().Add(maxReq))
		next.ServeHTTP(&idleRefreshWriter{ResponseWriter: w, ctl: ctl}, r)
	})
}

// WriteIdleTimeout resolves proxy.write_idle_timeout_seconds (default 30s,
// <=0 guards to the default) — the post-write idle window for the response.
func WriteIdleTimeout(c *config.Config) time.Duration {
	if c.Proxy.WriteIdleTimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.Proxy.WriteIdleTimeoutSeconds) * time.Second
}

// EffectiveMaxRequestSeconds resolves the request's total write ceiling:
// proxy.max_request_seconds when set, otherwise the automatic
// queue_wait + attempt ceiling + stream ceiling + write-idle computation —
// large enough for a request to queue its full budget, spend its whole
// attempt budget, and stream the longest accepted stream.
func EffectiveMaxRequestSeconds(c *config.Config) time.Duration {
	if c.Proxy.MaxRequestSeconds > 0 {
		return time.Duration(c.Proxy.MaxRequestSeconds) * time.Second
	}
	attempt := c.Fallback.ProviderTimeoutSeconds
	if attempt <= 0 {
		attempt = 120 // mirror the viper default for zeroed structs
	}
	for i := range c.Providers {
		if !c.Providers[i].Enabled {
			continue
		}
		if d := c.Providers[i].ResolvedTimeout(); int(d.Seconds()) > attempt {
			attempt = int(d.Seconds())
		}
	}
	stream := c.Fallback.StreamTimeoutSeconds
	if stream <= 0 {
		stream = attempt // 0 inherits provider_timeout_seconds (NG6)
	}
	queue := c.Fallback.QueueWaitSeconds
	if queue <= 0 {
		queue = 0
	}
	return time.Duration(queue+attempt+stream)*time.Second + WriteIdleTimeout(c)
}

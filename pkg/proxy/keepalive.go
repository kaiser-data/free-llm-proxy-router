package proxy

import (
	"net/http"
	"sync"
	"time"
)

// SSE keepalive for WAITING streaming requests (task P).
//
// A streaming request can spend a long time with nothing to say: waiting
// in the slot queue (task L0) or in the pre-commit hold for the first
// usable event (task J). Idle agents, LBs and proxies cut such connections.
// Once the wait passes fallback.stream_keepalive_after_seconds (default 5)
// the proxy sends the 200 SSE header and ": keepalive\n\n" comment events —
// spaced fallback.stream_keepalive_interval_seconds (default 15) apart,
// flushed every time — so the connection stays visibly alive without a
// single data byte being committed.
//
// Commit-state is split into two independent flags (the spec's hard rule):
//
//	headerSent    — WriteHeader(200) went out (by keepalive or commit);
//	                WriteHeader/Write afterwards is guarded to exactly once;
//	dataCommitted — the first DATA byte reached the client (a completion
//	                chunk, passthrough JSON, or a surfaced upstream error
//	                body). After this the walk must not touch the response.
//
// Silent fallback stays possible until dataCommitted: comments are not
// data, so the client that saw only keepalive comments has received no
// completion content and the ladder may still walk to another candidate.
// A failure AFTER headerSent ends in the OpenAI-style SSE error event +
// data: [DONE] (writeSSEErrorEvent) — the status can no longer change.
//
// Non-streaming requests never get keepalive: they wait silently within
// the deadline and answer with their real status code.
type keepaliveWriter struct {
	http.ResponseWriter
	flusher  http.Flusher
	canFlush bool

	requestID string
	after     time.Duration
	interval  time.Duration

	mu            sync.Mutex
	armed         bool
	stopped       bool
	headerSent    bool
	dataCommitted bool
	stopCh        chan struct{}
	doneCh        chan struct{}
}

// newKeepaliveWriter wraps w. after/interval come from the config (seconds,
// viper defaults 5/15); after <= 0 disables the keepalive entirely — the
// caller passes the original writer through untouched.
func newKeepaliveWriter(w http.ResponseWriter, requestID string, after, interval time.Duration) *keepaliveWriter {
	f, canFlush := flusherOf(w)
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &keepaliveWriter{
		ResponseWriter: w,
		flusher:        f,
		canFlush:       canFlush,
		requestID:      requestID,
		after:          after,
		interval:       interval,
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
}

// arm starts the keepalive watchdog; idempotent — the FIRST arm fixes the
// window (the "wait" is the request's total waiting time across queue and
// hold phases).
func (k *keepaliveWriter) arm() {
	k.mu.Lock()
	if k.armed || k.stopped {
		k.mu.Unlock()
		return
	}
	k.armed = true
	k.mu.Unlock()
	go k.watch()
}

// watch is the keepalive goroutine: sleep `after`, then send the 200 SSE
// header + first comment, then a comment per interval until stopped.
func (k *keepaliveWriter) watch() {
	defer close(k.doneCh)
	timer := time.NewTimer(k.after)
	defer timer.Stop()
	select {
	case <-k.stopCh:
		return
	case <-timer.C:
	}
	k.sendHeader()
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.stopCh:
			return
		case <-ticker.C:
			k.writeComment()
		}
	}
}

// sendHeader writes the 200 SSE header (once) and the first keepalive
// comment. A commit that won the race makes this a no-op — the client then
// sees the real completion with no stray header state.
func (k *keepaliveWriter) sendHeader() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped || k.dataCommitted || k.headerSent {
		return
	}
	w := k.ResponseWriter
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if k.requestID != "" {
		w.Header().Set("X-Request-Id", k.requestID)
	}
	w.WriteHeader(http.StatusOK)
	k.headerSent = true
	k.commentLocked()
}

// commentLocked writes one ": keepalive\n\n" and flushes. It deliberately
// does NOT touch dataCommitted — comments are not completion content.
// Caller holds k.mu.
func (k *keepaliveWriter) commentLocked() {
	if _, err := k.ResponseWriter.Write([]byte(": keepalive\n\n")); err != nil {
		return // client gone — nobody left to tell
	}
	if k.canFlush {
		k.flusher.Flush()
	}
}

// writeComment is the interval tick entry point.
func (k *keepaliveWriter) writeComment() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped || k.dataCommitted {
		return
	}
	k.commentLocked()
}

// WriteHeader is guarded to exactly once — a second call would panic the
// handler ("superfluous WriteHeader") or corrupt the stream.
func (k *keepaliveWriter) WriteHeader(code int) {
	k.mu.Lock()
	committed := k.headerSent
	if !committed {
		k.headerSent = true
	}
	k.mu.Unlock()
	if committed {
		return
	}
	k.ResponseWriter.WriteHeader(code)
}

// Write serializes with the keepalive goroutine and marks the response as
// data-committed: any body byte (completion chunk, passthrough JSON, error
// body) is completion content — after it the walk is terminal.
func (k *keepaliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	k.dataCommitted = true
	k.headerSent = true
	k.mu.Unlock()
	return k.ResponseWriter.Write(p)
}

// Flush delegates the underlying flusher.
func (k *keepaliveWriter) Flush() {
	if k.canFlush {
		k.flusher.Flush()
	}
}

// stop shuts the watchdog down and waits for its goroutine to exit, so the
// handler never returns while a keepalive write is in flight.
func (k *keepaliveWriter) stop() {
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		return
	}
	k.stopped = true
	wasArmed := k.armed
	close(k.stopCh)
	k.mu.Unlock()
	if wasArmed {
		<-k.doneCh
	}
}

// HeaderSent reports whether the 200 SSE header (or any body byte) already
// went out — the point after which errors must be SSE events, not statuses.
func (k *keepaliveWriter) HeaderSent() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.headerSent
}

// DataCommitted reports whether the first completion-content byte reached
// the client — the point after which the walk is terminal.
func (k *keepaliveWriter) DataCommitted() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.dataCommitted
}

// keepaliveOf unwraps w when it is a keepalive writer; nil otherwise.
func keepaliveOf(w http.ResponseWriter) *keepaliveWriter {
	kw, _ := w.(*keepaliveWriter)
	return kw
}

// armKeepalive arms the keepalive when w carries one — the entry point for
// the queue wait (ExecuteStream) and the pre-commit hold
// (streamUpstreamHeld).
func armKeepalive(w http.ResponseWriter) {
	if kw := keepaliveOf(w); kw != nil {
		kw.arm()
	}
}

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// upstreamStream is a provider SSE response the chain has committed to: the
// first content chunk has arrived, so the stream is handed to the client and
// fallback is no longer possible.
//
// Everything before the commit point (role-only chunks, reasoning deltas,
// comments) is held in prefix and replayed first, so the client sees the
// stream exactly as the provider sent it.
type upstreamStream struct {
	prefix  []byte
	rest    *bufio.Reader
	closers []func()
}

// Close releases the upstream connection and the contexts that bound it.
func (s *upstreamStream) Close() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
}

func (s *upstreamStream) onClose(f func()) { s.closers = append(s.closers, f) }

// maxStreamPrefix bounds what is buffered while waiting for the first content
// chunk (a reasoning model can think for a while). Past it the stream is
// committed anyway: the client gets its bytes, the chain gives up fallback.
const maxStreamPrefix = 1 << 20

// errStreamEndedEarly marks a stream that closed before any content arrived.
var errStreamEndedEarly = errors.New("stream ended before the first content chunk")

// peekStream reads an SSE body until the first chunk that carries content or a
// tool call, so the chain can still fall back on everything that goes wrong
// before the client has seen a byte. It returns one of:
//
//   - a committed stream (resp.stream set) — content is flowing;
//   - an empty stream (resp.emptyStream) — [DONE] arrived with no content;
//   - an upstream error (StatusCode 502, Body = the error event) — some
//     providers answer 200 and then send {"error": …} as the first event;
//   - err — the connection broke before any content.
func peekStream(status int, header http.Header, body io.Reader) (*Response, error) {
	br := bufio.NewReaderSize(body, 32<<10)
	var prefix bytes.Buffer
	usedModel := ""
	for {
		line, err := br.ReadBytes('\n')
		prefix.Write(line)
		if payload, ok := ssePayload(line); ok {
			if bytes.Equal(payload, []byte("[DONE]")) {
				return &Response{StatusCode: status, Header: header, Body: prefix.Bytes(), emptyStream: true}, nil
			}
			var chunk streamChunk
			if json.Unmarshal(payload, &chunk) == nil {
				if chunk.Error != nil {
					return &Response{StatusCode: http.StatusBadGateway, Header: header, Body: append([]byte(nil), payload...)}, nil
				}
				if usedModel == "" {
					usedModel = chunk.Model
				}
				if chunk.hasContent() || prefix.Len() > maxStreamPrefix {
					return &Response{
						StatusCode: status,
						Header:     header,
						UsedModel:  usedModel,
						stream:     &upstreamStream{prefix: prefix.Bytes(), rest: br},
					}, nil
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil, errStreamEndedEarly
			}
			return nil, err
		}
	}
}

// ssePayload returns the payload of an SSE "data:" line.
func ssePayload(line []byte) ([]byte, bool) {
	line = bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimSpace(line[len("data:"):]), true
}

type streamChunk struct {
	Model   string `json:"model"`
	Error   any    `json:"error"`
	Choices []struct {
		Delta struct {
			Content      any   `json:"content"`
			ToolCalls    []any `json:"tool_calls"`
			FunctionCall any   `json:"function_call"`
		} `json:"delta"`
		Text string `json:"text"`
	} `json:"choices"`
}

// hasContent mirrors isEmptyCompletion: text, tool calls or a function call
// count, reasoning alone does not (a reasoning model can spend its whole
// max_tokens budget thinking and answer nothing).
func (c streamChunk) hasContent() bool {
	for _, ch := range c.Choices {
		d := ch.Delta
		if len(d.ToolCalls) > 0 || d.FunctionCall != nil || ch.Text != "" {
			return true
		}
		if s, ok := d.Content.(string); ok && s != "" {
			return true
		}
		if d.Content != nil && hasContent(d.Content) {
			return true
		}
	}
	return false
}

// writeUpstreamStream pipes a committed provider stream to the client,
// flushing as bytes arrive. Once the first byte is written the status is
// final; an upstream failure after that can only end the stream.
func writeUpstreamStream(w http.ResponseWriter, resp *Response) {
	s := resp.stream
	defer s.Close()

	copyUpstreamHeaders(w.Header(), resp.Header)
	if resp.UsedModel != "" {
		w.Header().Set("X-Used-Model", resp.UsedModel)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	if _, err := w.Write(s.prefix); err != nil {
		return
	}
	flush()
	buf := make([]byte, 16<<10)
	for {
		n, err := s.rest.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client went away; Close cancels the upstream call
			}
			flush()
		}
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "context canceled") {
				log.Printf("stream: upstream ended mid-stream: %v", err)
			}
			return
		}
	}
}

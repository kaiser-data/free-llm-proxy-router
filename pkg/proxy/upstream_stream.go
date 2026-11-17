package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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

// streamReadBuffer is the read buffer for an upstream SSE stream.
const streamReadBuffer = 32 << 10

// peekStream reads an SSE body until the first event that carries content or
// a tool call, so the chain can still fall back on everything that goes wrong
// before the client has seen a byte. It returns one of:
//
//   - a committed stream (resp.stream set) — content is flowing;
//   - an empty stream (resp.emptyStream) — the stream ended ([DONE] or EOF)
//     without content, which falls back like an empty completion;
//   - an upstream error (Body = the error event) — some providers answer 200
//     and then send {"error": …}; its code becomes the status (400, 429, …)
//     so it is classified like the same error sent as a plain HTTP response;
//   - err — the connection broke before any content.
//
// Events follow the SSE spec: "data:" lines are joined until a blank line.
// Memory stays bounded by maxStreamPrefix whatever the provider sends: past
// it the stream is committed and piped as is.
func peekStream(status int, header http.Header, body io.Reader) (*Response, error) {
	br := bufio.NewReaderSize(body, streamReadBuffer)
	var prefix, line, data bytes.Buffer
	usedModel := ""
	commit := func() *Response {
		return &Response{
			StatusCode: status,
			Header:     header,
			UsedModel:  usedModel,
			stream:     &upstreamStream{prefix: prefix.Bytes(), rest: br},
		}
	}
	// dispatch handles one complete event; a non-nil result ends the peek.
	dispatch := func() *Response {
		payload := bytes.TrimSpace(data.Bytes())
		data.Reset()
		if len(payload) == 0 {
			return nil
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			return &Response{StatusCode: status, Header: header, Body: prefix.Bytes(), emptyStream: true}
		}
		var chunk streamChunk
		if json.Unmarshal(payload, &chunk) != nil {
			return nil
		}
		if chunk.Error != nil {
			return &Response{StatusCode: streamErrorStatus(chunk.Error), Header: header, Body: append([]byte(nil), payload...)}
		}
		if usedModel == "" {
			usedModel = chunk.Model
		}
		if chunk.hasContent() {
			return commit()
		}
		return nil
	}
	for {
		frag, err := br.ReadSlice('\n')
		prefix.Write(frag)
		if line.Len() <= maxStreamPrefix {
			line.Write(frag)
		}
		if err == bufio.ErrBufferFull {
			if prefix.Len() > maxStreamPrefix {
				return commit(), nil // a runaway line: stop buffering, pass it on
			}
			continue
		}
		l := bytes.TrimRight(line.Bytes(), "\r\n")
		switch {
		case len(l) == 0 && line.Len() > 0: // blank line: end of event
			if r := dispatch(); r != nil {
				return r, nil
			}
		case bytes.HasPrefix(l, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(bytes.TrimPrefix(l, []byte("data:")), []byte(" ")))
		}
		line.Reset()
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			if r := dispatch(); r != nil { // a last event without its blank line
				return r, nil
			}
			return &Response{StatusCode: status, Header: header, Body: prefix.Bytes(), emptyStream: true}, nil
		}
		if prefix.Len() > maxStreamPrefix {
			return commit(), nil
		}
	}
}

// streamErrorStatus maps an in-stream error object to an HTTP status: its
// "code" when that is one (OpenRouter sends {"code":400,…}), else 502.
func streamErrorStatus(e any) int {
	m, _ := e.(map[string]any)
	var code int
	switch c := m["code"].(type) {
	case float64:
		code = int(c)
	case string:
		code, _ = strconv.Atoi(c)
	}
	if code >= 400 && code <= 599 {
		return code
	}
	return http.StatusBadGateway
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
		if len(d.ToolCalls) > 0 || d.FunctionCall != nil {
			return true
		}
		if hasContent(d.Content) || strings.TrimSpace(ch.Text) != "" {
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
			if err != io.EOF {
				// Cut mid-answer (request deadline, upstream reset). Say so:
				// a stream that just stops looks like a complete, shorter
				// answer to an SDK. If the client is the one that left, this
				// write fails harmlessly.
				log.Printf("stream: upstream ended mid-stream: %v", err)
				writeStreamError(w, "upstream stream interrupted: "+err.Error())
				flush()
			}
			return
		}
	}
}

// writeStreamError ends an SSE stream with an OpenAI-style error event.
func writeStreamError(w io.Writer, msg string) {
	ev, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": "server_error", "code": "stream_interrupted",
	}})
	fmt.Fprintf(w, "data: %s\n\n", ev)
}

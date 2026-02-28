package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/config"
)

// StreamProxy transparently forwards a streaming (SSE) response from a provider
// back to the client. It does not buffer the full response.
type StreamProxy struct {
	HTTPClient *http.Client
}

// Forward proxies a streaming chat-completions request to the given provider.
// It streams SSE chunks from the provider directly to w.
func (sp *StreamProxy) Forward(ctx context.Context, w http.ResponseWriter, cfg config.ProviderConfig, body map[string]any) error {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/v1/chat/completions"

	// Ensure stream is set
	body = copyMap(body)
	body["stream"] = true

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding stream request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("building stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	h, v := cfg.ResolvedAuth()
	if v != "" && v != "Bearer " {
		req.Header.Set(h, v)
	}
	for k, val := range cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}

	resp, err := sp.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stream: provider %s status %d: %s", cfg.ID, resp.StatusCode, body)
	}

	// Set SSE headers on the client response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			log.Printf("stream: client write error: %v", err)
			return nil
		}
		// Flush after blank lines (SSE event boundaries) and data lines
		if canFlush && (line == "" || strings.HasPrefix(line, "data:")) {
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Printf("stream: scanner error: %v", err)
	}
	return nil
}

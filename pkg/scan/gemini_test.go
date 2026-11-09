package scan

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

const testGeminiKey = "AIzaTEST-not-a-real-key"

// The key must travel in the x-goog-api-key header: a key in the URL ends up
// in *url.Error messages, which the scanner logs.
func TestGeminiScan_KeyInHeaderNotURL(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if strings.Contains(r.URL.String(), testGeminiKey) {
			t.Errorf("API key leaked into URL: %s", r.URL)
		}
		if got := r.Header.Get("x-goog-api-key"); got != testGeminiKey {
			t.Errorf("x-goog-api-key = %q", got)
		}
		if r.URL.Query().Get("pageToken") == "" {
			io.WriteString(w, `{"models":[{"name":"models/gemma-a","supportedGenerationMethods":["generateContent"]}],"nextPageToken":"p2"}`)
			return
		}
		io.WriteString(w, `{"models":[{"name":"models/gemma-b","supportedGenerationMethods":["generateContent"]}]}`)
	}))
	defer srv.Close()

	cfg := config.ProviderConfig{ID: "gemini", APIKey: testGeminiKey}
	cfg.Discovery.ModelsEndpoint = srv.URL + "/v1beta/models"
	entries, err := (&GeminiScanner{Client: srv.Client()}).ScanFreeModels(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 || len(entries) != 2 {
		t.Errorf("pages = %d, entries = %d; want 2 and 2", pages, len(entries))
	}
}

func TestGeminiScan_TransportErrorHasNoKey(t *testing.T) {
	cfg := config.ProviderConfig{ID: "gemini", APIKey: testGeminiKey}
	cfg.Discovery.ModelsEndpoint = "http://127.0.0.1:1/v1beta/models" // nothing listens on port 1
	_, err := (&GeminiScanner{Client: &http.Client{}}).ScanFreeModels(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), testGeminiKey) {
		t.Errorf("error message contains the API key: %v", err)
	}
}

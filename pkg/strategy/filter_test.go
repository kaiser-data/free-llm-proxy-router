package strategy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

func mustBuild(t *testing.T, cfgs ...config.FilterConfig) Filters {
	t.Helper()
	fs, err := BuildFilters(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func filterCfg(name string, kv ...any) config.FilterConfig {
	s := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		s[kv[i].(string)] = kv[i+1]
	}
	return config.FilterConfig{Name: name, Settings: s}
}

// The shipped example must load and build, or every fresh install fails.
func TestBuildFilters_ExampleConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fs, err := BuildFilters(cfg.Proxy.Filters)
	if err != nil {
		t.Fatalf("example filters do not build: %v", err)
	}
	if got := strings.Join(fs.Names(), ","); got != "context-fit,tool-calling" {
		t.Errorf("filters = %s, want context-fit,tool-calling", got)
	}
	if fs[0].Filter.(*contextFitFilter).margin != 1.2 {
		t.Errorf("context-fit margin not read from the example: %+v", fs[0].Filter)
	}
}

func TestBuildFilters_RejectsTypos(t *testing.T) {
	cases := map[string]config.FilterConfig{
		"unknown filter":         filterCfg("context-fits"),
		"empty name":             filterCfg(""),
		"bad unknown value":      filterCfg("tool-calling", "unknown", "maybe"),
		"unknown setting":        filterCfg("context-fit", "margn", 1.5),
		"tool-calling setting":   filterCfg("tool-calling", "margin", 1.5),
		"margin below 1":         filterCfg("context-fit", "margin", 0.5),
		"margin is not a number": filterCfg("context-fit", "margin", "1.2"),
	}
	for name, fc := range cases {
		if _, err := BuildFilters([]config.FilterConfig{fc}); err == nil {
			t.Errorf("%s: %+v built without error", name, fc)
		}
	}
}

func TestContextFit(t *testing.T) {
	req := Request{EstimatedPromptTokens: 800, MaxTokens: 200} // 1000 * 1.2 = 1200
	cases := []struct {
		name    string
		window  int
		unknown string
		keep    bool
	}{
		{"fits", 1200, "", true},
		{"too small", 1199, "", false},
		{"no context window, default keep", 0, "", true},
		{"no context window, unknown drop", 0, "drop", false},
		{"too small, unknown keep does not rescue it", 1000, "keep", false},
	}
	for _, c := range cases {
		fc := filterCfg("context-fit")
		if c.unknown != "" {
			fc.Settings["unknown"] = c.unknown
		}
		fs := mustBuild(t, fc)
		e := catalog.CatalogEntry{ProviderID: "p", ModelID: "m", ContextWindow: c.window}
		if got := fs.Keeps(req, e); got != c.keep {
			t.Errorf("%s: keep = %v, want %v", c.name, got, c.keep)
		}
	}
}

func TestToolCalling(t *testing.T) {
	withParams := func(params ...string) catalog.CatalogEntry {
		return catalog.CatalogEntry{Metadata: map[string]any{"supported_parameters": params}}
	}
	// After a catalog round-trip through JSON the list is []any.
	var reloaded catalog.CatalogEntry
	if err := json.Unmarshal([]byte(`{"metadata":{"supported_parameters":["temperature","tools"]}}`), &reloaded); err != nil {
		t.Fatal(err)
	}
	tools := Request{HasTools: true}

	cases := []struct {
		name    string
		req     Request
		e       catalog.CatalogEntry
		unknown string
		keep    bool
	}{
		{"no tools in request", Request{}, withParams("temperature"), "drop", true},
		{"supports tools", tools, withParams("temperature", "tools"), "", true},
		{"supports tools, from JSON", tools, reloaded, "", true},
		{"listed params without tools is a definite drop", tools, withParams("temperature"), "keep", false},
		{"empty list is a definite drop", tools, withParams(), "keep", false},
		{"no metadata, default keep", tools, catalog.CatalogEntry{}, "", true},
		{"no metadata, unknown drop", tools, catalog.CatalogEntry{}, "drop", false},
	}
	for _, c := range cases {
		fc := filterCfg("tool-calling")
		if c.unknown != "" {
			fc.Settings["unknown"] = c.unknown
		}
		if got := mustBuild(t, fc).Keeps(c.req, c.e); got != c.keep {
			t.Errorf("%s: keep = %v, want %v", c.name, got, c.keep)
		}
	}
}

func TestFilters_Apply(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ModelID: "small", ContextWindow: 100},
		{ModelID: "no-tools", ContextWindow: 100000, Metadata: map[string]any{"supported_parameters": []string{"temperature"}}},
		{ModelID: "unknown", ContextWindow: 0},
		{ModelID: "good", ContextWindow: 100000, Metadata: map[string]any{"supported_parameters": []string{"tools"}}},
	}
	req := Request{EstimatedPromptTokens: 1000, HasTools: true}

	if got := Filters(nil).Apply(req, entries); len(got) != len(entries) {
		t.Errorf("no filters: %d entries, want all %d", len(got), len(entries))
	}

	fs := mustBuild(t, filterCfg("context-fit"), filterCfg("tool-calling"))
	var ids []string
	before := append([]catalog.CatalogEntry(nil), entries...)
	for _, e := range fs.Apply(req, entries) {
		ids = append(ids, e.ModelID)
	}
	// The proxy shares one free-entry slice across requests: Apply must not
	// write into it.
	if !reflect.DeepEqual(entries, before) {
		t.Errorf("Apply modified its input: %+v, was %+v", entries, before)
	}
	if got := strings.Join(ids, ","); got != "unknown,good" {
		t.Errorf("kept %s, want unknown,good (order preserved)", got)
	}

	fs = mustBuild(t, filterCfg("context-fit", "unknown", "drop"), filterCfg("tool-calling"))
	if got := fs.Apply(Request{EstimatedPromptTokens: 1 << 20}, entries); len(got) != 0 {
		t.Errorf("nothing fits a 1M-token prompt, kept %d", len(got))
	}
}

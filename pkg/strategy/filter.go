package strategy

import (
	"errors"
	"fmt"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// Verdict is a filter's decision about one candidate.
type Verdict int

const (
	// Keep: the candidate can serve the request.
	Keep Verdict = iota
	// Drop: the candidate cannot serve the request.
	Drop
	// Unknown: the filter lacks the metadata to decide. The filter's
	// "unknown" setting (keep|drop, default keep) settles it in Filters.keeps.
	Unknown
)

// Filter narrows the candidate list before a strategy ranks it.
type Filter interface {
	// Name returns the filter identifier used in config.
	Name() string
	// Judge decides whether e can serve req. It returns Unknown instead of
	// guessing when the entry lacks the metadata it needs.
	Judge(req Request, e catalog.CatalogEntry) Verdict
}

// ErrNoCandidates is returned when the filters leave no model that can serve
// the request. Retrying will not help, so it is not a 503.
var ErrNoCandidates = errors.New("no free model fits the request after filtering")

// filterConstructors maps config names to constructors. A constructor gets the
// filter's settings without "name" and "unknown", and must reject keys it does
// not know so a typo fails at startup.
var filterConstructors = map[string]func(settings map[string]any) (Filter, error){
	"context-fit":  newContextFitFilter,
	"tool-calling": newToolCallingFilter,
}

// configuredFilter is a built filter plus its policy for Unknown verdicts.
type configuredFilter struct {
	Filter
	dropUnknown bool
}

// Filters is a validated filter chain, built once per config load. The zero
// value filters nothing.
type Filters []configuredFilter

// BuildFilters validates the configured filters and builds them in order.
func BuildFilters(cfgs []config.FilterConfig) (Filters, error) {
	var out Filters
	for i, fc := range cfgs {
		ctor, ok := filterConstructors[fc.Name]
		if !ok {
			return nil, fmt.Errorf("proxy.filters[%d]: unknown filter %q", i, fc.Name)
		}
		settings := make(map[string]any, len(fc.Settings))
		dropUnknown := false
		for k, v := range fc.Settings {
			if k != "unknown" {
				settings[k] = v
				continue
			}
			switch v {
			case "keep":
			case "drop":
				dropUnknown = true
			default:
				return nil, fmt.Errorf("proxy.filters[%d] %s: unknown must be keep or drop, got %v", i, fc.Name, v)
			}
		}
		f, err := ctor(settings)
		if err != nil {
			return nil, fmt.Errorf("proxy.filters[%d] %s: %w", i, fc.Name, err)
		}
		out = append(out, configuredFilter{Filter: f, dropUnknown: dropUnknown})
	}
	return out, nil
}

// Keeps reports whether every filter lets e serve req. This is the one place
// an Unknown verdict becomes keep or drop.
func (fs Filters) Keeps(req Request, e catalog.CatalogEntry) bool {
	for _, f := range fs {
		switch f.Judge(req, e) {
		case Drop:
			return false
		case Unknown:
			if f.dropUnknown {
				return false
			}
		}
	}
	return true
}

// Apply returns the entries that pass every filter, in their original order.
// entries is never modified: the proxy shares one free-entry slice across all
// requests. When every entry passes, entries itself is returned (no
// allocation); a new slice is made only once a filter drops something.
// The result may be empty.
func (fs Filters) Apply(req Request, entries []catalog.CatalogEntry) []catalog.CatalogEntry {
	if len(fs) == 0 {
		return entries
	}
	var kept []catalog.CatalogEntry // nil until the first entry is dropped
	for i, e := range entries {
		if fs.Keeps(req, e) {
			if kept != nil {
				kept = append(kept, e)
			}
			continue
		}
		if kept == nil {
			kept = make([]catalog.CatalogEntry, i, len(entries))
			copy(kept, entries[:i])
		}
	}
	if kept == nil {
		return entries
	}
	return kept
}

// Names lists the filters for log and error messages.
func (fs Filters) Names() []string {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = f.Name()
	}
	return names
}

// contextFitFilter drops models whose context window cannot hold
// (prompt + max_tokens) * margin.
type contextFitFilter struct {
	margin float64
}

// newContextFitFilter: margin (number >= 1, default 1.2) is the safety factor
// on the estimated token count.
func newContextFitFilter(settings map[string]any) (Filter, error) {
	f := &contextFitFilter{margin: 1.2}
	for k, v := range settings {
		if k != "margin" {
			return nil, fmt.Errorf("unknown setting %q", k)
		}
		switch n := v.(type) {
		case float64:
			f.margin = n
		case int:
			f.margin = float64(n)
		default:
			return nil, fmt.Errorf("margin must be a number, got %v", v)
		}
		if f.margin < 1 {
			return nil, fmt.Errorf("margin must be at least 1, got %v", v)
		}
	}
	return f, nil
}

func (f *contextFitFilter) Name() string { return "context-fit" }

func (f *contextFitFilter) Judge(req Request, e catalog.CatalogEntry) Verdict {
	if e.ContextWindow <= 0 {
		return Unknown
	}
	required := float64(req.EstimatedPromptTokens+req.MaxTokens) * f.margin
	if required > float64(e.ContextWindow) {
		return Drop
	}
	return Keep
}

// toolCallingFilter drops models that cannot call tools when the request
// carries tools. It reads OpenRouter's supported_parameters; other providers
// do not report it yet, so their models are Unknown.
type toolCallingFilter struct{}

func newToolCallingFilter(settings map[string]any) (Filter, error) {
	for k := range settings {
		return nil, fmt.Errorf("unknown setting %q", k)
	}
	return toolCallingFilter{}, nil
}

func (toolCallingFilter) Name() string { return "tool-calling" }

func (toolCallingFilter) Judge(req Request, e catalog.CatalogEntry) Verdict {
	if !req.HasTools {
		return Keep
	}
	params, ok := supportedParameters(e)
	if !ok {
		return Unknown
	}
	for _, p := range params {
		if p == "tools" {
			return Keep
		}
	}
	return Drop // the provider listed its parameters and tools is not one
}

// supportedParameters reads the "supported_parameters" metadata: []string
// straight from a scan, []any once the catalog has been through JSON.
func supportedParameters(e catalog.CatalogEntry) ([]string, bool) {
	switch v := e.Metadata["supported_parameters"].(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, p := range v {
			if s, ok := p.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, false
}

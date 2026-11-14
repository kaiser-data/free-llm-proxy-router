// Package strategy implements the 13 model-selection strategies.
package strategy

import (
	"errors"
	"fmt"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
)

// Verdict represents the decision a filter makes about a candidate.
type Verdict int

const (
	// Keep means the candidate passes the filter.
	Keep Verdict = iota
	// Drop means the candidate is filtered out.
	Drop
	// Unknown means the filter cannot decide (missing metadata).
	Unknown
)

// Filter is implemented by all model filters.
// Filters narrow the candidate list BEFORE strategies rank it.
type Filter interface {
	// Name returns the filter identifier used in config.
	Name() string
	// Judge evaluates a catalog entry against the request.
	// Return Keep to include, Drop to exclude, Unknown if metadata is missing.
	Judge(req Request, e catalog.CatalogEntry) Verdict
}

// UnknownBehavior controls what happens when a filter returns Unknown.
type UnknownBehavior int

const (
	// UnknownKeep includes candidates when the filter cannot decide (default).
	UnknownKeep UnknownBehavior = iota
	// UnknownDrop excludes candidates when the filter cannot decide.
	UnknownDrop
)

// FilterConfig holds the configuration for a single filter.
type FilterConfig struct {
	Name      string                 `mapstructure:"name"`
	Settings  map[string]any         `mapstructure:"settings"`
	Unknown   string                 `mapstructure:"unknown"` // "keep" | "drop" (default: "keep")
}

// filterRegistry maps filter names to constructors.
type filterRegistry struct {
	constructors map[string]func(map[string]any) (Filter, error)
}

var defaultFilterRegistry = &filterRegistry{
	constructors: map[string]func(map[string]any) (Filter, error){
		"context-fit":      newContextFitFilter,
		"tool-calling":     newToolCallingFilter,
	},
}

// RegisterFilter adds a filter constructor to the registry.
func RegisterFilter(name string, constructor func(map[string]any) (Filter, error)) {
	defaultFilterRegistry.constructors[name] = constructor
}

// BuildFilters creates filter instances from their configurations.
func BuildFilters(configs []FilterConfig) ([]Filter, error) {
	var filters []Filter
	for _, fc := range configs {
		constructor, ok := defaultFilterRegistry.constructors[fc.Name]
		if !ok {
			return nil, fmt.Errorf("unknown filter: %q", fc.Name)
		}
		// Merge the unknown setting into settings for the constructor
		settings := make(map[string]any)
		for k, v := range fc.Settings {
			settings[k] = v
		}
		if fc.Unknown != "" {
			settings["unknown"] = fc.Unknown
		}
		f, err := constructor(settings)
		if err != nil {
			return nil, fmt.Errorf("building filter %q: %w", fc.Name, err)
		}
		filters = append(filters, f)
	}
	return filters, nil
}

// toolCallingFilter checks if the model supports tool calling.
type toolCallingFilter struct {
	unknownBehavior UnknownBehavior
}

// newToolCallingFilter creates a tool-calling filter.
// Settings: unknown (keep|drop, default keep) — behavior when capability metadata is missing.
func newToolCallingFilter(settings map[string]any) (Filter, error) {
	unknown := UnknownKeep
	if u, ok := settings["unknown"]; ok {
		switch v := u.(type) {
		case string:
			if v == "drop" {
				unknown = UnknownDrop
			}
		}
	}
	return &toolCallingFilter{unknownBehavior: unknown}, nil
}

func (f *toolCallingFilter) Name() string { return "tool-calling" }

func (f *toolCallingFilter) Judge(req Request, e catalog.CatalogEntry) Verdict {
	if !req.HasTools {
		return Keep // not a tools request, no filtering needed
	}

	// Check metadata for tool-calling capability
	// Expected metadata keys: "supports_tools", "has_tools", "tools", "function_calling"
	if meta, ok := e.Metadata["supports_tools"].(bool); ok {
		if meta {
			return Keep
		}
		return Drop
	}
	if meta, ok := e.Metadata["has_tools"].(bool); ok {
		if meta {
			return Keep
		}
		return Drop
	}
	if meta, ok := e.Metadata["tools"].(bool); ok {
		if meta {
			return Keep
		}
		return Drop
	}
	if meta, ok := e.Metadata["function_calling"].(bool); ok {
		if meta {
			return Keep
		}
		return Drop
	}

	// Check OpenRouter's supported_parameters for "tools" or "functions"
	if sp, ok := e.Metadata["supported_parameters"].([]any); ok {
		for _, p := range sp {
			if s, ok := p.(string); ok && (s == "tools" || s == "functions") {
				return Keep
			}
		}
	}
	if sp, ok := e.Metadata["supported_parameters"].([]string); ok {
		for _, p := range sp {
			if p == "tools" || p == "functions" {
				return Keep
			}
		}
	}

	// Unknown capability
	switch f.unknownBehavior {
	case UnknownDrop:
		return Drop
	default:
		return Unknown
	}
}

// ApplyFilters runs all filters on the candidate list.
// If all candidates are dropped, returns an error instead of an empty list.
func ApplyFilters(req Request, entries []catalog.CatalogEntry, filters []Filter) ([]catalog.CatalogEntry, error) {
	if len(filters) == 0 {
		return entries, nil
	}

	var kept []catalog.CatalogEntry
	for _, e := range entries {
		keep := true
		for _, f := range filters {
			verdict := f.Judge(req, e)
			switch verdict {
			case Drop:
				keep = false
				break
			case Unknown:
				// UnknownBehavior is encoded in the filter implementation.
				// For filters that support it (like tool-calling), the filter
				// itself returns Keep or Drop based on its configured behavior.
				// For filters that don't (like context-fit), Unknown is never returned.
				keep = false // conservative: treat Unknown as Drop
			case Keep:
				// continue to next filter
			}
			if !keep {
				break
			}
		}
		if keep {
			kept = append(kept, e)
		}
	}

	if len(kept) == 0 {
		return nil, ErrNoCandidates
	}
	return kept, nil
}

// ErrNoCandidates is returned when filters remove all candidates.
var ErrNoCandidates = errors.New("no free model fits the request after filtering")

// contextFitFilter checks if the model's context window can accommodate the request.
type contextFitFilter struct {
	margin float64
}

// newContextFitFilter creates a context-fit filter.
// Settings: margin (float64, default 1.2) — safety factor on estimated tokens.
func newContextFitFilter(settings map[string]any) (Filter, error) {
	margin := 1.2
	if m, ok := settings["margin"]; ok {
		switch v := m.(type) {
		case float64:
			margin = v
		case int:
			margin = float64(v)
		}
	}
	if margin < 1.0 {
		margin = 1.0
	}
	return &contextFitFilter{margin: margin}, nil
}

func (f *contextFitFilter) Name() string { return "context-fit" }

func (f *contextFitFilter) Judge(req Request, e catalog.CatalogEntry) Verdict {
	if e.ContextWindow <= 0 {
		return Unknown
	}
	required := int(float64(req.EstimatedPromptTokens+req.MaxTokens) * f.margin)
	if required > e.ContextWindow {
		return Drop
	}
	return Keep
}

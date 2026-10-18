// Package alias implements canonical model-name resolution for the proxy.
//
// Free-tier providers expose the same underlying model under different,
// provider-specific IDs ("qwen/qwen3-235b-a22b:free" on OpenRouter,
// "qwen3-32b" on Groq, ...). The alias layer groups those IDs under one
// canonical name ("qwen3") so clients can request a model without caring
// which provider serves it, while every raw upstream ID keeps working.
//
// Rules:
//   - Auto-groups are derived from the catalog by Canonicalize.
//   - Explicit config aliases ("models.aliases") replace auto-groups.
//   - Duplicate upstream IDs across providers stay separate candidates.
//   - Distinct families are never merged: qwen2 != qwen3 != qwen3-coder,
//     deepseek-r1 != deepseek-v3, gemini flash != flash-lite != pro.
package alias

import (
	"sort"
	"strings"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
)

// Upstream is one provider-specific model backing a canonical name.
type Upstream struct {
	ProviderID string
	ModelID    string
}

// Key uniquely identifies an upstream across providers.
func (u Upstream) Key() string { return u.ProviderID + "/" + u.ModelID }

// Group is a canonical name and the upstream models that can serve it.
type Group struct {
	Name      string
	Auto      bool // true when derived by Canonicalize, false when from config
	Upstreams []Upstream
}

// Options tunes resolver construction.
type Options struct {
	// FreeOnly restricts auto-grouping to entries flagged IsFree. Explicit
	// config aliases always apply regardless of this flag.
	FreeOnly bool
	// DisableAutoGroups turns off canonical family auto-derivation while
	// keeping config aliases and the pool alias active. Set when
	// canonicalization is disabled but a public pool alias is configured.
	DisableAutoGroups bool
	// PoolAlias, when set, creates one synthetic group mapping that name to
	// EVERY eligible free chat-capable catalog entry — the client-facing
	// "whole free pool" model (e.g. "kiwi-auto"). The pool is always
	// free-only and chat-capable regardless of FreeOnly/DisableAutoGroups,
	// and replaces any same-named auto or config-override group. Skipped
	// when the name is reserved (a raw model ID) or the catalog has no
	// eligible entries.
	PoolAlias string
	// Reserved canonical names are skipped (exact raw model IDs that must
	// not be shadowed by an alias). Keys are lowercase.
	Reserved map[string]bool
}

// Resolver maps canonical model names to provider-specific upstreams.
// It is immutable after construction and safe for concurrent use.
type Resolver struct {
	byName  map[string][]Upstream
	auto    map[string]bool
	names   []string
	byEntry map[Upstream]string // upstream -> canonical name
	pool    string              // global free-pool alias ("" when unconfigured)
}

// NewResolver builds the resolver from catalog entries and config overrides.
//
// Overrides map a canonical name to a list of upstream model IDs (matched by
// exact ModelID across all providers); an override replaces any auto-derived
// group with the same name entirely.
func NewResolver(entries []catalog.CatalogEntry, overrides map[string][]string, opts Options) *Resolver {
	r := &Resolver{
		byName:  map[string][]Upstream{},
		auto:    map[string]bool{},
		byEntry: map[Upstream]string{},
	}

	// Auto-groups derived from the catalog. Disabled when the caller only
	// wants explicit aliases and/or the pool alias (DisableAutoGroups).
	if !opts.DisableAutoGroups {
		seenAuto := map[Upstream]bool{}
		for _, e := range entries {
			if opts.FreeOnly && !e.IsFree {
				continue
			}
			name := Canonicalize(e.ModelID)
			if name == "" || opts.Reserved[name] {
				continue
			}
			u := Upstream{ProviderID: e.ProviderID, ModelID: e.ModelID}
			if !seenAuto[u] {
				seenAuto[u] = true
				r.byName[name] = append(r.byName[name], u)
				r.auto[name] = true
			}
		}
	}

	// Config overrides REPLACE auto-groups with the same name.
	for name, ids := range overrides {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || opts.Reserved[name] {
			continue
		}
		var ups []Upstream
		seen := map[Upstream]bool{}
		for _, id := range ids {
			id = strings.TrimSpace(id)
			for _, e := range entries {
				if e.ModelID == id {
					u := Upstream{ProviderID: e.ProviderID, ModelID: e.ModelID}
					if !seen[u] {
						seen[u] = true
						ups = append(ups, u)
					}
				}
			}
		}
		if len(ups) > 0 {
			r.byName[name] = ups
			r.auto[name] = false
		}
	}

	// Global free-pool alias: one public name over every eligible free
	// chat-capable catalog entry. The pool is always free-only (paid models
	// never enter, regardless of FreeOnly) and chat-capable (endpoint
	// metadata wins, name patterns fall back) so a normal chat request is
	// never routed to an embedding/image/speech model. It replaces any
	// same-named auto or override group — the pool alias is the public
	// abstraction sitting ABOVE the individual canonical families.
	if pool := strings.ToLower(strings.TrimSpace(opts.PoolAlias)); pool != "" && !opts.Reserved[pool] {
		var ups []Upstream
		seenPool := map[Upstream]bool{}
		for _, e := range entries {
			if !e.IsFree || !PoolChatCapable(e) {
				continue
			}
			u := Upstream{ProviderID: e.ProviderID, ModelID: e.ModelID}
			if !seenPool[u] {
				seenPool[u] = true
				ups = append(ups, u)
			}
		}
		if len(ups) > 0 {
			r.byName[pool] = ups
			delete(r.auto, pool)
			r.pool = pool
		}
	}

	// Index names (sorted) and the upstream -> canonical lookup. An upstream
	// reachable under several names keeps the first (sorted) one.
	for name, ups := range r.byName {
		if len(ups) == 0 {
			delete(r.byName, name)
			delete(r.auto, name)
		} else {
			r.names = append(r.names, name)
		}
	}
	sort.Strings(r.names)
	for _, name := range r.names {
		for _, u := range r.byName[name] {
			if _, ok := r.byEntry[u]; !ok {
				r.byEntry[u] = name
			}
		}
	}
	return r
}

// Canonicalize reduces a provider model ID to a canonical family name that
// is shared across providers, e.g. "qwen/qwen3-235b-a22b:free" and
// "qwen3-32b" both map to "qwen3".
//
// The rules are intentionally conservative — only namespace, qualifier,
// size/variant, and revision noise is stripped; distinct model families are
// never merged: qwen2 != qwen3 != qwen3-coder, deepseek-r1 != deepseek-v3,
// gemini-flash != gemini-flash-lite != gemini-pro.
func Canonicalize(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	if s == "" {
		return ""
	}
	// Strip vendor namespace: "meta-llama/llama-3.3-70b-instruct:free".
	if idx := strings.LastIndexByte(s, '/'); idx >= 0 {
		s = s[idx+1:]
	}
	// Strip qualifiers: ":free", ":extended", ...
	if idx := strings.IndexByte(s, ':'); idx >= 0 {
		s = s[:idx]
	}
	if s == "" {
		return ""
	}

	tokens := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' })
	if len(tokens) == 0 {
		return ""
	}

	family := tokens[0]
	rest := tokens[1:]
	// Merge a bare version token into the family: "llama-3.3" -> "llama3.3".
	if len(rest) > 0 && isVersionToken(rest[0]) && !strings.ContainsAny(family, "0123456789") {
		family += rest[0]
		rest = rest[1:]
	}

	parts := make([]string, 0, len(rest)+1)
	parts = append(parts, family)
	for _, tok := range rest {
		switch {
		case isVersionToken(tok), isParamToken(tok), isRevisionToken(tok), variantWords[tok]:
			// Size, variant, or revision noise — drop.
		default:
			parts = append(parts, tok)
		}
	}
	return strings.Join(parts, "-")
}

// variantWords are generic tuning/behavior qualifiers that do not
// distinguish model families.
var variantWords = map[string]bool{
	"instruct":  true,
	"versatile": true,
	"it":        true,
	"chat":      true,
	"base":      true,
	"preview":   true,
	"latest":    true,
}

// isVersionToken reports whether tok is a bare version ("3", "3.3", "2.5").
func isVersionToken(tok string) bool {
	if tok == "" {
		return false
	}
	for _, part := range strings.Split(tok, ".") {
		if !isDigits(part) {
			return false
		}
	}
	return true
}

// isParamToken reports whether tok is a size/parameter token: "70b", "32b",
// "120b", "a22b" (active params), "8x7b" (MoE experts).
func isParamToken(tok string) bool {
	if len(tok) < 2 {
		return false
	}
	if strings.HasPrefix(tok, "a") && strings.HasSuffix(tok, "b") && isDigits(tok[1:len(tok)-1]) {
		return true
	}
	if !strings.HasSuffix(tok, "b") {
		return false
	}
	rest := tok[:len(tok)-1]
	if idx := strings.IndexByte(rest, 'x'); idx >= 0 {
		return isDigits(rest[:idx]) && isDigits(rest[idx+1:])
	}
	return isDigits(rest)
}

// isRevisionToken reports whether tok is a dated revision ("0528", "0324", "001").
func isRevisionToken(tok string) bool {
	return isDigits(tok) && (len(tok) == 3 || len(tok) == 4)
}

// isDigits reports whether s consists only of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Names returns all canonical names, sorted.
func (r *Resolver) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.names...)
}

// Upstreams returns the upstream models backing a canonical name.
func (r *Resolver) Upstreams(name string) []Upstream {
	if r == nil {
		return nil
	}
	ups, ok := r.byName[strings.ToLower(name)]
	if !ok {
		return nil
	}
	return append([]Upstream(nil), ups...)
}

// IsCanonical reports whether name resolves to a canonical group.
func (r *Resolver) IsCanonical(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.byName[strings.ToLower(name)]
	return ok
}

// IsAuto reports whether name is an auto-derived (not config-overridden) group.
func (r *Resolver) IsAuto(name string) bool {
	if r == nil {
		return false
	}
	return r.auto[strings.ToLower(name)]
}

// PoolAlias returns the configured global free-pool alias, or "" when the
// resolver has no pool alias (unconfigured, reserved-name collision, or no
// eligible free candidates in the catalog).
func (r *Resolver) PoolAlias() string {
	if r == nil {
		return ""
	}
	return r.pool
}

// IsPool reports whether name is the global free-pool alias
// (case-insensitive, trimmed).
func (r *Resolver) IsPool(name string) bool {
	if r == nil || r.pool == "" {
		return false
	}
	return strings.ToLower(strings.TrimSpace(name)) == r.pool
}

// CanonicalName returns the canonical form of name if it is canonical, else "".
func (r *Resolver) CanonicalName(name string) string {
	if r == nil {
		return ""
	}
	name = strings.ToLower(name)
	if _, ok := r.byName[name]; ok {
		return name
	}
	return ""
}

// CanonicalFor returns the canonical name backing a specific upstream, or "".
func (r *Resolver) CanonicalFor(providerID, modelID string) string {
	if r == nil {
		return ""
	}
	return r.byEntry[Upstream{ProviderID: providerID, ModelID: modelID}]
}

// Candidates returns the catalog entries backing a canonical name, in
// resolver order (auto-groups follow catalog scan order).
func (r *Resolver) Candidates(cat *catalog.Catalog, name string) []catalog.CatalogEntry {
	if r == nil || cat == nil {
		return nil
	}
	var out []catalog.CatalogEntry
	for _, u := range r.Upstreams(name) {
		if e := cat.Find(u.ProviderID, u.ModelID); e != nil {
			out = append(out, *e)
		}
	}
	return out
}

// PoolChatCapable reports whether a catalog entry can serve OpenAI-compatible
// /chat/completions requests — the eligibility filter for the global free
// pool alias. Decisions come only from information the scanners already
// recorded; nothing is invented:
//
//  1. supported_endpoint_types metadata wins when present — the list must
//     mark a chat endpoint ("openai"/"chat"/"completions") and must not
//     mark a non-chat one ("embedding", "image-generation", "tts", …);
//  2. without endpoint metadata the model ID decides via the same
//     conservative name patterns the scanners apply at discovery time.
//
// An entry with no metadata and a neutral name is assumed chat-capable:
// the scanners only admit such models after their own chat-capability
// checks, so the pool keeps them.
func PoolChatCapable(e catalog.CatalogEntry) bool {
	if types := endpointTypes(e.Metadata); len(types) > 0 {
		chat := false
		for _, t := range types {
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "embedding", "embed", "rerank", "image", "image-generation",
				"audio", "speech", "tts", "stt", "asr", "aihorde", "midjourney":
				return false // explicitly non-chat endpoint
			case "openai", "chat", "completions", "chat/completions":
				chat = true
			}
		}
		return chat // endpoint list present but marks no chat endpoint → out
	}
	return chatCapableByName(e.ModelID)
}

// endpointTypes extracts the supported_endpoint_types list from entry
// metadata. JSON decoding produces []any; programmatic metadata may hold
// []string — both are accepted.
func endpointTypes(meta map[string]any) []string {
	if meta == nil {
		return nil
	}
	switch v := meta["supported_endpoint_types"].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// chatCapableByName applies the scanner name filter (the pkg/scan
// isEligibleModel patterns) to entries without endpoint metadata.
func chatCapableByName(modelID string) bool {
	lower := strings.ToLower(modelID)
	for _, skip := range []string{"embed", "rerank", "image", "vision-only", "tts", "stt", "whisper"} {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	return true
}

// RawRef is a raw escape hatch parsed from a request model field.
type RawRef struct {
	ProviderID string // may be empty — any provider
	ModelID    string
}

// ParseRawModel parses the "raw:" escape hatch syntax:
//
//	raw:<model-id>            exact model ID, any provider
//	raw:<provider>:<model-id> exact model ID on one provider
//
// The provider segment is detected by slug check because model IDs contain
// "/" and ":" (e.g. "raw:openrouter:qwen/qwen3-235b-a22b:free").
func ParseRawModel(model string) (RawRef, bool) {
	rest, ok := strings.CutPrefix(model, "raw:")
	if !ok {
		return RawRef{}, false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return RawRef{}, false
	}
	// "provider:model" only when the first segment is a bare provider slug
	// (no "/", no ":"); otherwise the whole rest is the model ID.
	if idx := strings.IndexByte(rest, ':'); idx > 0 {
		first := rest[:idx]
		if !strings.ContainsAny(first, "/:") && isProviderSlug(first) {
			return RawRef{ProviderID: first, ModelID: rest[idx+1:]}, true
		}
	}
	return RawRef{ModelID: rest}, true
}

// isProviderSlug reports whether s looks like a provider ID: lowercase
// letters, digits, dashes, and underscores only.
func isProviderSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

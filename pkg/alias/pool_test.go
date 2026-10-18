package alias

import (
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
)

// unoPoolEntries is a representative slice of the live UnoRouter free
// catalog: free chat models across several families, one paid model, one
// embedding model flagged by endpoint metadata, and one speech model caught
// only by its name.
func unoPoolEntries() []catalog.CatalogEntry {
	return []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true},
		{ProviderID: "unorouter", ModelID: "deepseek/deepseek-v3:free", IsFree: true},
		{ProviderID: "unorouter", ModelID: "gemma-3-27b-it:free", IsFree: true},
		{ProviderID: "unorouter", ModelID: "mimo-v2.6-flash:free", IsFree: true},
		// Paid — must never enter the pool.
		{ProviderID: "unorouter", ModelID: "gpt-5", IsFree: false},
		// Free but embedding-only by endpoint metadata — must never enter.
		{ProviderID: "unorouter", ModelID: "bge-m3:free", IsFree: true,
			Metadata: map[string]any{"supported_endpoint_types": []any{"embedding"}}},
		// Free but speech by name (no metadata) — must never enter.
		{ProviderID: "unorouter", ModelID: "whisper-large-v3:free", IsFree: true},
	}
}

// TestPoolAliasGroupsAllFreeChatModels: the pool alias maps to every
// eligible free chat-capable entry — not one family — and excludes paid and
// non-chat models.
func TestPoolAliasGroupsAllFreeChatModels(t *testing.T) {
	res := NewResolver(unoPoolEntries(), nil, Options{PoolAlias: "kiwi-auto", DisableAutoGroups: true})

	if !res.IsPool("kiwi-auto") || !res.IsPool(" KIWI-Auto ") {
		t.Error("expected kiwi-auto to be the pool alias (case-insensitive, trimmed)")
	}
	if got := res.PoolAlias(); got != "kiwi-auto" {
		t.Errorf("PoolAlias() = %q, want kiwi-auto", got)
	}

	ups := res.Upstreams("kiwi-auto")
	want := []string{
		"unorouter/qwen/qwen3-235b-a22b:free",
		"unorouter/deepseek/deepseek-v3:free",
		"unorouter/gemma-3-27b-it:free",
		"unorouter/mimo-v2.6-flash:free",
	}
	if len(ups) != len(want) {
		t.Fatalf("Upstreams(kiwi-auto) = %v, want %d candidates", ups, len(want))
	}
	keys := map[string]bool{}
	for _, u := range ups {
		keys[u.Key()] = true
	}
	for _, k := range want {
		if !keys[k] {
			t.Errorf("pool missing %q; got %v", k, keys)
		}
	}
	for _, banned := range []string{"unorouter/gpt-5", "unorouter/bge-m3:free", "unorouter/whisper-large-v3:free"} {
		if keys[banned] {
			t.Errorf("%q must not enter the free pool", banned)
		}
	}
}

// TestPoolAliasCoexistsWithCanonicalGroups: the pool sits ABOVE the family
// groups — families stay resolvable and the pool still holds every member.
func TestPoolAliasCoexistsWithCanonicalGroups(t *testing.T) {
	res := NewResolver(unoPoolEntries(), nil, Options{FreeOnly: true, PoolAlias: "kiwi-auto"})

	if ups := res.Upstreams("qwen3"); len(ups) != 1 {
		t.Errorf("Upstreams(qwen3) = %v, want the qwen3 family group", ups)
	}
	if ups := res.Upstreams("deepseek-v3"); len(ups) != 1 {
		t.Errorf("Upstreams(deepseek-v3) = %v, want the deepseek-v3 family group", ups)
	}
	// Pool holds every free chat model across all families.
	if ups := res.Upstreams("kiwi-auto"); len(ups) != 4 {
		t.Errorf("Upstreams(kiwi-auto) = %d candidates, want 4", len(ups))
	}
	if res.IsAuto("kiwi-auto") {
		t.Error("pool alias must not be marked as an auto-derived family group")
	}
}

// TestPoolAliasReplacesSameNamedGroup: an explicit public pool alias wins
// over an alias/override group with the same name.
func TestPoolAliasReplacesSameNamedGroup(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true},
		{ProviderID: "unorouter", ModelID: "deepseek/deepseek-v3:free", IsFree: true},
	}
	res := NewResolver(entries, map[string][]string{"kiwi-auto": {"qwen/qwen3-235b-a22b:free"}},
		Options{PoolAlias: "kiwi-auto"})
	if ups := res.Upstreams("kiwi-auto"); len(ups) != 2 {
		t.Errorf("pool alias must replace the same-named override group, got %v", ups)
	}
}

// TestPoolAliasReservedCollision: a pool alias that exactly matches a raw
// model ID is skipped — the alias layer never shadows raw IDs. (Production
// computes the Reserved map from the catalog in buildAliasResolver; the test
// mirrors that by passing it explicitly.)
func TestPoolAliasReservedCollision(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "kiwi-auto", IsFree: true},
		{ProviderID: "unorouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true},
	}
	res := NewResolver(entries, nil, Options{
		PoolAlias: "kiwi-auto",
		Reserved:  map[string]bool{"kiwi-auto": true},
	})
	if res.IsPool("kiwi-auto") || res.PoolAlias() != "" {
		t.Error("pool alias colliding with a raw model ID must be skipped")
	}
	if res.IsCanonical("kiwi-auto") {
		t.Error("reserved name must not become a group")
	}
}

// TestDisableAutoGroupsKeepsPoolAndOverrides: canonicalization disabled but
// a pool alias configured — families are not derived, config overrides and
// the pool stay active.
func TestDisableAutoGroupsKeepsPoolAndOverrides(t *testing.T) {
	res := NewResolver(unoPoolEntries(), map[string][]string{"pinned": {"qwen/qwen3-235b-a22b:free"}},
		Options{PoolAlias: "kiwi-auto", DisableAutoGroups: true})

	if res.IsCanonical("qwen3") {
		t.Error("auto family groups must be disabled")
	}
	if ups := res.Upstreams("pinned"); len(ups) != 1 {
		t.Errorf("config overrides must stay active, got %v", ups)
	}
	if ups := res.Upstreams("kiwi-auto"); len(ups) != 4 {
		t.Errorf("pool must stay active, got %d candidates", len(ups))
	}
}

// TestPoolAliasDynamicRebuild: rebuilding the resolver with a changed
// catalog grows and shrinks the pool — the dynamic-refresh contract.
func TestPoolAliasDynamicRebuild(t *testing.T) {
	base := []catalog.CatalogEntry{
		{ProviderID: "unorouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true},
	}
	res := NewResolver(base, nil, Options{PoolAlias: "kiwi-auto"})
	if ups := res.Upstreams("kiwi-auto"); len(ups) != 1 {
		t.Fatalf("initial pool = %v, want 1 candidate", ups)
	}

	grown := append(append([]catalog.CatalogEntry{}, base...),
		catalog.CatalogEntry{ProviderID: "unorouter", ModelID: "deepseek/deepseek-v3:free", IsFree: true})
	res2 := NewResolver(grown, nil, Options{PoolAlias: "kiwi-auto"})
	if ups := res2.Upstreams("kiwi-auto"); len(ups) != 2 {
		t.Errorf("pool after discovery = %d candidates, want 2", len(ups))
	}

	shrunk := []catalog.CatalogEntry{base[0],
		{ProviderID: "unorouter", ModelID: "deepseek/deepseek-v3:free", IsFree: false}}
	res3 := NewResolver(shrunk, nil, Options{PoolAlias: "kiwi-auto"})
	if ups := res3.Upstreams("kiwi-auto"); len(ups) != 1 {
		t.Errorf("pool after removal = %d candidates, want 1", len(ups))
	}
}

// TestPoolAliasEmptyCatalog: no eligible entries — no pool group at all, so
// callers fall back to their non-pool behavior.
func TestPoolAliasEmptyCatalog(t *testing.T) {
	res := NewResolver(nil, nil, Options{PoolAlias: "kiwi-auto"})
	if res.IsPool("kiwi-auto") || res.PoolAlias() != "" {
		t.Error("empty catalog must not create a pool group")
	}
	if names := res.Names(); len(names) != 0 {
		t.Errorf("Names() = %v, want empty", names)
	}
}

// TestPoolChatCapableMetadataVariants pins the eligibility decision table:
// metadata form ([]any vs []string), chat markers, non-chat markers, and the
// name-pattern fallback.
func TestPoolChatCapableMetadataVariants(t *testing.T) {
	cases := []struct {
		name    string
		entry   catalog.CatalogEntry
		capable bool
	}{
		{"any-of-openai", catalog.CatalogEntry{ModelID: "m", Metadata: map[string]any{"supported_endpoint_types": []any{"openai"}}}, true},
		{"chat-marker", catalog.CatalogEntry{ModelID: "m", Metadata: map[string]any{"supported_endpoint_types": []string{"chat"}}}, true},
		{"completions-marker", catalog.CatalogEntry{ModelID: "m", Metadata: map[string]any{"supported_endpoint_types": []any{"completions"}}}, true},
		{"embedding-out", catalog.CatalogEntry{ModelID: "m", Metadata: map[string]any{"supported_endpoint_types": []any{"openai", "embedding"}}}, false},
		{"image-out", catalog.CatalogEntry{ModelID: "m", Metadata: map[string]any{"supported_endpoint_types": []any{"image-generation"}}}, false},
		{"aihorde-out", catalog.CatalogEntry{ModelID: "m", Metadata: map[string]any{"supported_endpoint_types": []any{"aihorde"}}}, false},
		{"no-chat-marker-out", catalog.CatalogEntry{ModelID: "m", Metadata: map[string]any{"supported_endpoint_types": []any{"rerank"}}}, false},
		{"no-metadata-name-fallback", catalog.CatalogEntry{ModelID: "glm-5.3-flash:free"}, true},
		{"name-embed-out", catalog.CatalogEntry{ModelID: "text-embedding-3:free"}, false},
		{"name-tts-out", catalog.CatalogEntry{ModelID: "tts-1:free"}, false},
		{"name-rerank-out", catalog.CatalogEntry{ModelID: "bge-reranker-v2:free"}, false},
		{"nil-metadata", catalog.CatalogEntry{ModelID: "qwen3:free"}, true},
	}
	for _, c := range cases {
		if got := PoolChatCapable(c.entry); got != c.capable {
			t.Errorf("%s: PoolChatCapable = %v, want %v", c.name, got, c.capable)
		}
	}
}

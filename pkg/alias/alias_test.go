package alias

import (
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
)

func TestCanonicalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"qwen/qwen3-235b-a22b:free", "qwen3"},
		{"Qwen/Qwen3-235B-A22B:free", "qwen3"},
		{"qwen3-32b", "qwen3"},
		{"qwen3", "qwen3"},
		{"qwen3-coder-480b-a35b-instruct", "qwen3-coder"},
		{"qwen2.5-72b-instruct", "qwen2.5"},
		{"qwen-2.5-72b", "qwen2.5"}, // merges with qwen2.5-72b-instruct
		{"qwen2", "qwen2"},          // never merged with qwen3
		{"deepseek-r1-0528:free", "deepseek-r1"},
		{"deepseek-r1", "deepseek-r1"},
		{"deepseek-v3-0324:free", "deepseek-v3"},
		{"deepseek-r1-distill-llama-8b", "deepseek-r1-distill-llama"},
		{"llama-3.3-70b-versatile", "llama3.3"},
		{"meta-llama/llama-3.3-70b-instruct:free", "llama3.3"},
		{"gpt-oss-120b", "gpt-oss"},
		{"gpt-oss-20b", "gpt-oss"},
		{"mixtral-8x7b-instruct", "mixtral"},
		{"gemini-2.0-flash-lite-001", "gemini2.0-flash-lite"},
		{"gemini-2.5-flash", "gemini2.5-flash"},
		{"gemini-2.5-pro", "gemini2.5-pro"}, // distinct from flash
		{"kimi-k2-instruct", "kimi-k2"},
		{"glm-4.6", "glm4.6"},
		{"gemma-3-27b-it", "gemma3"},
		{"llama-4-scout-17b-16e-instruct", "llama4-scout-16e"},
		{"", ""},
		{":free", ""},
	}
	for _, c := range cases {
		if got := Canonicalize(c.in); got != c.want {
			t.Errorf("Canonicalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolverAutoGroups(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "openrouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true},
		{ProviderID: "groq", ModelID: "qwen3-32b", IsFree: true},
		{ProviderID: "openrouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true}, // duplicate scan row
		{ProviderID: "gemini", ModelID: "gemini-2.0-flash-lite-001", IsFree: true},
		{ProviderID: "openrouter", ModelID: "some-paid-model", IsFree: false},
	}
	res := NewResolver(entries, nil, Options{FreeOnly: true})

	if !res.IsCanonical("qwen3") || !res.IsCanonical("Qwen3") {
		t.Error("expected qwen3 to be canonical")
	}
	if got := res.CanonicalName("Qwen3"); got != "qwen3" {
		t.Errorf("CanonicalName(Qwen3) = %q, want qwen3", got)
	}
	// Duplicate scan rows dedup to one upstream; two providers stay separate.
	if ups := res.Upstreams("qwen3"); len(ups) != 2 {
		t.Errorf("Upstreams(qwen3) = %v, want 2 distinct upstreams", ups)
	}
	if res.IsCanonical("some-paid-model") {
		t.Error("paid model must not be grouped when FreeOnly is set")
	}
	if got := res.CanonicalFor("groq", "qwen3-32b"); got != "qwen3" {
		t.Errorf("CanonicalFor(groq, qwen3-32b) = %q, want qwen3", got)
	}
	if got := res.CanonicalFor("openrouter", "some-paid-model"); got != "" {
		t.Errorf("CanonicalFor paid model = %q, want empty", got)
	}
	if !res.IsAuto("qwen3") {
		t.Error("expected auto-derived group")
	}
	// Candidates must resolve back to catalog entries.
	cands := res.Candidates(&catalog.Catalog{Entries: entries}, "qwen3")
	if len(cands) != 2 {
		t.Errorf("Candidates(qwen3) = %d entries, want 2", len(cands))
	}
}

func TestResolverDuplicateModelAcrossProviders(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "alpha", ModelID: "mixtral-8x7b-instruct", IsFree: true},
		{ProviderID: "beta", ModelID: "mixtral-8x7b-instruct", IsFree: true},
	}
	res := NewResolver(entries, nil, Options{})
	ups := res.Upstreams("mixtral")
	if len(ups) != 2 || ups[0].ProviderID == ups[1].ProviderID {
		t.Errorf("duplicate upstream IDs across providers must stay separate candidates, got %v", ups)
	}
}

func TestResolverOverrides(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "groq", ModelID: "qwen3-32b", IsFree: true},
		{ProviderID: "cerebras", ModelID: "qwen3-32b", IsFree: true},
		{ProviderID: "openrouter", ModelID: "qwen/qwen3-235b-a22b:free", IsFree: true},
		{ProviderID: "openrouter", ModelID: "custom/pinned-model:free", IsFree: true},
	}
	// An override replaces the auto group entirely.
	res := NewResolver(entries, map[string][]string{"qwen3": {"custom/pinned-model:free"}}, Options{})
	ups := res.Upstreams("qwen3")
	if len(ups) != 1 || ups[0].ModelID != "custom/pinned-model:free" {
		t.Errorf("override must replace auto group, got %v", ups)
	}
	if res.IsAuto("qwen3") {
		t.Error("overridden group must not be marked auto")
	}
	// An override with no matching entries leaves the auto group intact.
	res2 := NewResolver(entries, map[string][]string{"qwen3": {"does/not-exist"}}, Options{})
	if ups := res2.Upstreams("qwen3"); len(ups) != 3 {
		t.Errorf("empty override must fall back to auto group, got %d upstreams", len(ups))
	}
}

func TestResolverReservedNames(t *testing.T) {
	entries := []catalog.CatalogEntry{{ProviderID: "groq", ModelID: "qwen3-32b", IsFree: true}}
	res := NewResolver(entries, map[string][]string{"qwen3": {"qwen3-32b"}}, Options{Reserved: map[string]bool{"qwen3": true}})
	if res.IsCanonical("qwen3") {
		t.Error("reserved name must not become canonical")
	}
}

func TestNilResolverSafety(t *testing.T) {
	var res *Resolver
	if res.IsCanonical("qwen3") || res.IsAuto("qwen3") {
		t.Error("nil resolver must report nothing canonical")
	}
	if got := res.CanonicalName("qwen3"); got != "" {
		t.Errorf("nil resolver CanonicalName = %q, want empty", got)
	}
	if got := res.CanonicalFor("groq", "qwen3-32b"); got != "" {
		t.Errorf("nil resolver CanonicalFor = %q, want empty", got)
	}
	if ups := res.Upstreams("qwen3"); ups != nil {
		t.Errorf("nil resolver Upstreams = %v, want nil", ups)
	}
	if names := res.Names(); names != nil {
		t.Errorf("nil resolver Names = %v, want nil", names)
	}
	if cands := res.Candidates(nil, "qwen3"); cands != nil {
		t.Errorf("nil resolver Candidates = %v, want nil", cands)
	}
}

func TestParseRawModel(t *testing.T) {
	cases := []struct {
		in      string
		want    RawRef
		wantRaw bool
	}{
		{"qwen3", RawRef{}, false},
		{"raw:qwen3-32b", RawRef{ModelID: "qwen3-32b"}, true},
		{"raw:openrouter:qwen/qwen3-235b-a22b:free", RawRef{ProviderID: "openrouter", ModelID: "qwen/qwen3-235b-a22b:free"}, true},
		{"raw:qwen/qwen3-235b-a22b:free", RawRef{ModelID: "qwen/qwen3-235b-a22b:free"}, true},
		{"raw:", RawRef{}, false},
		{"raw:  ", RawRef{}, false},
	}
	for _, c := range cases {
		got, ok := ParseRawModel(c.in)
		if ok != c.wantRaw {
			t.Errorf("ParseRawModel(%q) ok = %v, want %v", c.in, ok, c.wantRaw)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParseRawModel(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

// TestCanonicalizeUnoRouterIDs checks UnoRouter free-tier IDs canonicalize
// through the same shared family layer as the other providers — vendor
// namespaces and the ":free" suffix are stripped, paid-suffix-free families
// keep their identity.
func TestCanonicalizeUnoRouterIDs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"qwen/qwen3-235b-a22b:free", "qwen3"},
		{"qwen/qwen3-30b-a3b:free", "qwen3"},
		{"space-bunny-alpha:free", "space-bunny-alpha"},
		{"k2-horizon:free", "k2-horizon"},
		{"deepseek/deepseek-r1:free", "deepseek-r1"},
		{"mimo-v2.6-pro:free", "mimo-v2.6-pro"},
		{"glm-5.3-flash:free", "glm5.3-flash"},
	}
	for _, c := range cases {
		if got := Canonicalize(c.in); got != c.want {
			t.Errorf("Canonicalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

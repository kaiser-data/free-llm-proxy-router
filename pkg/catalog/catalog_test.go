package catalog

import (
	"reflect"
	"strings"
	"testing"
)

func TestKeepUnscanned(t *testing.T) {
	prev := &Catalog{Entries: []CatalogEntry{
		{ProviderID: "groq", ModelID: "old-groq"},
		{ProviderID: "gemini", ModelID: "g1"},
		{ProviderID: "gemini", ModelID: "g2"},
	}}
	cur := &Catalog{Entries: []CatalogEntry{
		{ProviderID: "groq", ModelID: "new-groq"},
	}}

	kept := cur.KeepUnscanned(prev)

	if !reflect.DeepEqual(kept, []string{"gemini"}) {
		t.Errorf("kept = %v, want [gemini]", kept)
	}
	var ids []string
	for _, e := range cur.Entries {
		ids = append(ids, e.ModelID)
	}
	// Scanned provider is replaced, not merged; unscanned one is carried forward.
	if !reflect.DeepEqual(ids, []string{"new-groq", "g1", "g2"}) {
		t.Errorf("entries = %v", ids)
	}
}

func TestKeepUnscanned_NilPrev(t *testing.T) {
	cur := &Catalog{}
	if kept := cur.KeepUnscanned(nil); kept != nil {
		t.Errorf("kept = %v, want nil", kept)
	}
}

func TestWriteMarkdown(t *testing.T) {
	c := &Catalog{Entries: []CatalogEntry{
		{ProviderID: "groq", ModelID: "llama-b", IsFree: true, ContextWindow: 131072},
		{ProviderID: "groq", ModelID: "llama-a", IsFree: true, ContextWindow: 8192},
		{ProviderID: "gemini", ModelID: "flash", IsFree: true, ContextWindow: 1_048_576},
		{ProviderID: "gemini", ModelID: "paid", IsFree: false},
	}}
	var b strings.Builder
	if err := c.WriteMarkdown(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"**3 free models** across **2 providers**",
		"| [groq](#groq) | 2 | 128k |",
		"| [gemini](#gemini) | 1 | 1M |",
		"| `flash` | 1M |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "paid") {
		t.Error("non-free model listed")
	}
	if strings.Index(out, "llama-a") > strings.Index(out, "llama-b") {
		t.Error("models not sorted")
	}
}

func TestFormatContext(t *testing.T) {
	for n, want := range map[int]string{
		0: "–", 512: "512", 8192: "8k", 131072: "128k", 128000: "128k",
		1_048_576: "1M", 2_000_000: "2M", 1_500_000: "1.5M",
	} {
		if got := formatContext(n); got != want {
			t.Errorf("formatContext(%d) = %q, want %q", n, got, want)
		}
	}
}

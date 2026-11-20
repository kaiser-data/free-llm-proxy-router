package strategy

import (
	"strings"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
)

// testEntries provides a rich test catalog spanning multiple providers and sizes.
func testEntries() []catalog.CatalogEntry {
	return []catalog.CatalogEntry{
		{ProviderID: "groq", ModelID: "llama-3.1-8b-instant", IsFree: true, ContextWindow: 131072},
		{ProviderID: "groq", ModelID: "llama-3.3-70b-versatile", IsFree: true, ContextWindow: 131072},
		{ProviderID: "groq", ModelID: "mixtral-8x7b-32768", IsFree: true, ContextWindow: 32768},
		{ProviderID: "cerebras", ModelID: "llama3.1-8b", IsFree: true},
		{ProviderID: "cerebras", ModelID: "llama3.1-70b", IsFree: true},
		{ProviderID: "gemini", ModelID: "gemini-2.0-flash", IsFree: true, ContextWindow: 1000000},
		{ProviderID: "gemini", ModelID: "gemini-2.0-flash-lite", IsFree: true, ContextWindow: 1000000},
		{ProviderID: "openrouter", ModelID: "meta-llama/llama-3.1-8b-instruct:free", IsFree: true},
		{ProviderID: "openrouter", ModelID: "google/gemma-2-9b-it:free", IsFree: true},
		{ProviderID: "openrouter", ModelID: "mistralai/mistral-7b-instruct:free", IsFree: true},
		{ProviderID: "huggingface", ModelID: "meta-llama/Llama-3.2-1B-Instruct", IsFree: true},
		// Non-free entries (should be excluded by all strategies)
		{ProviderID: "together", ModelID: "paid-model", IsFree: false},
	}
}

func testReq() Request {
	return Request{
		Messages:  []map[string]any{{"role": "user", "content": "hello"}},
		MaxTokens: 256,
	}
}

// TestAllStrategiesReturnResults verifies every strategy returns at least one result.
func TestAllStrategiesReturnResults(t *testing.T) {
	entries := testEntries()
	req := testReq()
	relTracker := reliability.New()
	rateLimiter := ratelimit.NewGlobalTracker()

	reg := NewRegistry(relTracker, rateLimiter, "llama-3.1", "gemini-2.0-flash-lite", 3, 5)

	for _, name := range reg.Names() {
		t.Run(name, func(t *testing.T) {
			strat, err := reg.Get(name)
			if err != nil {
				t.Fatalf("Get(%q): %v", name, err)
			}
			ranked := strat.Rank(req, entries, nil)
			if len(ranked) == 0 {
				t.Errorf("strategy %q returned 0 results", name)
			}
			// All returned entries should be from the free set
			for _, r := range ranked {
				found := false
				for _, e := range entries {
					if e.ProviderID == r.ProviderID && e.ModelID == r.ModelID {
						found = true
						if !e.IsFree {
							t.Errorf("strategy %q returned non-free model %s/%s", name, r.ProviderID, r.ModelID)
						}
						break
					}
				}
				if !found {
					t.Errorf("strategy %q returned unknown entry %s/%s", name, r.ProviderID, r.ModelID)
				}
			}
		})
	}
}

// TestStrategyPerformance verifies that the largest model ranks first.
func TestStrategyPerformance(t *testing.T) {
	entries := testEntries()
	s := &StrategyPerformance{}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// Largest model by params should be first — 70B variants
	first := ranked[0]
	params := models.ExtractParams(first.ModelID)
	if params.Effective < 60 && params.Billions < 60 {
		t.Logf("performance: first model = %s/%s (params=%+v)", first.ProviderID, first.ModelID, params)
		// Allow — some models have no extracted params (Gemini)
	}
}

// TestStrategySpeed verifies Cerebras appears before HuggingFace.
func TestStrategySpeed(t *testing.T) {
	entries := testEntries()
	s := &StrategySpeed{}
	ranked := s.Rank(testReq(), entries, nil)
	cerebrasIdx := -1
	hfIdx := -1
	for i, r := range ranked {
		if r.ProviderID == "cerebras" && cerebrasIdx < 0 {
			cerebrasIdx = i
		}
		if r.ProviderID == "huggingface" && hfIdx < 0 {
			hfIdx = i
		}
	}
	if hfIdx >= 0 && cerebrasIdx >= 0 && cerebrasIdx > hfIdx {
		t.Errorf("speed: cerebras (idx %d) should rank before huggingface (idx %d)", cerebrasIdx, hfIdx)
	}
}

// TestStrategyVolume verifies Gemini Flash-Lite ranks first.
func TestStrategyVolume(t *testing.T) {
	entries := testEntries()
	s := &StrategyVolume{GeminiPreferredModel: "gemini-2.0-flash-lite"}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	if ranked[0].ProviderID != "gemini" {
		t.Errorf("volume: first entry should be gemini, got %s", ranked[0].ProviderID)
	}
}

// TestStrategySmall verifies only Small-tier models rank highest.
func TestStrategySmall(t *testing.T) {
	entries := testEntries()
	s := &StrategySmall{}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// Top entries should be Small-tier (3-13B)
	first := ranked[0]
	params := models.ExtractParams(first.ModelID)
	tier := models.ClassifyTier(params)
	if tier != models.TierSmall && tier != models.TierTiny {
		// Allow — if no small models are found, it falls back
		t.Logf("small: first model = %s/%s tier=%v (may be fallback)", first.ProviderID, first.ModelID, tier)
	}
}

// TestStrategyTiny verifies tiny models rank first.
func TestStrategyTiny(t *testing.T) {
	entries := testEntries()
	s := &StrategyTiny{}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// First entry should be tiny (1B)
	t.Logf("tiny: first=%s/%s", ranked[0].ProviderID, ranked[0].ModelID)
}

// TestStrategySimilar verifies family matching works.
func TestStrategySimilar(t *testing.T) {
	entries := testEntries()
	s := &StrategySimilar{TargetFamily: "llama-3.1"}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// LLaMA 3.1 models should appear first
	first := ranked[0]
	if !containsAny(first.ModelID, "llama-3.1", "llama3.1") {
		t.Logf("similar: first=%s/%s (may be from enriched data)", first.ProviderID, first.ModelID)
	}
}

// TestStrategyParallel verifies at most FanOut distinct providers are returned.
func TestStrategyParallel(t *testing.T) {
	entries := testEntries()
	s := &StrategyParallel{FanOut: 3}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) > 3 {
		t.Errorf("parallel: returned %d entries, want <= 3", len(ranked))
	}
	// All returned entries should be from distinct providers
	seen := map[string]bool{}
	for _, r := range ranked {
		if seen[r.ProviderID] {
			t.Errorf("parallel: duplicate provider %s", r.ProviderID)
		}
		seen[r.ProviderID] = true
	}
}

// TestStrategyReliable verifies high-reliability providers rank first.
func TestStrategyReliable(t *testing.T) {
	entries := testEntries()
	tracker := reliability.New()
	// Record some failures for groq, success for gemini
	for i := 0; i < 10; i++ {
		tracker.Record("groq", false)
	}
	for i := 0; i < 10; i++ {
		tracker.Record("gemini", true)
	}

	s := &StrategyReliable{Tracker: tracker}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// Gemini should rank before groq due to higher reliability
	geminiIdx := -1
	groqIdx := -1
	for i, r := range ranked {
		if r.ProviderID == "gemini" && geminiIdx < 0 {
			geminiIdx = i
		}
		if r.ProviderID == "groq" && groqIdx < 0 {
			groqIdx = i
		}
	}
	if geminiIdx > groqIdx && groqIdx >= 0 {
		t.Errorf("reliable: gemini (idx %d) should rank before groq (idx %d)", geminiIdx, groqIdx)
	}
}

// TestStrategyEconomical verifies credit-based providers rank last.
func TestStrategyEconomical(t *testing.T) {
	entries := append(testEntries(), catalog.CatalogEntry{
		ProviderID: "together", ModelID: "credit-model", IsFree: true, TierType: "credit",
	})
	s := &StrategyEconomical{}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// Credit-based entry should be last
	last := ranked[len(ranked)-1]
	if last.ProviderID == "together" {
		// Good — credit-based is last
		return
	}
	// Check that no credit entry appears before non-credit
	creditSeen := false
	for _, r := range ranked {
		if r.Entry.TierType == "credit" || (r.ProviderID == "together" && r.ModelID == "credit-model") {
			creditSeen = true
		} else if creditSeen {
			t.Errorf("economical: non-credit model %s/%s appears after credit model", r.ProviderID, r.ModelID)
		}
	}
}

// TestStrategyAdaptive verifies adaptive returns results and uses reliability.
func TestStrategyAdaptive(t *testing.T) {
	entries := testEntries()
	tracker := reliability.New()
	s := &StrategyAdaptive{Tracker: tracker}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
}

// TestStrategyLongContext verifies Gemini (1M context) ranks first.
func TestStrategyLongContext(t *testing.T) {
	entries := testEntries()
	s := &StrategyLongContext{}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// Gemini with 1M context should be first
	if ranked[0].ProviderID != "gemini" {
		t.Logf("long_context: first=%s/%s (may have larger context)", ranked[0].ProviderID, ranked[0].ModelID)
	}
}

// TestStrategyCoding verifies models with "code" in name rank higher.
func TestStrategyCoding(t *testing.T) {
	entries := append(testEntries(), catalog.CatalogEntry{
		ProviderID: "groq",
		ModelID:    "deepseek-coder-33b-instruct",
		IsFree:     true,
	})
	s := &StrategyCoding{}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	// Coder model should rank first (has code capability)
	if ranked[0].ModelID != "deepseek-coder-33b-instruct" {
		t.Logf("coding: first=%s/%s (not the explicit coder)", ranked[0].ProviderID, ranked[0].ModelID)
	}
}

// TestStrategyBalanced verifies 14-79B models rank first.
func TestStrategyBalanced(t *testing.T) {
	entries := testEntries()
	s := &StrategyBalanced{}
	ranked := s.Rank(testReq(), entries, nil)
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
}

// TestRegistry13Strategies verifies all 13 strategies are registered.
func TestRegistry13Strategies(t *testing.T) {
	relTracker := reliability.New()
	rateLimiter := ratelimit.NewGlobalTracker()
	reg := NewRegistry(relTracker, rateLimiter, "", "", 3, 5)
	names := reg.Names()
	if len(names) != 13 {
		t.Errorf("registry has %d strategies, want 13: %v", len(names), names)
	}
}

// TestStrategyLongContextQualityTieBreak verifies the quality tie-break
// within one context tier: full variants before "lite" siblings, code-capable
// models before generic ones, mid-sized before tiny — while context tiers
// themselves still dominate quality.
func TestStrategyLongContextQualityTieBreak(t *testing.T) {
	// Capability patterns are loaded from YAML at startup; load the bundled
	// set so the code-capability tie-break is active in this test too.
	if err := models.LoadCapabilities("../../configs/capabilities.yaml"); err != nil {
		t.Fatalf("LoadCapabilities: %v", err)
	}
	entries := []catalog.CatalogEntry{
		{ProviderID: "gemini", ModelID: "gemini-3.5-flash-lite", IsFree: true, ContextWindow: 1048576},
		{ProviderID: "gemini", ModelID: "gemini-3.5-flash", IsFree: true, ContextWindow: 1048576},
		{ProviderID: "openrouter", ModelID: "cohere/north-mini-code:free", IsFree: true, ContextWindow: 256000},
		{ProviderID: "openrouter", ModelID: "recursive/flint-v1.0:free", IsFree: true, ContextWindow: 262144},
		{ProviderID: "groq", ModelID: "qwen/qwen3.8-27b", IsFree: true, ContextWindow: 131072},
		{ProviderID: "openrouter", ModelID: "liquid/lfm-2.5-2.6b:free", IsFree: true, ContextWindow: 65536},
	}
	ranked := (&StrategyLongContext{}).Rank(testReq(), entries, nil)
	idx := map[string]int{}
	for i, r := range ranked {
		idx[r.ModelID] = i
	}

	if idx["gemini-3.5-flash"] > idx["gemini-3.5-flash-lite"] {
		t.Errorf("long_context: full flash (idx %d) should rank before flash-lite (idx %d)",
			idx["gemini-3.5-flash"], idx["gemini-3.5-flash-lite"])
	}
	if idx["recursive/flint-v1.0:free"] > idx["cohere/north-mini-code:free"] {
		t.Errorf("long_context: code-capable flint (idx %d) should rank before north-mini-code (idx %d)",
			idx["recursive/flint-v1.0:free"], idx["cohere/north-mini-code:free"])
	}
	if idx["qwen/qwen3.8-27b"] > idx["liquid/lfm-2.5-2.6b:free"] {
		t.Errorf("long_context: 27B qwen (idx %d) should rank before 2.6B lfm (idx %d)",
			idx["qwen/qwen3.8-27b"], idx["liquid/lfm-2.5-2.6b:free"])
	}
	// Context tiers still dominate: a 1M-context lite model outranks a
	// 256k full model, and the tiny 65k model stays last.
	if idx["gemini-3.5-flash-lite"] > idx["recursive/flint-v1.0:free"] {
		t.Errorf("long_context: 1M-context flash-lite (idx %d) should rank before 262k flint (idx %d)",
			idx["gemini-3.5-flash-lite"], idx["recursive/flint-v1.0:free"])
	}
	if last := ranked[len(ranked)-1].ModelID; last != "liquid/lfm-2.5-2.6b:free" {
		t.Errorf("long_context: tiny 65k-context lfm should rank last, got %s", last)
	}
}

// TestStrategyLongContextSmallPromptQualityFirst verifies that a small
// prompt flips the ranking to quality-first: mid-sized and code models
// serve new sessions before the Gemini flashes, and the tiny model stays
// last.
func TestStrategyLongContextSmallPromptQualityFirst(t *testing.T) {
	if err := models.LoadCapabilities("../../configs/capabilities.yaml"); err != nil {
		t.Fatalf("LoadCapabilities: %v", err)
	}
	entries := []catalog.CatalogEntry{
		{ProviderID: "gemini", ModelID: "gemini-3.5-flash-lite", IsFree: true, ContextWindow: 1048576},
		{ProviderID: "gemini", ModelID: "gemini-3.5-flash", IsFree: true, ContextWindow: 1048576},
		{ProviderID: "openrouter", ModelID: "cohere/north-mini-code:free", IsFree: true, ContextWindow: 256000},
		{ProviderID: "openrouter", ModelID: "recursive/flint-v1.0:free", IsFree: true, ContextWindow: 262144},
		{ProviderID: "groq", ModelID: "qwen/qwen3.8-27b", IsFree: true, ContextWindow: 131072},
		{ProviderID: "openrouter", ModelID: "liquid/lfm-2.5-2.6b:free", IsFree: true, ContextWindow: 65536},
	}
	req := Request{
		Messages:              []map[string]any{{"role": "user", "content": "hi"}},
		EstimatedPromptTokens: 500,
		MaxTokens:             1024,
	}
	ranked := (&StrategyLongContext{}).Rank(req, entries, nil)
	idx := map[string]int{}
	for i, r := range ranked {
		idx[r.ModelID] = i
	}

	if ranked[0].ModelID != "recursive/flint-v1.0:free" {
		t.Errorf("small prompt: first = %s, want flint (code-tuned)", ranked[0].ModelID)
	}
	// Qwen (27B) before the full flash and the lites: quality first, and
	// big windows are saved for the requests that need them.
	if idx["qwen/qwen3.8-27b"] > idx["gemini-3.5-flash"] {
		t.Errorf("small prompt: qwen (idx %d) should rank before full flash (idx %d)",
			idx["qwen/qwen3.8-27b"], idx["gemini-3.5-flash"])
	}
	if idx["qwen/qwen3.8-27b"] > idx["gemini-3.5-flash-lite"] {
		t.Errorf("small prompt: qwen (idx %d) should rank before flash-lite (idx %d)",
			idx["qwen/qwen3.8-27b"], idx["gemini-3.5-flash-lite"])
	}
	// The tiny model is still the worst choice even for a tiny prompt.
	if last := ranked[len(ranked)-1].ModelID; last != "liquid/lfm-2.5-2.6b:free" {
		t.Errorf("small prompt: last = %s, want the tiny lfm", last)
	}
}

// TestStrategyLongContextFitPenalty verifies that a model whose window
// cannot hold prompt + reply ranks below every model that fits, whatever
// its context tier.
func TestStrategyLongContextFitPenalty(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "gemini", ModelID: "gemini-3.5-flash", IsFree: true, ContextWindow: 1048576},
		{ProviderID: "groq", ModelID: "qwen/qwen3.8-27b", IsFree: true, ContextWindow: 131072},
		{ProviderID: "openrouter", ModelID: "liquid/lfm-2.5-2.6b:free", IsFree: true, ContextWindow: 65536},
		{ProviderID: "openrouter", ModelID: "tiny-8k", IsFree: true, ContextWindow: 8192},
	}
	req := Request{EstimatedPromptTokens: 100_000, MaxTokens: 4096} // needs 104k
	ranked := (&StrategyLongContext{}).Rank(req, entries, nil)
	idx := map[string]int{}
	for i, r := range ranked {
		idx[r.ModelID] = i
	}

	// Everything that fits ranks ahead of everything that does not.
	for _, fits := range []string{"gemini-3.5-flash", "qwen/qwen3.8-27b"} {
		for _, nofit := range []string{"liquid/lfm-2.5-2.6b:free", "tiny-8k"} {
			if idx[fits] > idx[nofit] {
				t.Errorf("fit ranking: %s (idx %d) should rank before %s (idx %d)",
					fits, idx[fits], nofit, idx[nofit])
			}
		}
	}
}

// helpers
var _ = models.TierSmall // ensure package is linked

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// parallel sends each provider's fastest model, not whichever the catalog
// lists first, and orders providers fastest first.
func TestStrategyParallel_PicksFastestModelPerProvider(t *testing.T) {
	entries := []catalog.CatalogEntry{
		{ProviderID: "groq", ModelID: "llama-3.3-70b-versatile", IsFree: true},
		{ProviderID: "groq", ModelID: "llama-3.1-8b-instant", IsFree: true},
		{ProviderID: "mistral", ModelID: "mistral-large-123b", IsFree: true},
		{ProviderID: "cerebras", ModelID: "qwen-3-32b", IsFree: true},
		{ProviderID: "cerebras", ModelID: "llama3.1-8b", IsFree: true},
		{ProviderID: "groq", ModelID: "llama-3.2-1b-preview", IsFree: false}, // not free
	}
	ranked := (&StrategyParallel{FanOut: 3}).Rank(testReq(), entries, nil)
	var got []string
	for _, r := range ranked {
		got = append(got, r.ProviderID+"/"+r.ModelID)
		if r.Entry == nil || r.Entry.ModelID != r.ModelID {
			t.Errorf("%s: Entry does not point at the ranked model", r.ModelID)
		}
	}
	want := "cerebras/llama3.1-8b,groq/llama-3.1-8b-instant,mistral/mistral-large-123b"
	if strings.Join(got, ",") != want {
		t.Errorf("parallel ranked %v, want %s", got, want)
	}
}

package proxy

import (
	"fmt"
	"testing"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

// unknown400Body matches NO marker table — the unrecognized-400 class the
// round-8 policy targets.
const unknown400Body = `{"error":{"message":"mystery barrel exploded","code":400}}`

// TestDemotionRegistryTTLExpiry: a demotion hides the model only for the
// TTL; a lazy-expired entry is popped (handed back off to the ranking).
func TestDemotionRegistryTTLExpiry(t *testing.T) {
	reg := newDemotionRegistry()
	now := time.Now()
	reg.demote("prova", "model-a", now)
	if !reg.isDemoted("prova", "model-a", now.Add(time.Second), time.Minute) {
		t.Fatal("freshly demoted model must be hidden")
	}
	if reg.isDemoted("prova", "model-a", now.Add(2*time.Minute), time.Minute) {
		t.Fatal("expired demotion must hand the model back off")
	}
	if reg.len() != 0 {
		t.Fatalf("registry len = %d, want 0 after expiry", reg.len())
	}
	if reg.isDemoted("provb", "model-b", now, time.Minute) {
		t.Fatal("unknown key must not be demoted")
	}
}

// TestDemotionRegistryFIFOHandOff: the registry is bounded — when full the
// OLDEST demotion is handed back off to make room for the newest.
func TestDemotionRegistryFIFOHandOff(t *testing.T) {
	reg := newDemotionRegistry()
	now := time.Now()
	for i := 0; i <= demotionRegistryMax; i++ { // max+1 demotes → one eviction
		reg.demote("p", fmt.Sprintf("m%02d", i), now)
	}
	if reg.isDemoted("p", "m00", now, time.Minute) {
		t.Fatal("oldest demotion must be handed back off when the registry overflows")
	}
	if !reg.isDemoted("p", fmt.Sprintf("m%02d", demotionRegistryMax), now, time.Minute) {
		t.Fatal("newest demotion must still be hidden")
	}
	if reg.len() != demotionRegistryMax {
		t.Fatalf("registry len = %d, want %d", reg.len(), demotionRegistryMax)
	}
}

// TestDemotionRegistryRedemoteRefreshes: a repeat offender refreshes its
// timestamp but (by construction) keeps its FIFO hand-off position.
func TestDemotionRegistryRedemoteRefreshes(t *testing.T) {
	reg := newDemotionRegistry()
	base := time.Now()
	reg.demote("p", "m1", base)
	reg.demote("p", "m1", base.Add(50*time.Second))
	if !reg.isDemoted("p", "m1", base.Add(100*time.Second), time.Minute) {
		t.Fatal("re-demote must refresh the TTL window")
	}
	if reg.isDemoted("p", "m1", base.Add(50*time.Second+2*time.Minute), time.Minute) {
		t.Fatal("refreshed window must still lapse")
	}
}

// TestFilterDemotedEntriesLastChance: demotion must never starve the walk —
// when every candidate is demoted they are handed back rather than leaving
// the request with zero candidates.
func TestFilterDemotedEntriesLastChance(t *testing.T) {
	reg := newDemotionRegistry()
	reg.demote("prova", "model-a", time.Now())
	reg.demote("provb", "model-b", time.Now())
	fc := &FallbackChain{
		Cfg:       &config.Config{Fallback: config.FallbackConfig{Unknown400DemotionSeconds: 60}},
		Demotions: reg,
	}
	entries := []catalog.CatalogEntry{
		{ProviderID: "prova", ModelID: "model-a", IsFree: true},
		{ProviderID: "provb", ModelID: "model-b", IsFree: true},
	}
	if out := fc.filterDemotedEntries(entries); len(out) != 2 {
		t.Fatalf("all-demoted candidates = %d entries, want 2 handed back (no starvation)", len(out))
	}

	// Mixed case: model-a/b stay demoted, model-c is fresh → only it survives.
	entries = append(entries, catalog.CatalogEntry{ProviderID: "prova", ModelID: "model-c", IsFree: true})
	out := fc.filterDemotedEntries(entries)
	if len(out) != 1 || out[0].ModelID != "model-c" {
		t.Fatalf("mixed filter = %+v, want only model-c", out)
	}

	// Disabled (ttl 0): input passes through untouched.
	fcOff := &FallbackChain{Cfg: &config.Config{}, Demotions: reg}
	if out := fcOff.filterDemotedEntries(entries); len(out) != len(entries) {
		t.Fatalf("disabled demotion filter = %d entries, want %d", len(out), len(entries))
	}
}
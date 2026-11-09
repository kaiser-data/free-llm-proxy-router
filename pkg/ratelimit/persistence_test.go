package ratelimit

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsage_SnapshotRestoreRoundTrip(t *testing.T) {
	g := NewGlobalTracker()
	for i := 0; i < 3; i++ {
		g.Provider("groq").RecordRequest("llama")
	}
	g.Provider("gemini").RecordRequest("gemma")

	path := filepath.Join(t.TempDir(), "usage.json")
	if err := SaveUsage(g.Snapshot(), path); err != nil {
		t.Fatal(err)
	}
	u, err := LoadUsage(path)
	if err != nil {
		t.Fatal(err)
	}

	g2 := NewGlobalTracker()
	g2.Restore(u)
	// RPD limit 4: three requests restored, so one more fits, two don't.
	if !g2.Provider("groq").CanRequest("llama", 0, 4) {
		t.Error("restored count too high: 4th request should be allowed")
	}
	if g2.Provider("groq").CanRequest("llama", 0, 3) {
		t.Error("restored count lost: limit 3 should already be reached")
	}
	if g2.Provider("gemini").CanRequest("gemma", 0, 1) {
		t.Error("gemini count not restored")
	}
}

func TestUsage_RestoreSkipsExpiredWindows(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	u := &UsageSnapshot{Providers: map[string]ProviderUsage{
		"groq": {ModelDayCounts: map[string]int{"llama": 99}, ModelDayEnds: map[string]int64{"llama": past}},
	}}
	g := NewGlobalTracker()
	g.Restore(u)
	if !g.Provider("groq").CanRequest("llama", 0, 1) {
		t.Error("expired day window was restored")
	}
}

func TestRecordRequest_CountsOnce(t *testing.T) {
	p := NewProviderTracker()
	p.RecordRequest("m")
	if !p.CanRequest("m", 2, 2) {
		t.Error("one request recorded but limit 2 already reached (double count)")
	}
	p.RecordRequest("m")
	if p.CanRequest("m", 2, 2) {
		t.Error("two requests recorded but limit 2 not reached")
	}
}

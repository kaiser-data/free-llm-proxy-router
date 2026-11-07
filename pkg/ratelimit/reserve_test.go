package ratelimit

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// Concurrent requests must not all pass the check before any of them counts:
// admission and counting happen under one lock.
func TestGeminiReserve_ConcurrentNeverOvershoots(t *testing.T) {
	gt := NewGeminiTracker()
	gt.SetLimits("m", GeminiModelLimits{RPM: 10, RPD: 100})

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := gt.Reserve("m", 0); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 10 {
		t.Errorf("admitted %d concurrent requests, want exactly the RPM limit 10", admitted)
	}
}

func TestGeminiReserve_ReleaseFreesTheSlot(t *testing.T) {
	gt := NewGeminiTracker()
	gt.SetLimits("m", GeminiModelLimits{RPM: 1, TPM: 100})

	release, ok := gt.Reserve("m", 60)
	if !ok {
		t.Fatal("first reservation refused")
	}
	if _, ok := gt.Reserve("m", 60); ok {
		t.Fatal("second reservation admitted while the first holds the only slot")
	}
	release()
	release() // releasing twice must not free a slot that is not held
	if _, ok := gt.Reserve("m", 60); !ok {
		t.Fatal("released slot was not freed")
	}
	if _, ok := gt.Reserve("m", 0); ok {
		t.Fatal("a double release freed an extra slot")
	}
}

// Gemini daily counts must survive a restart, like the provider counts do.
func TestGeminiSnapshot_RoundTripsDailyCounts(t *testing.T) {
	gt := NewGeminiTracker()
	gt.SetLimits("m", GeminiModelLimits{RPD: 3})
	for i := 0; i < 3; i++ {
		gt.RecordRequest("m", 0)
	}

	restored := NewGeminiTracker()
	restored.SetLimits("m", GeminiModelLimits{RPD: 3})
	restored.Restore(gt.Snapshot())
	if restored.CanRequest("m", 0) {
		t.Error("restored tracker forgot the 3 requests already made today")
	}
}

func TestGeminiRestore_IgnoresYesterdaysCounts(t *testing.T) {
	gt := NewGeminiTracker()
	gt.SetLimits("m", GeminiModelLimits{RPD: 1})
	snap := &GeminiUsage{DayStart: midnightPT().Add(-24 * time.Hour).Unix(), ModelDayCounts: map[string]int{"m": 5}}
	gt.Restore(snap)
	if !gt.CanRequest("m", 0) {
		t.Error("counts from a previous Pacific day were restored")
	}
}

// Spacing applies across concurrent requests, not per request: each caller
// gets its own slot, spaced by the interval.
func TestProviderTracker_SpacingIsGlobal(t *testing.T) {
	pt := NewProviderTracker()
	const spacing = 100 * time.Millisecond

	var wg sync.WaitGroup
	var mu sync.Mutex
	var waits []time.Duration
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := pt.NextSlot(spacing)
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	if waits[0] > 10*time.Millisecond {
		t.Errorf("first caller waited %v, want ~0", waits[0])
	}
	for i := 1; i < len(waits); i++ {
		if gap := waits[i] - waits[i-1]; gap < spacing-10*time.Millisecond {
			t.Errorf("callers %d and %d are only %v apart, want >= %v", i-1, i, gap, spacing)
		}
	}
}

// usage.json carries the Gemini counts through SaveUsage/LoadUsage.
func TestSaveUsage_PersistsGeminiCounts(t *testing.T) {
	gt := NewGeminiTracker()
	gt.RecordRequest("m", 0)
	u := NewGlobalTracker().Snapshot()
	u.Gemini = gt.Snapshot()
	path := t.TempDir() + "/usage.json"
	if err := SaveUsage(u, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Gemini == nil || loaded.Gemini.ModelDayCounts["m"] != 1 {
		t.Errorf("gemini counts lost in usage.json: %+v", loaded.Gemini)
	}
}

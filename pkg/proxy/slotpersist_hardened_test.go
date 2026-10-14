package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSlotStateCorruptVariantsStartFresh (hardening): a corrupt, empty, or
// whitespace slots.json never panics and never poisons the limiter — the
// state starts empty and fresh traffic flows.
func TestSlotStateCorruptVariantsStartFresh(t *testing.T) {
	variants := map[string][]byte{
		"empty":      {},
		"whitespace": []byte("   \n\t "),
		"truncated":  []byte(`{"version":1,"slots":{"prov/m":{"reservations":[`),
		"garbage":    []byte("not json at all"),
		"wrong-type": []byte(`{"version":1,"slots":"nope"}`),
		"null-entry": []byte(`{"version":1,"slots":{"prov/m":null}}`),
	}
	for name, data := range variants {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "slots.json")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatalf("write variant: %v", err)
			}
			fresh := NewSlotLimiter()
			err := fresh.LoadState(path) // may be an error — must not panic
			if (name == "empty" || name == "whitespace" || name == "null-entry") && err != nil {
				t.Fatalf("tolerated variant %s must load clean, got %v", name, err)
			}
			// The limiter stays fully functional either way.
			if granted, _ := fresh.Reserve("prov/m", 1); !granted {
				t.Fatalf("limiter must serve fresh traffic after corrupt load (%s)", name)
			}
		})
	}
}

// TestSlotStateUnknownVersionStartsFresh: a file written by a NEWER build
// (version this code does not know) is rejected with a clear error — its
// semantics are unknown, so nothing is loaded and the limiter starts fresh.
func TestSlotStateUnknownVersionStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	data := fmt.Sprintf(`{"version":99,"saved_at":%q,"slots":{"prov/m":{"reservations":[%q]}}}`,
		time.Now().Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fresh := NewSlotLimiter()
	err := fresh.LoadState(path)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("unknown version must be rejected with a version error, got %v", err)
	}
	if granted, _ := fresh.Reserve("prov/m", 1); !granted {
		t.Fatal("limiter must serve fresh traffic after unknown-version load")
	}
}

// TestSlotStateLegacyVersion0StillLoads: a file written by the pre-versioning
// build (task T-7 — no version field) still loads, so the upgrade keeps the
// quota state the previous build saved.
func TestSlotStateLegacyVersion0StillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	data := fmt.Sprintf(`{"saved_at":%q,"slots":{"prov/m":{"reservations":[],"blocked_until":%q}}}`,
		time.Now().Format(time.RFC3339Nano), time.Now().Add(time.Minute).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fresh := NewSlotLimiter()
	if err := fresh.LoadState(path); err != nil {
		t.Fatalf("legacy v0 file must load: %v", err)
	}
	if granted, _ := fresh.Reserve("prov/m", 1); granted {
		t.Fatal("legacy 429 block must survive the upgrade")
	}
}

// TestSlotStateClampsFutureTimestamps: a bad or hostile file with far-future
// timestamps can never deny a model longer than window+margin — reservations
// in the future are pinned to now, a 429 block is clamped to the ceiling.
func TestSlotStateClampsFutureTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	futureResv := time.Now().Add(30 * time.Minute).Format(time.RFC3339Nano)
	farBlock := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	data := fmt.Sprintf(`{"version":1,"slots":{"prov/far":{"reservations":[%q],"blocked_until":%q}}}`,
		futureResv, farBlock)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	fresh := NewSlotLimiter()
	if err := fresh.LoadState(path); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	next := fresh.NextAvailable("prov/far")
	if next.IsZero() {
		t.Fatal("clamped state must still deny for a while (the clamp ceiling is window+margin, not zero)")
	}
	maxDeny := time.Minute + slotClampMargin + 5*time.Second // slack for test timing
	if d := time.Until(next); d > maxDeny {
		t.Fatalf("clamped nextAvailable = %s, want <= %s", d, maxDeny)
	}
	if d := time.Until(next); d < time.Minute {
		t.Fatalf("clamped nextAvailable = %s, want >= one window", d)
	}
}

// TestSaveStateWritesVersionAndLeavesNoTmp: every save is a complete,
// versioned file and the tmp never survives — an interrupted save cannot
// leave a half-written slots.json behind.
func TestSaveStateWritesVersionAndLeavesNoTmp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	l := NewSlotLimiter()
	l.AdjustNextAvailable("prov/m", time.Now().Add(time.Minute))
	if err := l.SaveState(path); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file must not survive SaveState, stat err = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	var f slotStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("saved file must parse: %v", err)
	}
	if f.Version != slotStateVersion {
		t.Fatalf("saved version = %d, want %d", f.Version, slotStateVersion)
	}
}

// TestPeriodicSaveWritesAndStops: StartPeriodicSave writes on its interval
// and stops once the context is canceled — the shutdown save in main is the
// final write, the ticker must not outlive it.
func TestPeriodicSaveWritesAndStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	l := NewSlotLimiter()
	l.AdjustNextAvailable("prov/m", time.Now().Add(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	l.StartPeriodicSave(ctx, path, 20*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic save never wrote the state file")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	time.Sleep(100 * time.Millisecond) // let any in-flight save finish
	raw1, _ := os.ReadFile(path)
	time.Sleep(80 * time.Millisecond) // > 3 intervals
	raw2, _ := os.ReadFile(path)
	if string(raw1) != string(raw2) {
		t.Fatal("ticker must stop after ctx cancel — the file changed after cancel")
	}
}

// TestConcurrentSaveStateSafe: the periodic saver and the shutdown save can
// overlap — concurrent SaveState calls serialize and always leave a valid
// file (run under -race to prove the locking).
func TestConcurrentSaveStateSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	l := NewSlotLimiter()
	l.AdjustNextAvailable("prov/m", time.Now().Add(time.Minute))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.SaveState(path); err != nil {
				t.Errorf("concurrent SaveState: %v", err)
			}
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after concurrent saves: %v", err)
	}
	var f slotStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("file after concurrent saves must be valid JSON: %v", err)
	}
}

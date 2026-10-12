package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSlotStateSurvivesRestart (task T-7 fix): the per-candidate state a
// restart must not forget — active window reservations and a 429-driven
// block — round-trips through slots.json, and a fresh limiter denies the
// requests the old limiter would have denied.
func TestSlotStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")

	old := NewSlotLimiter()
	if granted, _ := old.Reserve("prov/m", 1); !granted {
		t.Fatal("first reservation must be granted")
	}
	if granted, _ := old.Reserve("prov/m", 1); granted {
		t.Fatal("second reservation must be denied (window full)")
	}
	old.AdjustNextAvailable("prov/other", time.Now().Add(time.Minute))
	if err := old.SaveState(path); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	fresh := NewSlotLimiter()
	if err := fresh.LoadState(path); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if granted, next := fresh.Reserve("prov/m", 1); granted {
		t.Fatal("fresh limiter must deny — the reservation survived the restart")
	} else if d := time.Until(next); d < 55*time.Second || d > 61*time.Second {
		t.Fatalf("nextAvailable = %s, want ~1 minute out", d)
	}
	if granted, _ := fresh.Reserve("prov/other", 1); granted {
		t.Fatal("fresh limiter must deny — the 429 block survived the restart")
	}
	// A candidate the old limiter never saw is unaffected.
	if granted, _ := fresh.Reserve("prov/unknown", 1); !granted {
		t.Fatal("unknown candidate must be granted")
	}
}

// TestSlotStateLoadDropsExpired: reservations that fell out of the window
// and 429 blocks whose time has passed are dropped on load — a stale file
// never blocks fresh traffic.
func TestSlotStateLoadDropsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	frozen := time.Now().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	goneBlock := time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	crafted := fmt.Sprintf(`{"saved_at":%q,"slots":{`+
		`"prov/old":{"reservations":[%q]},`+
		`"prov/gone-block":{"reservations":[],"blocked_until":%q}}}`,
		time.Now().Format(time.RFC3339Nano), frozen, goneBlock)
	if err := os.WriteFile(path, []byte(crafted), 0o644); err != nil {
		t.Fatalf("write crafted file: %v", err)
	}

	fresh := NewSlotLimiter()
	if err := fresh.LoadState(path); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if granted, _ := fresh.Reserve("prov/old", 1); !granted {
		t.Fatal("expired reservation must prune on load — fresh traffic must pass")
	}
	if granted, _ := fresh.Reserve("prov/gone-block", 1); !granted {
		t.Fatal("expired 429 block must prune on load — fresh traffic must pass")
	}
}

// TestSlotStateRoundTripKeepsReservationCount: the saved file carries every
// active reservation (not just the first), so a restart mid-window keeps the
// window exactly as full as it was.
func TestSlotStateRoundTripKeepsReservationCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")

	old := NewSlotLimiter()
	for i := 0; i < 3; i++ {
		if granted, _ := old.Reserve("prov/multi", 3); !granted {
			t.Fatalf("reservation %d must be granted", i+1)
		}
	}
	if err := old.SaveState(path); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	var f slotStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse saved file: %v", err)
	}
	if got := len(f.Slots["prov/multi"].Reservations); got != 3 {
		t.Fatalf("saved reservations = %d, want 3", got)
	}
	fresh := NewSlotLimiter()
	if err := fresh.LoadState(path); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if granted, _ := fresh.Reserve("prov/multi", 3); granted {
		t.Fatal("4th reservation must be denied — all 3 slots survived the restart")
	}
}

// TestSlotStateMissingFileIsFreshStart: no slots.json is not an error.
func TestSlotStateMissingFileIsFreshStart(t *testing.T) {
	fresh := NewSlotLimiter()
	if err := fresh.LoadState(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Fatalf("LoadState on missing file must be nil, got %v", err)
	}
}

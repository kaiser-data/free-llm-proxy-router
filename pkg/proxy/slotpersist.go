package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Slot-state persistence (task T-7 follow-up).
//
// The slot limiter's per-candidate state — active window reservations and
// 429-driven blocks — used to live only in memory, so a restart forgot a
// model the proxy had just exhausted and re-hit its real upstream quota
// immediately (sandbox T-7 evidence: two upstream 429s inside one minute
// across a graceful restart). slots.json mirrors reliability.json's
// pattern: written atomically on graceful shutdown, read on boot.
//
// Entries whose reservations fell out of the window and whose 429 block has
// passed prune themselves on first use, so a crash that skips the save only
// loses the protection, never correctness.

// slotStateFile is the on-disk format for ~/.free-llm-proxy-router/slots.json.
// slotState itself has unexported fields (encoding/json skips those), so the
// file round-trips through the exported slotStateDTO.
type slotStateFile struct {
	SavedAt time.Time                `json:"saved_at"`
	Slots   map[string]*slotStateDTO `json:"slots"`
}

// slotStateDTO is the exported wire form of one candidate's slotState.
type slotStateDTO struct {
	Reservations []time.Time `json:"reservations"`
	BlockedUntil time.Time   `json:"blocked_until"`
}

func toDTO(st *slotState) *slotStateDTO {
	if st == nil {
		return nil
	}
	return &slotStateDTO{Reservations: st.reservations, BlockedUntil: st.blockedUntil}
}

func (d *slotStateDTO) toState() *slotState {
	if d == nil {
		return nil
	}
	return &slotState{reservations: d.Reservations, blockedUntil: d.BlockedUntil}
}

// SlotLimiter exposes the shared RPM slot limiter so main can wire the
// slots.json load/save around boot and graceful shutdown.
func (s *Server) SlotLimiter() *SlotLimiter { return s.slotLimit }

// SaveState writes the limiter's per-candidate state to a JSON file
// atomically (tmp + rename). Candidates with no live state are skipped.
func (l *SlotLimiter) SaveState(path string) error {
	l.mu.Lock()
	f := slotStateFile{SavedAt: time.Now(), Slots: make(map[string]*slotStateDTO, len(l.states))}
	for k, st := range l.states {
		if st == nil || (len(st.reservations) == 0 && st.blockedUntil.IsZero()) {
			continue
		}
		f.Slots[k] = toDTO(st)
	}
	l.mu.Unlock()
	expanded := expandHomePath(path)
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return fmt.Errorf("creating slots state dir: %w", err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding slots state: %w", err)
	}
	tmp := expanded + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing slots state tmp: %w", err)
	}
	return os.Rename(tmp, expanded)
}

// LoadState restores per-candidate state saved by SaveState. A missing file
// is not an error (fresh start). Expired entries are dropped on load; the
// survivors keep pruning on first use through the normal Reserve path.
func (l *SlotLimiter) LoadState(path string) error {
	data, err := os.ReadFile(expandHomePath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading slots state %s: %w", path, err)
	}
	var f slotStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parsing slots state %s: %w", path, err)
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, dto := range f.Slots {
		st := dto.toState()
		if st == nil {
			continue
		}
		l.pruneLocked(st, now)
		if len(st.reservations) == 0 && st.blockedUntil.IsZero() {
			continue
		}
		l.states[k] = st
	}
	return nil
}

// expandHomePath resolves a leading "~/..." against the user's home
// directory (same convention as the reliability state file).
func expandHomePath(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
//
// Hardening (follow-up): writes fsync the tmp file before the atomic rename;
// the file carries a version (corrupt/empty/unknown → warn + fresh start,
// never a panic or a poisoned limiter); loaded timestamps reaching further
// than window+margin into the future are clamped; and a periodic save
// (proxy.slot_persist_interval_seconds) bounds what a hard crash loses.

// slotStateVersion is the on-disk format version this build writes. Version
// 0 (no version field) is the pre-versioning layout — still readable, so an
// upgrade keeps the state the previous build saved.
const slotStateVersion = 1

// slotClampMargin bounds how far into the future a loaded 429 block may sit:
// window + margin is the longest denial a slots.json can impose after a
// restart. A legitimate upstream Retry-After beyond that re-arms itself on
// the next 429 — the protection is best-effort, a bad or hostile file must
// never deny a model for hours.
const slotClampMargin = 5 * time.Minute

// slotStateFile is the on-disk format for ~/.free-llm-proxy-router/slots.json.
// slotState itself has unexported fields (encoding/json skips those), so the
// file round-trips through the exported slotStateDTO.
type slotStateFile struct {
	Version int                      `json:"version"`
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
// atomically (tmp + fsync + rename). Candidates with no live state are
// skipped. Safe for concurrent use: the periodic saver and the shutdown
// save serialize on saveMu.
func (l *SlotLimiter) SaveState(path string) error {
	// The periodic saver and the graceful-shutdown save can overlap —
	// serialize them so two writers never share the tmp file.
	l.saveMu.Lock()
	defer l.saveMu.Unlock()

	l.mu.Lock()
	f := slotStateFile{Version: slotStateVersion, SavedAt: time.Now(), Slots: make(map[string]*slotStateDTO, len(l.states))}
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
	// Atomic write with durability: fsync the tmp file BEFORE the rename,
	// so a power loss between rename and disk flush can never leave a
	// truncated slots.json — the rename publishes fully written bytes or
	// nothing, and LoadState starts fresh on the nothing.
	tmp := expanded + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening slots state tmp: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("writing slots state tmp: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("fsyncing slots state tmp: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing slots state tmp: %w", err)
	}
	return os.Rename(tmp, expanded)
}

// LoadState restores per-candidate state saved by SaveState. A missing,
// empty, or truncated file is a fresh start, never an error the limiter
// refuses to boot over; a corrupt file is reported so main logs the warning
// and starts fresh (hardening: no panic, no poisoned limiter). Unsupported
// future versions are rejected the same way — the build that wrote them
// knows their semantics. Expired entries are dropped on load; the survivors
// keep pruning on first use through the normal Reserve path.
func (l *SlotLimiter) LoadState(path string) error {
	data, err := os.ReadFile(expandHomePath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading slots state %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil // empty file (disk-full history, interrupted writer): fresh start
	}
	var f slotStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parsing slots state %s (starting fresh): %w", path, err)
	}
	if f.Version > slotStateVersion {
		return fmt.Errorf("slots state %s has unsupported version %d (this build writes %d) — starting fresh", path, f.Version, slotStateVersion)
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, dto := range f.Slots {
		st := dto.toState()
		if st == nil {
			continue
		}
		l.clampLoadedLocked(st, now)
		l.pruneLocked(st, now)
		if len(st.reservations) == 0 && st.blockedUntil.IsZero() {
			continue
		}
		l.states[k] = st
	}
	return nil
}

// clampLoadedLocked bounds the future a restored entry may deny: a
// reservation never belongs in the future (it is written at grant time), and
// a 429 block may reach at most window+margin past now. A bad or hostile
// slots.json can therefore never deny a model longer than that. Caller
// holds l.mu.
func (l *SlotLimiter) clampLoadedLocked(st *slotState, now time.Time) {
	blockCeil := now.Add(l.window + slotClampMargin)
	for i, r := range st.reservations {
		if r.After(now) {
			st.reservations[i] = now
		}
	}
	if st.blockedUntil.After(blockCeil) {
		st.blockedUntil = blockCeil
	}
}

// StartPeriodicSave writes slots.json every interval until ctx is canceled
// (proxy.slot_persist_interval_seconds, T-7 hardening): a hard crash
// (SIGKILL, OOM, power loss) then loses at most one interval of protection
// instead of everything since boot. The graceful-shutdown save in main
// stays the final, complete write. interval <= 0 disables the periodic
// save; a failed write is logged and the loop keeps going — one error must
// not end the protection.
func (l *SlotLimiter) StartPeriodicSave(ctx context.Context, path string, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := l.SaveState(path); err != nil {
					log.Printf("periodic slots save: %v", err)
				}
			}
		}
	}()
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

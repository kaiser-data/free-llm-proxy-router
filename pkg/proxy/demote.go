package proxy

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/alias"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
)

// demotionRegistryMax bounds the registry: when full, the OLDEST demotion
// is handed back off to the ranking (FIFO) to make room — a broken-model
// burst can never pin more than this many models out of rotation, and the
// oldest offenders are the first to be re-tried.
const demotionRegistryMax = 64

// demotionRegistry is the cross-request memory of upstream models that
// answered an UNRECOGNIZED HTTP 400 (round 8 steps 3–4). Step 2's
// cross-model confirm protects a single request; this registry makes the
// lesson stick: the next request's walk skips the demoted model until the
// TTL lapses, so the free proxy's flaky 400-ers stop burning the first
// hops of every request.
//
// Semantics:
//   - Key is "providerID/modelID" — the same identity the walk's per-model
//     cooldowns use.
//   - Re-demoting a key refreshes its timestamp but keeps its FIFO position
//     (a repeat offender does not jump the hand-off queue).
//   - Expiry is lazy: isDemoted pops an expired entry (handed back off to
//     ranking). Capacity overflow pops the OLDEST entry (also handed back
//     off). Both bounds guarantee every demotion eventually re-enters the
//     ranking — no model is ever permanently hidden by a runtime failure.
//   - The registry stores only timestamps; the TTL lives in config
//     (fallback.unknown_400_demotion_seconds) and is read per call, so a
//     config hot-reload takes effect immediately.
type demotionRegistry struct {
	mu      sync.Mutex
	entries map[string]time.Time // "provider/model" → demoted at
	order   []string             // demotion order, oldest first (FIFO hand-off)
}

func newDemotionRegistry() *demotionRegistry {
	return &demotionRegistry{entries: make(map[string]time.Time)}
}

func demotionKey(providerID, modelID string) string {
	return providerID + "/" + modelID
}

// demote records the failure at now. A nil registry is a no-op (chains
// built without the registry keep the legacy behavior).
func (d *demotionRegistry) demote(providerID, modelID string, now time.Time) {
	if d == nil {
		return
	}
	key := demotionKey(providerID, modelID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.entries[key]; ok {
		d.entries[key] = now // refresh, keep FIFO position
		return
	}
	if len(d.order) >= demotionRegistryMax {
		// FIFO hand-off under pressure: the oldest demotion re-enters
		// the ranking to make room.
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.entries, oldest)
	}
	d.entries[key] = now
	d.order = append(d.order, key)
}

// isDemoted reports whether the model is currently demoted at now, given
// ttl. ttl <= 0 (demotion disabled) or a nil registry answers false. An
// expired entry is popped — the model is handed back off to the ranking.
func (d *demotionRegistry) isDemoted(providerID, modelID string, now time.Time, ttl time.Duration) bool {
	if d == nil || ttl <= 0 {
		return false
	}
	key := demotionKey(providerID, modelID)
	d.mu.Lock()
	defer d.mu.Unlock()
	at, ok := d.entries[key]
	if !ok {
		return false
	}
	if now.Sub(at) >= ttl {
		d.removeLocked(key)
		return false
	}
	return true
}

// len reports the currently-demoted model count (tests + debug).
func (d *demotionRegistry) len() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// removeLocked drops a key from both the timestamp map and the FIFO order.
// Caller holds mu.
func (d *demotionRegistry) removeLocked(key string) {
	delete(d.entries, key)
	for i, k := range d.order {
		if k == key {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}

// demotionTTL returns the effective cross-request demotion TTL; 0 disables
// demotion (including for zeroed FallbackConfigs as built by tests).
func (fc *FallbackChain) demotionTTL() time.Duration {
	if s := fc.Cfg.Fallback.Unknown400DemotionSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

// filterDemotedEntries drops currently-demoted models from the candidate
// list (round 8 step 4). The demoted candidates are NOT allowed to starve
// the walk: when EVERY entry is demoted they are handed back (last chance)
// instead of leaving the request with no candidates. Disabled (nil registry
// or ttl 0) returns the input unchanged.
func (fc *FallbackChain) filterDemotedEntries(entries []catalog.CatalogEntry) []catalog.CatalogEntry {
	ttl := fc.demotionTTL()
	if ttl <= 0 || fc.Demotions == nil || len(entries) == 0 {
		return entries
	}
	now := time.Now()
	kept := make([]catalog.CatalogEntry, 0, len(entries))
	var demoted []catalog.CatalogEntry
	for _, e := range entries {
		if fc.Demotions.isDemoted(e.ProviderID, e.ModelID, now, ttl) {
			demoted = append(demoted, e)
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		return demoted // every candidate demoted — last chance beats starving
	}
	return kept
}

// filterDemotedUpstreams is the alias-route twin of filterDemotedEntries:
// that route walks []alias.Upstream, not catalog entries. Same semantics —
// demoted candidates are dropped unless EVERY candidate is demoted, in
// which case they are handed back (last chance) rather than starving the
// request.
func (fc *FallbackChain) filterDemotedUpstreams(ups []alias.Upstream) []alias.Upstream {
	ttl := fc.demotionTTL()
	if ttl <= 0 || fc.Demotions == nil || len(ups) == 0 {
		return ups
	}
	now := time.Now()
	kept := make([]alias.Upstream, 0, len(ups))
	var demoted []alias.Upstream
	for _, u := range ups {
		if fc.Demotions.isDemoted(u.ProviderID, u.ModelID, now, ttl) {
			demoted = append(demoted, u)
			continue
		}
		kept = append(kept, u)
	}
	if len(kept) == 0 {
		return demoted // every candidate demoted — last chance beats starving
	}
	return kept
}

// failureDecisionWalk classifies a candidate's LAST failed attempt WITH the
// cross-model unknown-400 policy (round 8 step 2). It is wired at every
// genuine multi-candidate walk: the ranked walk (fallback.go), the direct
// route's catalog-entry loop, and the alias route's upstream loop. The
// openrouter models[] and openrouter/free ultimate-fallback steps keep
// finalFailureDecision — they speak a multi-model / router-meta wire shape,
// so "confirm across models" is meaningless there.
//
// On an UNRECOGNIZED 400 with fallback.unknown_400_policy=cross_model_confirm
// the failing model joins the per-request seen-set and the cross-request
// demotion registry; the classifier then reads the set size: below
// unknown_400_confirm_n the walk moves to the next candidate, at the
// ceiling it fail-fasts and surfaces the upstream 400 exactly as before.
// Everything else (recognized 400 subclasses, transport errors, other
// statuses) is decided identically to finalFailureDecision.
func (fc *FallbackChain) failureDecisionWalk(resp *Response, err error, providerID, modelID string, seenUnknown400 map[string]bool) FailureDecision {
	in := finalFailureInput(resp, err)
	if in.StatusCode != http.StatusBadRequest {
		return ClassifyFailure(in)
	}
	// Tools-unsupported 400 (round 9) and model-unsupported 400 (round 12,
	// UnoRouter "Model X:free not supported"): deterministic MODEL defects —
	// the walk moves on regardless of the unknown-400 policy, and the model
	// is parked so the next request's ranking skips it (the unknown-400
	// demotion lesson, without the confirm dance).
	sub := classifyBadRequestSubtype(in.Body)
	if sub == bad400Tools || sub == bad400ModelUnsupported {
		if fc.Demotions != nil && fc.demotionTTL() > 0 {
			fc.Demotions.demote(providerID, modelID, time.Now())
			if sub == bad400ModelUnsupported {
				log.Printf("fallback: %s/%s demoted for %s (model-unsupported)", providerID, modelID, fc.demotionTTL())
			} else {
				log.Printf("fallback: %s/%s demoted for %s (tools-unsupported)", providerID, modelID, fc.demotionTTL())
			}
		}
		return ClassifyFailure(in) // bad400Tools / bad400ModelUnsupported → FallbackNext
	}
	if seenUnknown400 == nil {
		return ClassifyFailure(in)
	}
	if fc.Cfg.Fallback.Unknown400Policy != policyCrossModelConfirm || fc.Cfg.Fallback.Unknown400ConfirmN <= 0 {
		return ClassifyFailure(in)
	}
	if classifyBadRequestSubtype(in.Body) != bad400Unknown {
		return ClassifyFailure(in)
	}
	key := demotionKey(providerID, modelID)
	if !seenUnknown400[key] {
		seenUnknown400[key] = true
		if fc.Demotions != nil && fc.demotionTTL() > 0 {
			fc.Demotions.demote(providerID, modelID, time.Now())
			log.Printf("fallback: %s/%s demoted for %s (unknown-400)", providerID, modelID, fc.demotionTTL())
		}
	}
	in.Policy = policyCrossModelConfirm
	in.ConfirmN = fc.Cfg.Fallback.Unknown400ConfirmN
	in.SeenDistinct = len(seenUnknown400)
	d := ClassifyFailure(in)
	if d == FallbackNext {
		log.Printf("fallback: %s/%s unknown-400 (cross-model confirm %d/%d) — walking to another model", providerID, modelID, in.SeenDistinct, in.ConfirmN)
	}
	return d
}

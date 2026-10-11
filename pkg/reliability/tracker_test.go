package reliability

import "testing"

// TestOutcomePenaltyOrdering locks the task-M ordering: wasted
// (empty-200/timeout) sinks the EMA harder than rate-limited (429/5xx),
// which sinks it harder than a slow success; a plain success keeps the EMA
// at 1.0. Every case burns the 1 request/minute/model slot — the ordering
// says WHICH burn is worse.
func TestOutcomePenaltyOrdering(t *testing.T) {
	cases := []struct {
		name string
		o    Outcome
	}{
		{"success", OutcomeSuccess},
		{"slow-success", OutcomeSlowSuccess},
		{"rate-limited", OutcomeRateLimited},
		{"wasted", OutcomeWasted},
	}
	rates := make(map[string]float64, len(cases))
	for _, c := range cases {
		tr := New()
		tr.RecordOutcome("p", c.o)
		rates[c.name] = tr.SuccessRate("p")
	}
	if rates["success"] != 1.0 {
		t.Fatalf("success must keep the EMA at 1.0, got %v", rates["success"])
	}
	if !(rates["wasted"] < rates["rate-limited"] && rates["rate-limited"] < rates["slow-success"] && rates["slow-success"] < rates["success"]) {
		t.Fatalf("penalty ordering violated: %v", rates)
	}
}

// TestRecordOutcomeCooldownBehavior: repeated wasted attempts trip
// ShouldCooldown fastest; repeated 429/5xx attempts trip it after enough
// observations (their weight asymptote is below the 0.5 line); slow
// successes never do (asymptote 0.8).
func TestRecordOutcomeCooldownBehavior(t *testing.T) {
	tr := New()
	for i := 0; i < 8; i++ {
		tr.RecordOutcome("bad", OutcomeWasted)
	}
	if !tr.ShouldCooldown("bad") {
		t.Fatalf("repeated wasted attempts must trip the reliability cooldown, EMA=%v", tr.SuccessRate("bad"))
	}

	tr2 := New()
	for i := 0; i < 15; i++ {
		tr2.RecordOutcome("rl", OutcomeRateLimited)
	}
	if !tr2.ShouldCooldown("rl") {
		t.Fatalf("sustained 429/5xx attempts must trip the reliability cooldown, EMA=%v", tr2.SuccessRate("rl"))
	}

	tr3 := New()
	for i := 0; i < 6; i++ {
		tr3.RecordOutcome("slow", OutcomeSlowSuccess)
	}
	if tr3.ShouldCooldown("slow") {
		t.Fatal("slow successes must never trip the reliability cooldown")
	}
}

// TestRecordDelegatesToOutcome: the boolean Record keeps its old meaning —
// true = success (EMA stays at the 1.0 seed), false = rate-limited-class
// failure (one observation lands on alpha*0.3 + (1-alpha)*1.0).
func TestRecordDelegatesToOutcome(t *testing.T) {
	tr := New()
	tr.Record("a", true)
	if tr.SuccessRate("a") != 1.0 {
		t.Fatalf("Record(true) must keep the seed EMA, got %v", tr.SuccessRate("a"))
	}
	tr.Record("b", false)
	want := emaAlpha*0.3 + (1-emaAlpha)*defaultSuccessRate
	if tr.SuccessRate("b") != want {
		t.Fatalf("Record(false) must land on the rate-limited weight: got %v want %v", tr.SuccessRate("b"), want)
	}
}

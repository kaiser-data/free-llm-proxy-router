package ratelimit

import (
	"sync"
	"time"
)

// GeminiModelLimits holds the 4-dimensional quota for a single Gemini model.
// Limits are at the Google Cloud project level (shared by all API keys in the project).
type GeminiModelLimits struct {
	RPM int // requests per minute
	TPM int // tokens per minute (input + output)
	RPD int // requests per day (resets midnight PT)
	IPM int // images per minute (0 = not applicable)
}

// GeminiTracker tracks 4-dimensional usage per model.
// All API keys sharing the same GCP project share the same tracker instance.
type GeminiTracker struct {
	mu          sync.Mutex
	modelLimits map[string]GeminiModelLimits

	// per-model minute windows
	rpmCounts map[string]*Window
	tpmCounts map[string]*Window
	ipmCounts map[string]*Window

	// per-model day windows (reset at midnight PT)
	rpdCounts map[string]*Window
	dayStart  time.Time // last midnight PT — used for RPD reset tracking
}

// NewGeminiTracker creates a GeminiTracker with default model limits.
// Limits are provided as a starting point; verify against live docs before deployment.
// Source: https://ai.google.dev/gemini-api/docs/rate-limits (verified 2026-02-28)
func NewGeminiTracker() *GeminiTracker {
	t := &GeminiTracker{
		modelLimits: defaultGeminiLimits(),
		rpmCounts:   make(map[string]*Window),
		tpmCounts:   make(map[string]*Window),
		ipmCounts:   make(map[string]*Window),
		rpdCounts:   make(map[string]*Window),
		dayStart:    midnightPT(),
	}
	return t
}

// SetLimits overrides the limits for a specific model (e.g. from live docs).
func (t *GeminiTracker) SetLimits(modelID string, limits GeminiModelLimits) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.modelLimits[modelID] = limits
}

// CanRequest checks all 4 quota dimensions for the given Gemini model.
// estimatedTokens is the approximate token count for the upcoming request.
func (t *GeminiTracker) CanRequest(modelID string, estimatedTokens int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.maybeResetDay()

	limits, ok := t.modelLimits[modelID]
	if !ok {
		// Unknown model — allow and let the API decide
		return true
	}

	// RPM check
	if limits.RPM > 0 {
		w := t.getOrCreate(t.rpmCounts, modelID, time.Minute)
		if w.Count >= limits.RPM {
			return false
		}
	}
	// TPM check
	if limits.TPM > 0 && estimatedTokens > 0 {
		w := t.getOrCreate(t.tpmCounts, modelID, time.Minute)
		if w.Count+estimatedTokens > limits.TPM {
			return false
		}
	}
	// RPD check
	if limits.RPD > 0 {
		wd := t.getOrCreate(t.rpdCounts, modelID, 24*time.Hour)
		if wd.Count >= limits.RPD {
			return false
		}
	}
	return true
}

// RecordRequest increments counters for a Gemini request that was sent.
func (t *GeminiTracker) RecordRequest(modelID string, tokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.maybeResetDay()

	t.getOrCreate(t.rpmCounts, modelID, time.Minute).Count++
	t.getOrCreate(t.rpdCounts, modelID, 24*time.Hour).Count++
	if tokens > 0 {
		t.getOrCreate(t.tpmCounts, modelID, time.Minute).Count += tokens
	}
}

// maybeResetDay resets daily counters if midnight PT has passed.
// Must be called with t.mu held.
func (t *GeminiTracker) maybeResetDay() {
	nextMidnight := midnightPT()
	if nextMidnight.After(t.dayStart) {
		// A new day has started in Pacific Time
		t.rpdCounts = make(map[string]*Window)
		t.dayStart = nextMidnight
	}
}

// midnightPT returns the most recent midnight in America/Los_Angeles.
func midnightPT() time.Time {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.FixedZone("PST", -8*60*60)
	}
	now := time.Now().In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

func (t *GeminiTracker) getOrCreate(m map[string]*Window, key string, size time.Duration) *Window {
	if w, ok := m[key]; ok {
		// Reset window if expired
		if time.Now().After(w.WindowEnd) {
			w.Count = 0
			w.WindowEnd = time.Now().Add(size)
		}
		return w
	}
	w := &Window{Size: size, WindowEnd: time.Now().Add(size)}
	m[key] = w
	return w
}

// defaultGeminiLimits returns conservative free-tier limits.
// These are verified starting points — run free-llm-scan to get live values.
// Source: https://ai.google.dev/gemini-api/docs/rate-limits (2026-02-28)
// Note: Google cut free tier quotas 50-92% on Dec 7, 2025.
func defaultGeminiLimits() map[string]GeminiModelLimits {
	return map[string]GeminiModelLimits{
		// Gemini 2.0 Flash — fast, capable, moderate free limits
		"gemini-2.0-flash":          {RPM: 10, TPM: 250_000, RPD: 20},
		"gemini-2.0-flash-001":      {RPM: 10, TPM: 250_000, RPD: 20},
		// Gemini 2.0 Flash Lite — highest RPD, preferred for volume
		"gemini-2.0-flash-lite":     {RPM: 15, TPM: 1_000_000, RPD: 1000},
		"gemini-2.0-flash-lite-001": {RPM: 15, TPM: 1_000_000, RPD: 1000},
		// Gemini 1.5 Flash — generous TPM
		"gemini-1.5-flash":          {RPM: 15, TPM: 1_000_000, RPD: 1500},
		"gemini-1.5-flash-8b":       {RPM: 15, TPM: 1_000_000, RPD: 1500},
		// Gemini 2.5 Pro — low limits, high capability
		"gemini-2.5-pro-preview":    {RPM: 5, TPM: 250_000, RPD: 25},
		"gemini-1.5-pro":            {RPM: 2, TPM: 32_000, RPD: 50},
	}
}

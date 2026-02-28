package ratelimit

import (
	"math"
	"math/rand"
	"time"
)

// ExponentialBackoff computes a delay for the given retry attempt (0-indexed).
// It adds 10% jitter to avoid thundering-herd effects.
func ExponentialBackoff(attempt int, base time.Duration, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	exp := math.Pow(2, float64(attempt))
	delay := time.Duration(float64(base) * exp)
	if delay > max {
		delay = max
	}
	// Add up to 10% jitter
	jitterRange := int64(delay / 10)
	if jitterRange > 0 {
		delay += time.Duration(rand.Int63n(jitterRange))
	}
	return delay
}

// DefaultBackoffParams returns the standard backoff settings used across providers.
func DefaultBackoffParams() (base, max time.Duration) {
	return 1 * time.Second, 60 * time.Second
}

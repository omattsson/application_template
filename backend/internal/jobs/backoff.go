package jobs

import "time"

// jitterFraction is the share of a delay that jitter can take off.
const jitterFraction = 0.2

// Backoff returns the delay before the attempt after attempt number attempt
// (1 for the first failure): min(base * 2^(attempt-1), max), shortened by up
// to 20% (jitter is a value from 0 to 1) so that jobs that failed together
// do not retry together. The jitter only shortens the delay, so max stays a
// bound. An attempt below 1 counts as 1 and a max below base as base.
func Backoff(attempt int, base, maxDelay time.Duration, jitter float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	if base <= 0 {
		return 0
	}

	maxDelay = max(maxDelay, base)

	d := base
	for i := 1; i < attempt; i++ {
		// Doubling stops at the cap, so the value cannot overflow.
		if d >= maxDelay/2 {
			d = maxDelay
			break
		}

		d *= 2
	}

	d = min(d, maxDelay)
	jitter = min(max(jitter, 0), 1)

	return d - time.Duration(float64(d)*jitterFraction*jitter)
}

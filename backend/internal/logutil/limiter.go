// Package logutil holds small helpers for logging.
package logutil

import (
	"sync"
	"time"
)

// DefaultInterval is the interval of a Limiter that was not given one.
const DefaultInterval = time.Minute

// Limiter lets one log line through per interval. A failure that repeats on
// every request (a database that is down, a full queue) would otherwise write
// a line per request. The caller logs the line that passes at its normal
// level and the others at Debug level, and counts every event in a metric, so
// the limit hides nothing from the metrics.
//
// The zero value is ready to use with DefaultInterval. A Limiter is safe for
// concurrent use and must not be copied after first use.
type Limiter struct {
	last       time.Time
	now        func() time.Time
	interval   time.Duration
	suppressed int
	mu         sync.Mutex
}

// NewLimiter returns a Limiter that lets one call through per interval. An
// interval of 0 or less means DefaultInterval.
func NewLimiter(interval time.Duration) *Limiter {
	return &Limiter{interval: interval}
}

// NewLimiterWithClock is NewLimiter with its own clock (tests).
func NewLimiterWithClock(interval time.Duration, now func() time.Time) *Limiter {
	return &Limiter{interval: interval, now: now}
}

// Allow reports whether the caller may log. When it may, suppressed is the
// number of calls that Allow refused since the last call it allowed, so the
// line can say how many were left out.
func (l *Limiter) Allow() (ok bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	interval := l.interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	now := time.Now
	if l.now != nil {
		now = l.now
	}

	t := now()
	if !l.last.IsZero() && t.Sub(l.last) < interval {
		l.suppressed++
		return false, 0
	}

	suppressed = l.suppressed
	l.suppressed = 0
	l.last = t

	return true, suppressed
}

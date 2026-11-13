package logutil_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/logutil"

	"github.com/stretchr/testify/assert"
)

func TestLimiter_AllowsOnePerInterval(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	l := logutil.NewLimiterWithClock(time.Minute, func() time.Time { return now })

	ok, suppressed := l.Allow()
	assert.True(t, ok, "the first call passes")
	assert.Zero(t, suppressed)

	for i := 0; i < 5; i++ {
		ok, _ = l.Allow()
		assert.False(t, ok, "calls inside the interval are refused")
	}

	now = now.Add(59 * time.Second)
	ok, _ = l.Allow()
	assert.False(t, ok, "still inside the interval")

	now = now.Add(time.Second)
	ok, suppressed = l.Allow()
	assert.True(t, ok, "the interval has passed")
	assert.Equal(t, 6, suppressed, "the call reports how many were refused since the last one that passed")

	now = now.Add(time.Minute)
	ok, suppressed = l.Allow()
	assert.True(t, ok)
	assert.Zero(t, suppressed, "the count starts again")
}

func TestLimiter_ZeroValueUsesTheDefaultInterval(t *testing.T) {
	t.Parallel()

	var l logutil.Limiter
	ok, _ := l.Allow()
	assert.True(t, ok)
	ok, _ = l.Allow()
	assert.False(t, ok, "a zero Limiter still limits")
}

func TestLimiter_SafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	l := logutil.NewLimiter(time.Hour)
	var passed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow(); ok {
				passed.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, passed.Load(), "exactly one of the concurrent calls passes")
}

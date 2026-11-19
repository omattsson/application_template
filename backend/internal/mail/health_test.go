package mail

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthCheckCachesTheResult(t *testing.T) {
	t.Parallel()

	var (
		calls atomic.Int32
		clock = time.Unix(1000, 0)
		mu    sync.Mutex
		fail  atomic.Bool
	)

	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()

		clock = clock.Add(d)
	}

	check := NewHealthCheck(func(context.Context) error {
		calls.Add(1)

		if fail.Load() {
			return errors.New("dial tcp smtp.internal.example.com:587: connection refused (user mailer)")
		}

		return nil
	}, 30*time.Second, time.Second, now)

	require.NoError(t, check(context.Background()))
	require.NoError(t, check(context.Background()))
	assert.Equal(t, int32(1), calls.Load(), "the second call within the TTL reuses the result")

	fail.Store(true)
	require.NoError(t, check(context.Background()), "a cached success still holds")

	advance(31 * time.Second)

	err := check(context.Background())
	require.ErrorIs(t, err, ErrUnavailable)
	assert.NotContains(t, err.Error(), "smtp.internal.example.com", "the public answer holds no detail")
	assert.Equal(t, int32(2), calls.Load())

	require.ErrorIs(t, check(context.Background()), ErrUnavailable, "a failure is cached too")
	assert.Equal(t, int32(2), calls.Load())

	fail.Store(false)
	advance(31 * time.Second)
	require.NoError(t, check(context.Background()), "it recovers after the TTL")
	assert.Equal(t, int32(3), calls.Load())
}

func TestHealthCheckProbeIsBoundedAndSurvivesACancelledCaller(t *testing.T) {
	t.Parallel()

	check := NewHealthCheck(func(ctx context.Context) error {
		<-ctx.Done()

		return ctx.Err()
	}, time.Minute, 100*time.Millisecond, nil)

	start := time.Now()
	require.ErrorIs(t, check(context.Background()), ErrUnavailable)
	assert.Less(t, time.Since(start), 3*time.Second, "the probe has its own deadline")

	var probeCancelled atomic.Bool

	check = NewHealthCheck(func(ctx context.Context) error {
		probeCancelled.Store(ctx.Err() != nil)

		return nil
	}, time.Minute, time.Second, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, check(ctx), "the caller's cancellation is not the result of the probe")
	assert.False(t, probeCancelled.Load())
}

func TestHealthCheckConcurrentCallersShareOneProbe(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	check := NewHealthCheck(func(context.Context) error {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)

		return nil
	}, time.Minute, time.Second, nil)

	var wg sync.WaitGroup

	for range 10 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			assert.NoError(t, check(context.Background()))
		}()
	}

	wg.Wait()
	assert.Equal(t, int32(1), calls.Load())
}

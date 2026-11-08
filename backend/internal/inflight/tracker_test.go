package inflight

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackerWaitsForStartedWork(t *testing.T) {
	t.Parallel()
	tr := New()

	release := make(chan struct{})
	var finished atomic.Bool
	require.True(t, tr.Go(func() {
		<-release
		finished.Store(true)
	}))

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tr.Wait(ctx))
	assert.True(t, finished.Load(), "Wait returned before the work finished")
}

func TestTrackerWaitReturnsAtDeadlineForHungWork(t *testing.T) {
	t.Parallel()
	tr := New()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	require.True(t, tr.Go(func() { <-release }))

	const budget = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	err := tr.Wait(ctx)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, elapsed, budget-10*time.Millisecond)
	assert.Less(t, elapsed, 2*time.Second)
}

func TestTrackerRejectsWorkAfterWaitStarted(t *testing.T) {
	t.Parallel()
	tr := New()

	require.NoError(t, tr.Wait(context.Background()))

	var ran atomic.Bool
	assert.False(t, tr.Go(func() { ran.Store(true) }), "Go after Wait must be rejected")
	time.Sleep(20 * time.Millisecond)
	assert.False(t, ran.Load(), "rejected work must not run")
}

// TestTrackerConcurrentGoAndWait races Go against Wait (run with -race):
// every accepted job finishes before Wait returns, and no Add happens
// after Wait began.
func TestTrackerConcurrentGoAndWait(t *testing.T) {
	t.Parallel()
	tr := New()

	var accepted, finished atomic.Int32
	var starters sync.WaitGroup
	for i := 0; i < 50; i++ {
		starters.Add(1)
		go func() {
			defer starters.Done()
			if tr.Go(func() {
				time.Sleep(time.Millisecond)
				finished.Add(1)
			}) {
				accepted.Add(1)
			}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tr.Wait(ctx))
	got := finished.Load()
	starters.Wait()
	assert.Equal(t, accepted.Load(), got, "Wait returned before an accepted job finished")
}

func TestTrackerRecoversPanic(t *testing.T) {
	// Not parallel: it replaces the default slog logger.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	var mu sync.Mutex
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))

	tr := New()
	require.True(t, tr.Go(func() { panic("boom") }))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tr.Wait(ctx), "a panicking job still counts as done")

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, buf.String(), "In-flight work panicked")
	assert.Contains(t, buf.String(), "boom")
}

func TestTrackerZeroValueAndNoWork(t *testing.T) {
	t.Parallel()
	var tr Tracker
	assert.NoError(t, tr.Wait(context.Background()))
}

func TestTrackerNilReceiver(t *testing.T) {
	t.Parallel()
	var tr *Tracker

	var ran atomic.Bool
	assert.NotPanics(t, func() {
		assert.False(t, tr.Go(func() { ran.Store(true) }), "a nil tracker accepts no work")
	})
	assert.False(t, ran.Load(), "rejected work must not run")
	assert.NotPanics(t, func() {
		assert.NoError(t, tr.Wait(context.Background()), "a nil tracker has nothing to wait for")
	})
}

// panicSiteForTest panics in a named function, so the log can name it.
func panicSiteForTest() {
	var m map[string]int
	m["x"] = 1 // runtime error: assignment to entry in nil map
}

func TestTrackerPanicLogNamesThePanicSite(t *testing.T) {
	// Not parallel: it replaces the default slog logger.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	var mu sync.Mutex
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))

	tr := New()
	require.True(t, tr.Go(panicSiteForTest))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tr.Wait(ctx))

	mu.Lock()
	defer mu.Unlock()
	out := buf.String()
	assert.Contains(t, out, "panic_site=")
	assert.Regexp(t, `inflight\.panicSiteForTest:\d+`, out, "the top frame is the function that panicked")
	assert.NotContains(t, out, "runtime.gopanic", "runtime frames are skipped")
	assert.NotContains(t, out, "goroutine ", "only the top frames, not a full goroutine dump")
	assert.Equal(t, 1, strings.Count(out, "\n"), "one log line")
}

func TestPanicSiteFrames(t *testing.T) {
	t.Parallel()

	var got []string
	func() {
		defer func() {
			_ = recover()
			got = panicSite(3)
		}()
		panicSiteForTest()
	}()

	require.NotEmpty(t, got)
	assert.LessOrEqual(t, len(got), 3)
	assert.Regexp(t, `inflight\.panicSiteForTest:\d+$`, got[0])

	assert.Empty(t, panicSite(0))
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

package leader

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder counts worker starts and stops.
type recorder struct {
	mu      sync.Mutex
	starts  int
	stops   int
	running atomic.Int32
}

func (r *recorder) worker() Worker {
	return Worker{Name: "test", Run: func(ctx context.Context) {
		r.running.Add(1)
		r.mu.Lock()
		r.starts++
		r.mu.Unlock()
		<-ctx.Done()
		r.mu.Lock()
		r.stops++
		r.mu.Unlock()
		r.running.Add(-1)
	}}
}

func (r *recorder) counts() (starts, stops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts, r.stops
}

func TestGroup_StartStopStartAgain(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	g := NewGroup(time.Second, rec.worker())
	g.Add(rec.worker())
	assert.Equal(t, []string{"test", "test"}, g.Names())

	for term := 1; term <= 3; term++ {
		g.Start(context.Background())
		require.Eventually(t, func() bool { return rec.running.Load() == 2 }, time.Second, 5*time.Millisecond)
		assert.True(t, g.Running())

		// A second Start in the same term does nothing.
		g.Start(context.Background())
		time.Sleep(20 * time.Millisecond)
		assert.Equal(t, int32(2), rec.running.Load())

		require.True(t, g.Stop())
		assert.Equal(t, int32(0), rec.running.Load())
		assert.False(t, g.Running())
	}
	starts, stops := rec.counts()
	assert.Equal(t, 6, starts)
	assert.Equal(t, 6, stops)
}

func TestGroup_StartsEachWorkerOnce(t *testing.T) {
	t.Parallel()

	a, b := &recorder{}, &recorder{}
	g := NewGroup(time.Second, a.worker(), b.worker())
	g.Start(context.Background())
	g.Start(context.Background()) // no-op
	require.Eventually(t, func() bool { return a.running.Load() == 1 && b.running.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.True(t, g.Stop())

	aStarts, _ := a.counts()
	bStarts, _ := b.counts()
	assert.Equal(t, 1, aStarts)
	assert.Equal(t, 1, bStarts)
}

func TestGroup_StopCancelsAndWaits(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	var finished atomic.Bool
	w := Worker{Name: "slowstop", Run: func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	}}
	g := NewGroup(time.Second, w)
	g.Start(context.Background())
	<-started

	require.True(t, g.Stop())
	assert.True(t, finished.Load(), "Stop returned before the worker finished")
}

func TestGroup_CloseReturnsFalseWhenWorkerIgnoresCancel(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})
	stuck := Worker{Name: "stuck", Run: func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		<-release // ignores the cancel until the test releases it
	}}
	g := NewGroup(time.Second, stuck)
	g.Start(context.Background())
	<-started

	assert.False(t, g.Close(30*time.Millisecond), "Close must report the timeout")
	assert.False(t, g.Running())
	close(release)
}

func TestGroup_StopWithoutStart(t *testing.T) {
	t.Parallel()

	g := NewGroup(0)
	assert.True(t, g.Stop())
	assert.True(t, g.Close(0))
}

func TestGroup_CloseBlocksLaterStart(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	g := NewGroup(time.Second, rec.worker())
	g.Start(context.Background())
	require.Eventually(t, func() bool { return rec.running.Load() == 1 }, time.Second, 5*time.Millisecond)

	require.True(t, g.Close(time.Second))
	g.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), rec.running.Load())
	starts, _ := rec.counts()
	assert.Equal(t, 1, starts)
}

func TestGroup_ParentContextEndsTerm(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	g := NewGroup(time.Second, rec.worker())
	ctx, cancel := context.WithCancel(context.Background())
	g.Start(ctx)
	require.Eventually(t, func() bool { return rec.running.Load() == 1 }, time.Second, 5*time.Millisecond)
	cancel()
	require.Eventually(t, func() bool { return rec.running.Load() == 0 }, time.Second, 5*time.Millisecond)
	assert.True(t, g.Stop())
}

func TestGroup_SlowWorkerDelaysNextTerm(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var running, maxRunning atomic.Int32
	slow := Worker{Name: "slow", Run: func(ctx context.Context) {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		<-ctx.Done()
		<-release // ignores the stop request until the test releases it
		running.Add(-1)
	}}
	g := NewGroup(50*time.Millisecond, slow)

	g.Start(context.Background())
	require.Eventually(t, func() bool { return running.Load() == 1 }, time.Second, 5*time.Millisecond)
	assert.False(t, g.Stop(), "Stop must report the timeout")

	// The next term must not start while the old worker still runs.
	g.Start(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), running.Load())

	release <- struct{}{} // old worker returns
	require.Eventually(t, func() bool { return running.Load() == 1 && maxRunning.Load() == 1 },
		time.Second, 5*time.Millisecond)
	close(release)
	assert.True(t, g.Stop())
	assert.Equal(t, int32(1), maxRunning.Load(), "two terms ran at the same time")
}

func TestGroup_RepeatedLossesNeverRunTwoCopies(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var running, starts atomic.Int32
	slow := Worker{Name: "slow", Run: func(ctx context.Context) {
		starts.Add(1)
		running.Add(1)
		defer running.Add(-1)
		<-ctx.Done()
		<-release // the first term's worker ignores the stop request
	}}
	g := NewGroup(20*time.Millisecond, slow)

	// Term 1 starts; its worker does not stop in time.
	g.Start(context.Background())
	require.Eventually(t, func() bool { return running.Load() == 1 }, time.Second, 5*time.Millisecond)
	require.False(t, g.Stop())

	// Two quick stops: term 2 starts and stops while term 1 still runs.
	g.Start(context.Background())
	g.Stop()

	// Term 3 must wait for term 1, not only for term 2.
	g.Start(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), starts.Load(), "term 3 started its workers while term 1 still runs")
	assert.Equal(t, int32(1), running.Load())

	close(release)
	require.Eventually(t, func() bool { return starts.Load() == 2 }, time.Second, 5*time.Millisecond)
	assert.True(t, g.Stop())
	assert.Equal(t, int32(0), running.Load())
}

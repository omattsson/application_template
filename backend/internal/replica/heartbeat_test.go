package replica

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepo records the heartbeat writes.
type fakeRepo struct {
	removeCtx context.Context
	beatErr   error
	beats     map[string]int
	rows      map[string]bool
	beat      chan struct{}
	removed   []string
	mu        sync.Mutex
	beatHang  bool // Beat blocks until its ctx ends
	// removeHang makes Remove block until its ctx ends.
	removeHang bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{beats: map[string]int{}, rows: map[string]bool{}, beat: make(chan struct{}, 100)}
}

func (f *fakeRepo) Beat(ctx context.Context, id string) error {
	f.mu.Lock()
	hang, err := f.beatHang, f.beatErr
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	f.beats[id]++
	if err == nil {
		f.rows[id] = true
	}
	f.mu.Unlock()
	select {
	case f.beat <- struct{}{}:
	default:
	}
	return err
}

func (f *fakeRepo) Remove(ctx context.Context, id string) error {
	f.mu.Lock()
	hang := f.removeHang
	f.removeCtx = ctx
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, id)
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeRepo) DeleteOlderThan(context.Context, time.Duration) (int64, error) {
	return 0, nil
}

func (f *fakeRepo) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats[id]
}

func (f *fakeRepo) waitBeats(t *testing.T, id string, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for f.count(id) < n {
		select {
		case <-f.beat:
		case <-deadline:
			t.Fatalf("got %d beats, want %d", f.count(id), n)
		}
	}
}

func TestProcessID(t *testing.T) {
	t.Parallel()
	a, b := ProcessID("host"), ProcessID("host")
	assert.True(t, strings.HasPrefix(a, "host-"))
	assert.Len(t, a, len("host-")+8)
	assert.NotEqual(t, a, b, "two processes on one host get different IDs")
	assert.True(t, strings.HasPrefix(ProcessID(""), fallbackIdentity+"-"))
	long := ProcessID(strings.Repeat("x", 400))
	assert.LessOrEqual(t, len(long), maxProcessIDLen)
}

func TestNewHeartbeatNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, NewHeartbeat(nil, "id", time.Second))
	assert.Nil(t, NewHeartbeat(newFakeRepo(), "", time.Second))
	var h *Heartbeat
	h.Start()
	h.Stop(time.Second)
	assert.Empty(t, h.ID())
	assert.Equal(t, HeartbeatInterval, NewHeartbeat(newFakeRepo(), "id", 0).interval)
}

// The heartbeat writes at once and then every interval, and Stop deletes
// the row.
func TestHeartbeatBeatsAndRemoves(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := NewHeartbeat(repo, "p1", 20*time.Millisecond)
	assert.Equal(t, "p1", h.ID())
	h.Start()
	h.Start() // a second Start does nothing
	repo.waitBeats(t, "p1", 4)

	h.Stop(time.Second)
	repo.mu.Lock()
	assert.Equal(t, []string{"p1"}, repo.removed)
	assert.False(t, repo.rows["p1"], "the row is deleted")
	repo.mu.Unlock()

	// No beat after Stop.
	n := repo.count("p1")
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, n, repo.count("p1"))

	// Stop is idempotent: no second delete.
	h.Stop(time.Second)
	repo.mu.Lock()
	assert.Len(t, repo.removed, 1)
	repo.mu.Unlock()
}

// A failing write is logged and the heartbeat keeps trying; it recovers.
func TestHeartbeatFailureDoesNotStop(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.beatErr = errors.New("db down")
	h := NewHeartbeat(repo, "p1", 10*time.Millisecond)
	h.Start()
	repo.waitBeats(t, "p1", 3)
	repo.mu.Lock()
	repo.beatErr = nil
	n := repo.beats["p1"]
	repo.mu.Unlock()
	repo.waitBeats(t, "p1", n+2)
	repo.mu.Lock()
	assert.True(t, repo.rows["p1"])
	repo.mu.Unlock()
	h.Stop(time.Second)
}

// Stop returns within its timeout when the write and the delete hang.
func TestHeartbeatStopIsBounded(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.beatHang = true
	repo.removeHang = true
	h := NewHeartbeat(repo, "p1", time.Hour)
	h.Start()
	time.Sleep(20 * time.Millisecond) // the first beat hangs
	start := time.Now()
	h.Stop(150 * time.Millisecond)
	assert.Less(t, time.Since(start), time.Second)
	repo.mu.Lock()
	_, hasDeadline := repo.removeCtx.Deadline()
	repo.mu.Unlock()
	assert.True(t, hasDeadline, "the delete has a deadline")
}

// Start after Stop starts a new heartbeat.
func TestHeartbeatRestart(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := NewHeartbeat(repo, "p1", time.Hour)
	h.Start()
	repo.waitBeats(t, "p1", 1)
	h.Stop(time.Second)
	h.Start()
	repo.waitBeats(t, "p1", 2)
	h.Stop(time.Second)
	repo.mu.Lock()
	assert.Len(t, repo.removed, 2)
	repo.mu.Unlock()
}

// Concurrent Start and Stop calls are safe (run with -race).
func TestHeartbeatConcurrentStartStop(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := NewHeartbeat(repo, "p1", 5*time.Millisecond)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); h.Start() }()
		go func() { defer wg.Done(); h.Stop(time.Second) }()
	}
	wg.Wait()
	h.Stop(time.Second)
	require.NotNil(t, h)
}

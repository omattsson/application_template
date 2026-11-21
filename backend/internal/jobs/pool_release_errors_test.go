package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Release that fails at shutdown must not stop the shutdown: Stop still
// returns within its budgets, reports the stuck handler, and the job stays
// running until its lease ends (another pool claims it then).
func TestPoolStopWithAFailingReleaseStillReturns(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.releaseErr = errors.New("db down")
	started, unblock := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	p := startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error {
		close(started)
		<-unblock
		return nil
	}}, func(c *PoolConfig) {
		c.DrainTimeout, c.ReleaseTimeout, c.VisibilityTimeout = 30*time.Millisecond, 200*time.Millisecond, time.Minute
	})
	id := repo.add("a", 3, 0)
	<-started

	start := time.Now()
	err := p.Stop(context.Background())
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "Stop is bounded")

	repo.mu.Lock()
	released := append([]uint64(nil), repo.releaseCalls...)
	repo.mu.Unlock()
	assert.Contains(t, released, id, "the release was tried")
	assert.Equal(t, models.JobStatusRunning, repo.status(id), "the lease ends later")
}

// releaseRunning skips a job that was taken over (Release answers false) and
// goes on with the others.
func TestPoolReleaseRunningSkipsAFencedJob(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	reg := NewRegistry()
	reg.Register("a", func(context.Context, Job) error { return nil })
	p := NewPool(PoolConfig{Repo: repo, Registry: reg, Workers: 2, PollInterval: time.Hour, VisibilityTimeout: time.Minute})
	taken := repo.add("a", 3, 0)
	mine := repo.add("a", 3, 0)
	c, err := repo.Claim(context.Background(), []string{p.workerID(0), p.workerID(1)}, []string{"a"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 2)

	// Another worker took the first job over.
	repo.mu.Lock()
	repo.jobs[taken].LockedBy = "thief#1"
	repo.mu.Unlock()
	run := &poolRun{running: map[uint64]string{taken: p.workerID(0), mine: p.workerID(1)}}

	p.releaseRunning(context.Background(), run)

	assert.Equal(t, models.JobStatusRunning, repo.status(taken), "the thief's job is left alone")
	assert.Equal(t, "thief#1", repo.get(taken).LockedBy)
	assert.Equal(t, models.JobStatusPending, repo.status(mine))
}

// A job that is claimed while Stop begins and cannot be released does not
// block the dispatcher: the worker slot is free again.
func TestPoolHandOutKeepsTheSlotWhenTheReleaseFails(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.releaseErr = errors.New("db down")
	reg := NewRegistry()
	reg.Register("a", func(context.Context, Job) error { return nil })
	p := NewPool(PoolConfig{Repo: repo, Registry: reg, Workers: 1, PollInterval: time.Hour, VisibilityTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := &poolRun{claimCtx: ctx, idle: make(chan int, 1), work: []chan task{make(chan task, 1)}, running: map[uint64]string{}}
	id := repo.add("a", 3, 0)
	claim, err := repo.Claim(context.Background(), []string{p.workerID(0)}, []string{"a"}, time.Minute)
	require.NoError(t, err)

	p.handOut(run, []int{0}, claim.Jobs)

	assert.Equal(t, models.JobStatusRunning, repo.status(id), "left to its lease")
	assert.Len(t, run.idle, 1)
}

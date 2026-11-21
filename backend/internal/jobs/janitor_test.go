package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupDeletesBothStatusesWithTheirRetention(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	require.NoError(t, Cleanup(repo, time.Hour, 24*time.Hour)(context.Background()))
	assert.Equal(t, []deleteCall{
		{models.JobStatusSucceeded, time.Hour, 1000},
		{models.JobStatusDead, 24 * time.Hour, 1000},
	}, repo.deleted)
}

func TestCleanupLoopsUntilABatchIsShort(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	left := map[string]int64{models.JobStatusSucceeded: 2500, models.JobStatusDead: 10}
	repo.deleteFn = func(status string, limit int) (int64, error) {
		n := min(left[status], int64(limit))
		left[status] -= n
		return n, nil
	}
	require.NoError(t, Cleanup(repo, time.Hour, 2*time.Hour)(context.Background()))
	assert.Len(t, repo.deleted, 4, "three batches of succeeded (1000, 1000, 500), one of dead")
	assert.Zero(t, left[models.JobStatusSucceeded])
	assert.Zero(t, left[models.JobStatusDead])
}

func TestCleanupBoundsTheBatchesOfOneRun(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.deleteFn = func(string, int) (int64, error) { return cleanupBatch, nil }
	require.NoError(t, Cleanup(repo, time.Hour, 2*time.Hour)(context.Background()))
	assert.Len(t, repo.deleted, 2*cleanupMaxBatches, "a run is bounded for every status")
}

func TestCleanupReturnsTheErrorAndStops(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	boom := errors.New("db down")
	repo.deleteFn = func(string, int) (int64, error) { return 0, boom }
	assert.ErrorIs(t, Cleanup(repo, time.Hour, time.Hour)(context.Background()), boom)
	assert.Len(t, repo.deleted, 1, "the dead pass does not run after a failure")
}

func TestCleanupStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	ctx, cancel := context.WithCancel(context.Background())
	repo.deleteFn = func(string, int) (int64, error) {
		cancel()
		return cleanupBatch, nil
	}
	err := Cleanup(repo, time.Hour, time.Hour)(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Len(t, repo.deleted, 1)
}

func TestCleanupWorker(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	var runs atomic.Int32
	repo.deleteFn = func(status string, _ int) (int64, error) {
		if status == models.JobStatusSucceeded {
			runs.Add(1)
		}
		return 0, nil
	}
	w := NewCleanupWorker(CleanupConfig{Repo: repo, Retention: time.Hour, DeadRetention: 2 * time.Hour, Interval: 20 * time.Millisecond})
	assert.Equal(t, "jobs-cleanup", w.Name)

	// Run twice: a leader worker is restartable.
	for range 2 {
		runs.Store(0)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); w.Run(ctx) }()
		require.Eventually(t, func() bool { return runs.Load() >= 3 }, waitFor, tick, "a run at the start, then one per interval")
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("the worker did not return after the cancel")
		}
	}
}

func TestCleanupWorkerSurvivesAFailedRun(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	var calls atomic.Int32
	repo.deleteFn = func(string, int) (int64, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("db down")
		}
		return 0, nil
	}
	w := NewCleanupWorker(CleanupConfig{Repo: repo, Retention: time.Hour, DeadRetention: time.Hour, Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	require.Eventually(t, func() bool { return calls.Load() >= 3 }, waitFor, tick)
}

func TestCleanupWorkerDefaultInterval(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	w := NewCleanupWorker(CleanupConfig{Repo: repo, Retention: time.Hour, DeadRetention: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	require.Eventually(t, func() bool { repo.mu.Lock(); defer repo.mu.Unlock(); return len(repo.deleted) == 2 }, waitFor, tick)
	cancel()
	<-done
	assert.Equal(t, DefaultCleanupInterval, time.Minute)
}

package jobs

import (
	"context"
	"log/slog"
	"time"

	"backend/internal/leader"
	"backend/internal/models"
)

// Settings of the cleanup worker.
const (
	// CleanupWorkerName is the name of the leader worker.
	CleanupWorkerName = "jobs-cleanup"
	// DefaultCleanupInterval is the time between two cleanup runs.
	DefaultCleanupInterval = time.Minute
	// cleanupBatch is the number of rows of one delete.
	cleanupBatch = 1000
	// cleanupMaxBatches bounds the deletes of one run per status, so a run
	// that has a lot to delete still returns and leaves room for the next.
	cleanupMaxBatches = 100
)

// CleanupConfig configures the cleanup worker.
type CleanupConfig struct {
	// Repo is the job repository.
	Repo models.JobRepository
	// Retention is how long a succeeded job stays (JOBS_RETENTION).
	Retention time.Duration
	// DeadRetention is how long a dead job stays (JOBS_DEAD_RETENTION).
	DeadRetention time.Duration
	// Interval is the time between two runs; zero means DefaultCleanupInterval.
	Interval time.Duration
}

// Cleanup returns the function of one cleanup run: it deletes the succeeded
// jobs older than retention and the dead jobs older than deadRetention, in
// batches of 1000, at most 100 batches per status, and stops at once when ctx
// ends (so the worker returns within 5 s of a cancel). A short batch means that
// nothing old is left. It is safe on several replicas: a delete by primary key
// that matches no row is not an error. Pending and running jobs are never
// deleted.
func Cleanup(repo models.JobRepository, retention, deadRetention time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		for _, p := range []struct {
			status string
			age    time.Duration
		}{
			{models.JobStatusSucceeded, retention},
			{models.JobStatusDead, deadRetention},
		} {
			total := int64(0)

			for range cleanupMaxBatches {
				if err := ctx.Err(); err != nil {
					return err
				}

				n, err := repo.DeleteFinishedBefore(ctx, p.status, p.age, cleanupBatch)
				total += n

				if err != nil {
					return err
				}

				if n < cleanupBatch {
					break
				}
			}

			if total > 0 {
				slog.Info("Deleted finished jobs", "status", p.status, "count", total)
			}
		}

		return nil
	}
}

// NewCleanupWorker returns the leader worker "jobs-cleanup": it runs Cleanup at
// the start of each leadership term and then every interval, with the worker
// context (the end of the term stops a running call). It is restartable: Run
// keeps no state. A failed run is logged and tried again at the next interval;
// an error after the context ended (shutdown, lost leadership) is not logged.
func NewCleanupWorker(cfg CleanupConfig) leader.Worker {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultCleanupInterval
	}

	fn := Cleanup(cfg.Repo, cfg.Retention, cfg.DeadRetention)

	run := func(ctx context.Context) {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			slog.Error("Leader worker run failed", "worker", CleanupWorkerName, "error", err)
		}
	}

	return leader.Worker{Name: CleanupWorkerName, Run: func(ctx context.Context) {
		run(ctx)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run(ctx)
			}
		}
	}}
}

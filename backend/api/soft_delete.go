package main

import (
	"context"
	"log/slog"
	"time"

	"backend/internal/leader"
	"backend/internal/models"
)

// The size of the purge of a soft delete repository.
const (
	// purgeBatch is the number of rows of one PurgeDeleted call.
	purgeBatch = 1000
	// purgeMaxBatches bounds the calls of one run, so a large backlog does not
	// hold the worker: the next tick continues.
	purgeMaxBatches = 100
)

// purgeWorker returns the leader worker that removes the rows of a soft
// delete repository (models.WithSoftDelete) that were deleted more than
// retention ago, every interval. It is not registered in buildServices:
// nothing in the template deletes softly. An application that adds a soft
// delete entity appends it to leaderWorkers (EXTENDING.md, "Adding soft
// delete to an entity"):
//
//	if repo.SoftDeleteEnabled() { // repo is the *models.GenericRepository
//	    leaderWorkers = append(leaderWorkers, purgeWorker("note-purge", time.Hour, 30*24*time.Hour, repo))
//	}
//
// Check SoftDeleteEnabled, not a type assertion to models.SoftDeleter:
// GenericRepository always implements the interface, and without the option
// PurgeDeleted returns ErrNotImplemented on every run.
//
// The run is idempotent and safe on several replicas: a second call finds the
// rows gone. The cutoff is computed from the clock of this process, which is
// fine for a retention of hours or days.
func purgeWorker(name string, interval, retention time.Duration, sd models.SoftDeleter) leader.Worker {
	return periodicWorker(name, interval, purgeJob(sd, retention, purgeBatch, purgeMaxBatches))
}

// purgeJob returns the function of a purge worker: it calls PurgeDeleted in
// batches of batch rows until a batch is short (nothing older is left) or
// maxBatches were made, and stops at once when ctx ends, so the worker returns
// within its 5 s after a cancel (the call itself is one bounded statement).
func purgeJob(sd models.SoftDeleter, retention time.Duration, batch, maxBatches int) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		cutoff := time.Now().Add(-retention)
		total := int64(0)
		for range maxBatches {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, err := sd.PurgeDeleted(ctx, cutoff, batch)
			total += n
			if err != nil {
				return err
			}
			if n < int64(batch) {
				break
			}
		}
		if total > 0 {
			slog.Info("Purged soft deleted rows", "count", total)
		}
		return nil
	}
}

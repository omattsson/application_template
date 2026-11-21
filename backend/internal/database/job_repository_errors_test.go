package database

import (
	"context"
	"database/sql/driver"
	"strconv"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failOn makes the statements that match a SQLite trigger condition fail, to
// reach the error branches that a healthy database never shows.
func failOn(t *testing.T, r *GORMJobRepository, event, when string) {
	t.Helper()
	require.NoError(t, r.DB.Exec("CREATE TRIGGER fail_"+event+" BEFORE "+event+" ON jobs WHEN "+when+
		" BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error)
}

// TestJobClaimIsAllOrNothing: when the write for the second claimed job fails,
// the transaction rolls back, so the first job is not left running with a
// lease that nobody holds.
func TestJobClaimIsAllOrNothing(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	first := enqueue(t, r, "mail", 0)
	second := enqueue(t, r, "mail", 0)
	failOn(t, r, "UPDATE", "NEW.status = 'running' AND NEW.id = "+strconv.FormatUint(second, 10))

	_, err := r.Claim(context.Background(), []string{"w1", "w2"}, []string{"mail"}, time.Minute)
	require.Error(t, err)

	for _, id := range []uint64{first, second} {
		row := readJob(t, db, id)
		assert.Equal(t, models.JobStatusPending, row.Status, "job %d", id)
		assert.Zero(t, row.Attempts, "job %d", id)
		assert.Empty(t, row.LockedBy, "job %d", id)
	}
}

// TestJobClaimFailsWhenTheDeadMarkFails: an error while it marks an exhausted
// job dead stops the claim and leaves the row as it was.
func TestJobClaimFailsWhenTheDeadMarkFails(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	id, err := r.Enqueue(context.Background(), models.JobSpec{Type: "mail", Payload: []byte(`{}`), MaxAttempts: 1})
	require.NoError(t, err)
	require.Len(t, claim(t, r, 1, "mail").Jobs, 1)
	setTime(t, db, id, "locked_until", "-1 seconds")
	failOn(t, r, "UPDATE", "NEW.status = 'dead'")

	_, err = r.Claim(context.Background(), []string{"w1"}, []string{"mail"}, time.Minute)
	require.Error(t, err)
	assert.Equal(t, models.JobStatusRunning, readJob(t, db, id).Status)
}

// TestJobFencedWriteErrorIsAnErrorNotAFence: a database error on Complete,
// Fail, Release or Extend is returned as an error, never as "the job was taken
// over" (false, nil): the pool treats the two differently.
func TestJobFencedWriteErrorIsAnErrorNotAFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	calls := map[string]func(r *GORMJobRepository, id uint64) (bool, error){
		"complete": func(r *GORMJobRepository, id uint64) (bool, error) { return r.Complete(ctx, id, "w1") },
		"extend":   func(r *GORMJobRepository, id uint64) (bool, error) { return r.Extend(ctx, id, "w1", time.Minute) },
		"fail": func(r *GORMJobRepository, id uint64) (bool, error) {
			return r.Fail(ctx, id, "w1", "x", time.Second, false)
		},
		"release": func(r *GORMJobRepository, id uint64) (bool, error) { return r.Release(ctx, id, "w1") },
	}
	for name, call := range calls {
		call := call
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, _ := newJobRepo(t)
			id := enqueue(t, r, "mail", 0)
			require.Len(t, claim(t, r, 1, "mail").Jobs, 1)
			failOn(t, r, "UPDATE", "1")

			ok, err := call(r, id)
			require.Error(t, err)
			assert.False(t, ok)
		})
	}
}

// TestJobDeleteFinishedBeforeDeleteError: a failing delete is returned.
func TestJobDeleteFinishedBeforeDeleteError(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	id := enqueue(t, r, "mail", 0)
	require.NoError(t, db.Exec("UPDATE jobs SET status = 'succeeded', finished_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '-2 hours') WHERE id = ?", id).Error)
	failOn(t, r, "DELETE", "1")

	_, err := r.DeleteFinishedBefore(context.Background(), models.JobStatusSucceeded, time.Hour, 10)
	require.Error(t, err)
	var n int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM jobs").Scan(&n).Error)
	assert.EqualValues(t, 1, n, "nothing was deleted")
}

// TestJobRepositoryErrorsWhenTheTableIsGone: every operation returns an error
// (and none panics or reports success) when the table cannot be read.
func TestJobRepositoryErrorsWhenTheTableIsGone(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	require.NoError(t, db.Exec("DROP TABLE jobs").Error)
	ctx := context.Background()

	_, err := r.Enqueue(ctx, models.JobSpec{Type: "mail", Payload: []byte(`{}`), MaxAttempts: 1})
	assert.Error(t, err, "enqueue")
	_, err = r.Claim(ctx, []string{"w1"}, []string{"mail"}, time.Minute)
	assert.Error(t, err, "claim")
	_, _, err = r.ListPaged(ctx, models.JobFilter{}, 10, 0)
	assert.Error(t, err, "list count")
	_, err = r.Stats(ctx)
	assert.Error(t, err, "stats")
	_, err = r.DeleteFinishedBefore(ctx, models.JobStatusDead, time.Hour, 10)
	assert.Error(t, err, "delete")
	ok, err := r.Complete(ctx, 1, "w1")
	assert.Error(t, err, "complete")
	assert.False(t, ok)
}

// TestJobListPagedPageQueryError: the page query fails after the count query
// succeeded (the table holds a row that cannot be scanned).
func TestJobListPagedPageQueryError(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	id := enqueue(t, r, "mail", 0)
	require.NoError(t, db.Exec("UPDATE jobs SET run_at = 'not a time' WHERE id = ?", id).Error)

	_, _, err := r.ListPaged(context.Background(), models.JobFilter{}, 10, 0)
	require.Error(t, err, "an unreadable row is an error, not an empty page")
}

func TestJobTimeValue(t *testing.T) {
	t.Parallel()
	v, err := dbTime{}.Value()
	require.NoError(t, err)
	assert.Nil(t, v, "no time is NULL")

	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	v, err = dbTime{Time: now, Valid: true}.Value()
	require.NoError(t, err)
	assert.Equal(t, driver.Value(now), v)
}

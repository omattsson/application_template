package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newJobRepo(t *testing.T) (*GORMJobRepository, *gorm.DB) {
	t.Helper()
	db := openMigratedDB(t)
	return NewGORMJobRepository(db), db
}

func enqueue(t *testing.T, r *GORMJobRepository, typ string, delay time.Duration) uint64 {
	t.Helper()
	id, err := r.Enqueue(context.Background(), models.JobSpec{Type: typ, Payload: []byte(`{"a":1}`), MaxAttempts: 3, Delay: delay})
	require.NoError(t, err)
	require.NotZero(t, id)
	return id
}

// setTime moves a time column of a job by an offset from now (SQLite modifier).
func setTime(t *testing.T, db *gorm.DB, id uint64, col, modifier string) {
	t.Helper()
	require.NoError(t, db.Exec("UPDATE jobs SET "+col+" = strftime('%Y-%m-%d %H:%M:%f', 'now', ?) WHERE id = ?", modifier, id).Error)
}

type jobRow struct {
	Status      string
	LockedBy    string
	LastError   string
	LockedUntil *string
	FinishedAt  *string
	Attempts    int
}

func readJob(t *testing.T, db *gorm.DB, id uint64) jobRow {
	t.Helper()
	var row jobRow
	require.NoError(t, db.Raw("SELECT status, locked_by, last_error, locked_until, finished_at, attempts FROM jobs WHERE id = ?", id).Scan(&row).Error)
	return row
}

func claim(t *testing.T, r *GORMJobRepository, n int, types ...string) models.JobClaim {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "w" + string(rune('1'+i))
	}
	c, err := r.Claim(context.Background(), ids, types, time.Minute)
	require.NoError(t, err)
	return c
}

func TestJobEnqueueValidation(t *testing.T) {
	t.Parallel()
	ok := models.JobSpec{Type: "t", Payload: []byte(`{}`), MaxAttempts: 5}
	tests := []struct {
		mutate func(*models.JobSpec)
		name   string
	}{
		{func(s *models.JobSpec) { s.Type = "" }, "empty type"},
		{func(s *models.JobSpec) { s.Type = strings.Repeat("a", 101) }, "long type"},
		{func(s *models.JobSpec) { s.Payload = nil }, "no payload"},
		{func(s *models.JobSpec) { s.Payload = make([]byte, models.JobPayloadMaxBytes+1) }, "big payload"},
		{func(s *models.JobSpec) { s.MaxAttempts = 0 }, "zero attempts"},
		{func(s *models.JobSpec) { s.MaxAttempts = 51 }, "too many attempts"},
		{func(s *models.JobSpec) { s.Delay = -time.Second }, "negative delay"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, db := newJobRepo(t)
			spec := ok
			tt.mutate(&spec)
			_, err := r.Enqueue(context.Background(), spec)
			require.Error(t, err)
			assert.ErrorIs(t, err, dberrors.ErrValidation)
			var n int64
			require.NoError(t, db.Table("jobs").Count(&n).Error)
			assert.Zero(t, n)
		})
	}

	t.Run("a payload of exactly 64 KiB is accepted", func(t *testing.T) {
		t.Parallel()
		r, _ := newJobRepo(t)
		spec := ok
		spec.Payload = make([]byte, models.JobPayloadMaxBytes)
		_, err := r.Enqueue(context.Background(), spec)
		require.NoError(t, err)
	})
}

func TestJobEnqueueClaimComplete(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	id := enqueue(t, r, "mail", 0)

	row := readJob(t, db, id)
	assert.Equal(t, models.JobStatusPending, row.Status)
	assert.Zero(t, row.Attempts)

	c := claim(t, r, 2, "mail")
	require.Len(t, c.Jobs, 1)
	assert.Empty(t, c.Expired)
	j := c.Jobs[0]
	assert.Equal(t, id, j.ID)
	assert.Equal(t, "mail", j.Type)
	assert.JSONEq(t, `{"a":1}`, j.Payload)
	assert.Equal(t, int32(1), j.Attempts)
	assert.Equal(t, int32(3), j.MaxAttempts)
	assert.Equal(t, "w1", j.LockedBy)

	row = readJob(t, db, id)
	assert.Equal(t, models.JobStatusRunning, row.Status)
	assert.Equal(t, "w1", row.LockedBy)
	assert.NotNil(t, row.LockedUntil)
	assert.Equal(t, 1, row.Attempts)

	assert.Empty(t, claim(t, r, 2, "mail").Jobs, "a running job with a valid lease is not claimed")

	ok, err := r.Complete(ctx, id, "other")
	require.NoError(t, err)
	assert.False(t, ok, "fenced by the worker")
	ok, err = r.Complete(ctx, id, "w1")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = r.Complete(ctx, id, "w1")
	require.NoError(t, err)
	assert.False(t, ok, "a second complete changes nothing")

	row = readJob(t, db, id)
	assert.Equal(t, models.JobStatusSucceeded, row.Status)
	assert.Empty(t, row.LockedBy)
	assert.Nil(t, row.LockedUntil)
	assert.NotNil(t, row.FinishedAt)
}

func TestJobClaimOrderTypesAndDue(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	late := enqueue(t, r, "a", 0)
	early := enqueue(t, r, "a", 0)
	same := enqueue(t, r, "a", 0)
	future := enqueue(t, r, "a", time.Hour)
	otherType := enqueue(t, r, "b", 0)
	setTime(t, db, late, "run_at", "-1 seconds")
	setTime(t, db, early, "run_at", "-10 seconds")
	setTime(t, db, same, "run_at", "-1 seconds")
	setTime(t, db, otherType, "run_at", "-100 seconds")

	c := claim(t, r, 10, "a")
	require.Len(t, c.Jobs, 3, "the future job and the other type are skipped")
	assert.Equal(t, []uint64{early, late, same}, []uint64{c.Jobs[0].ID, c.Jobs[1].ID, c.Jobs[2].ID}, "run_at, then id")
	assert.Equal(t, models.JobStatusPending, readJob(t, db, future).Status)
	assert.Equal(t, models.JobStatusPending, readJob(t, db, otherType).Status)

	c = claim(t, r, 10, "b")
	require.Len(t, c.Jobs, 1)
	assert.Equal(t, otherType, c.Jobs[0].ID)
}

func TestJobClaimHonoursTheWorkerCount(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	for range 5 {
		enqueue(t, r, "a", 0)
	}
	c := claim(t, r, 2, "a")
	require.Len(t, c.Jobs, 2)
	assert.Equal(t, []string{"w1", "w2"}, []string{c.Jobs[0].LockedBy, c.Jobs[1].LockedBy})
	var n int64
	require.NoError(t, db.Table("jobs").Where("status = 'pending'").Count(&n).Error)
	assert.Equal(t, int64(3), n)
}

func TestJobClaimNothingToDo(t *testing.T) {
	t.Parallel()
	r, _ := newJobRepo(t)
	enqueue(t, r, "a", 0)
	ctx := context.Background()

	c, err := r.Claim(ctx, nil, []string{"a"}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, c.Jobs)
	c, err = r.Claim(ctx, []string{"w1"}, nil, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, c.Jobs, "no registered type, no claim")
	_, err = r.Claim(ctx, []string{"w1"}, []string{"a"}, 0)
	assert.ErrorIs(t, err, dberrors.ErrValidation)
}

func TestJobLeaseTakeover(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	id := enqueue(t, r, "a", 0)
	require.Len(t, claim(t, r, 1, "a").Jobs, 1)

	setTime(t, db, id, "locked_until", "-5 seconds")
	c, err := r.Claim(ctx, []string{"other"}, []string{"a"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1, "the expired lease is claimed again")
	assert.Equal(t, int32(2), c.Jobs[0].Attempts)
	assert.Equal(t, "other", c.Jobs[0].LockedBy)

	ok, err := r.Complete(ctx, id, "w1")
	require.NoError(t, err)
	assert.False(t, ok, "the first worker is fenced out")
	ok, err = r.Extend(ctx, id, "w1", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = r.Fail(ctx, id, "w1", "late", time.Second, false)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = r.Release(ctx, id, "w1")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, models.JobStatusRunning, readJob(t, db, id).Status)
	assert.Equal(t, "other", readJob(t, db, id).LockedBy)
}

func TestJobLeaseExpiredWithNoAttemptLeftIsDead(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	id, err := r.Enqueue(ctx, models.JobSpec{Type: "a", Payload: []byte(`{}`), MaxAttempts: 1})
	require.NoError(t, err)
	require.Len(t, claim(t, r, 1, "a").Jobs, 1)
	setTime(t, db, id, "locked_until", "-5 seconds")

	c, err := r.Claim(ctx, []string{"w2"}, []string{"a"}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, c.Jobs)
	require.Len(t, c.Expired, 1)
	assert.Equal(t, id, c.Expired[0].ID)
	assert.Equal(t, "a", c.Expired[0].Type)

	row := readJob(t, db, id)
	assert.Equal(t, models.JobStatusDead, row.Status)
	assert.Equal(t, "lease expired after 1 attempts", row.LastError)
	assert.NotNil(t, row.FinishedAt)
	assert.Empty(t, row.LockedBy)
}

func TestJobExpiredLeasesComeBeforeThePendingBacklog(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	stuck := enqueue(t, r, "a", 0)
	require.Len(t, claim(t, r, 1, "a").Jobs, 1)
	setTime(t, db, stuck, "locked_until", "-5 seconds")
	for range 3 {
		id := enqueue(t, r, "a", 0)
		setTime(t, db, id, "run_at", "-1 hours")
	}
	c, err := r.Claim(ctx, []string{"x"}, []string{"a"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	assert.Equal(t, stuck, c.Jobs[0].ID)
}

func TestJobExtend(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	id := enqueue(t, r, "a", 0)
	require.Len(t, claim(t, r, 1, "a").Jobs, 1)
	setTime(t, db, id, "locked_until", "+1 seconds")
	before := readJob(t, db, id).LockedUntil

	ok, err := r.Extend(ctx, id, "w1", time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)
	after := readJob(t, db, id).LockedUntil
	require.NotNil(t, after)
	assert.Greater(t, *after, *before)

	_, err = r.Extend(ctx, id, "w1", 0)
	assert.ErrorIs(t, err, dberrors.ErrValidation)
}

func TestJobFail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("retry later", func(t *testing.T) {
		t.Parallel()
		r, db := newJobRepo(t)
		id := enqueue(t, r, "a", 0)
		require.Len(t, claim(t, r, 1, "a").Jobs, 1)
		ok, err := r.Fail(ctx, id, "w1", "boom", time.Hour, false)
		require.NoError(t, err)
		require.True(t, ok)
		row := readJob(t, db, id)
		assert.Equal(t, models.JobStatusPending, row.Status)
		assert.Equal(t, "boom", row.LastError)
		assert.Equal(t, 1, row.Attempts, "the attempt stays counted")
		assert.Empty(t, row.LockedBy)
		assert.Nil(t, row.LockedUntil)
		assert.Nil(t, row.FinishedAt)
		assert.Empty(t, claim(t, r, 1, "a").Jobs, "not due for an hour")

		setTime(t, db, id, "run_at", "-1 seconds")
		c := claim(t, r, 1, "a")
		require.Len(t, c.Jobs, 1)
		assert.Equal(t, int32(2), c.Jobs[0].Attempts)
	})

	t.Run("dead", func(t *testing.T) {
		t.Parallel()
		r, db := newJobRepo(t)
		id := enqueue(t, r, "a", 0)
		require.Len(t, claim(t, r, 1, "a").Jobs, 1)
		ok, err := r.Fail(ctx, id, "w1", "fatal", 0, true)
		require.NoError(t, err)
		require.True(t, ok)
		row := readJob(t, db, id)
		assert.Equal(t, models.JobStatusDead, row.Status)
		assert.Equal(t, "fatal", row.LastError)
		assert.NotNil(t, row.FinishedAt)
		assert.Empty(t, claim(t, r, 1, "a").Jobs)
	})

	t.Run("the error text is cut to 2000 characters and kept valid", func(t *testing.T) {
		t.Parallel()
		r, db := newJobRepo(t)
		id := enqueue(t, r, "a", 0)
		require.Len(t, claim(t, r, 1, "a").Jobs, 1)
		long := strings.Repeat("é", 3000) + "\xff"
		_, err := r.Fail(ctx, id, "w1", long, 0, true)
		require.NoError(t, err)
		got := readJob(t, db, id).LastError
		assert.Equal(t, models.JobLastErrorMaxChars, len([]rune(got)))
	})

	t.Run("a negative delay is refused", func(t *testing.T) {
		t.Parallel()
		r, _ := newJobRepo(t)
		_, err := r.Fail(ctx, 1, "w1", "x", -time.Second, false)
		assert.ErrorIs(t, err, dberrors.ErrValidation)
	})
}

func TestJobRelease(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	id := enqueue(t, r, "a", 0)
	require.Len(t, claim(t, r, 1, "a").Jobs, 1)

	ok, err := r.Release(ctx, id, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	row := readJob(t, db, id)
	assert.Equal(t, models.JobStatusPending, row.Status)
	assert.Equal(t, 0, row.Attempts, "the attempt is not counted")
	assert.Empty(t, row.LockedBy)

	c := claim(t, r, 1, "a")
	require.Len(t, c.Jobs, 1, "due at once")
	assert.Equal(t, int32(1), c.Jobs[0].Attempts)

	ok, err = r.Release(ctx, id, "nobody")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestJobListPaged(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	a1 := enqueue(t, r, "a", 0)
	a2 := enqueue(t, r, "a", 0)
	b1 := enqueue(t, r, "b", time.Hour)
	setTime(t, db, a1, "created_at", "-30 seconds")
	setTime(t, db, a2, "created_at", "-10 seconds")
	setTime(t, db, b1, "created_at", "-20 seconds")
	require.NoError(t, db.Exec("UPDATE jobs SET status = 'dead', last_error = 'bad', finished_at = strftime('%Y-%m-%d %H:%M:%f','now') WHERE id = ?", a1).Error)

	ids := func(jobs []models.Job) []uint64 {
		out := make([]uint64, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, j.ID)
		}
		return out
	}

	tests := []struct {
		name      string
		filter    models.JobFilter
		limit     int
		offset    int
		want      []uint64
		wantTotal int64
	}{
		{"default is id ascending", models.JobFilter{}, 10, 0, []uint64{a1, a2, b1}, 3},
		{"by type", models.JobFilter{Type: "a"}, 10, 0, []uint64{a1, a2}, 2},
		{"by status", models.JobFilter{Status: "dead"}, 10, 0, []uint64{a1}, 1},
		{"by status and type", models.JobFilter{Status: "pending", Type: "b"}, 10, 0, []uint64{b1}, 1},
		{"no match", models.JobFilter{Status: "succeeded"}, 10, 0, []uint64{}, 0},
		{"created_at ascending", models.JobFilter{SortBy: "created_at"}, 10, 0, []uint64{a1, b1, a2}, 3},
		{"created_at descending", models.JobFilter{SortBy: "created_at", SortDesc: true}, 10, 0, []uint64{a2, b1, a1}, 3},
		{"run_at descending", models.JobFilter{SortBy: "run_at", SortDesc: true}, 10, 0, []uint64{b1, a2, a1}, 3},
		{"id descending", models.JobFilter{SortDesc: true}, 10, 0, []uint64{b1, a2, a1}, 3},
		{"id ascending", models.JobFilter{SortBy: "id"}, 10, 0, []uint64{a1, a2, b1}, 3},
		{"page", models.JobFilter{SortBy: "id"}, 1, 1, []uint64{a2}, 3},
		{"offset past the end", models.JobFilter{SortBy: "id"}, 10, 5, []uint64{}, 3},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, total, err := r.ListPaged(ctx, tt.filter, tt.limit, tt.offset)
			require.NoError(t, err)
			assert.Equal(t, tt.want, ids(got))
			assert.Equal(t, tt.wantTotal, total)
		})
	}

	t.Run("the rows carry the status fields and no payload", func(t *testing.T) {
		t.Parallel()
		got, _, err := r.ListPaged(ctx, models.JobFilter{Status: "dead"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		j := got[0]
		assert.Equal(t, "bad", j.LastError)
		assert.Equal(t, int32(3), j.MaxAttempts)
		assert.NotNil(t, j.FinishedAt)
		assert.False(t, j.CreatedAt.IsZero())
		assert.False(t, j.RunAt.IsZero())
		assert.Empty(t, j.Payload, "the payload is never selected")
	})

	t.Run("an unknown sort key or status is a validation error", func(t *testing.T) {
		t.Parallel()
		_, _, err := r.ListPaged(ctx, models.JobFilter{SortBy: "payload"}, 10, 0)
		assert.ErrorIs(t, err, dberrors.ErrValidation)
		_, _, err = r.ListPaged(ctx, models.JobFilter{Status: "weird"}, 10, 0)
		assert.ErrorIs(t, err, dberrors.ErrValidation)
	})
}

func TestJobStats(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()

	s, err := r.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, models.JobStats{}, s)

	old := enqueue(t, r, "a", 0)
	enqueue(t, r, "a", 0)
	enqueue(t, r, "a", time.Hour)
	running := enqueue(t, r, "a", 0)
	done := enqueue(t, r, "a", 0)
	dead := enqueue(t, r, "a", 0)
	setTime(t, db, old, "run_at", "-90 seconds")
	require.NoError(t, db.Exec("UPDATE jobs SET status='running' WHERE id = ?", running).Error)
	require.NoError(t, db.Exec("UPDATE jobs SET status='succeeded' WHERE id = ?", done).Error)
	require.NoError(t, db.Exec("UPDATE jobs SET status='dead' WHERE id = ?", dead).Error)

	s, err = r.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), s.Pending)
	assert.Equal(t, int64(1), s.Running)
	assert.Equal(t, int64(1), s.Succeeded)
	assert.Equal(t, int64(1), s.Dead)
	assert.Equal(t, int64(2), s.Due, "the future job is not due")
	assert.InDelta(t, 90, s.OldestDueAge.Seconds(), 5)
}

func TestJobDeleteFinishedBefore(t *testing.T) {
	t.Parallel()
	r, db := newJobRepo(t)
	ctx := context.Background()
	var oldDone, newDone, oldDead []uint64
	for range 3 {
		id := enqueue(t, r, "a", 0)
		require.NoError(t, db.Exec("UPDATE jobs SET status='succeeded' WHERE id = ?", id).Error)
		setTime(t, db, id, "finished_at", "-2 hours")
		oldDone = append(oldDone, id)
	}
	id := enqueue(t, r, "a", 0)
	require.NoError(t, db.Exec("UPDATE jobs SET status='succeeded' WHERE id = ?", id).Error)
	setTime(t, db, id, "finished_at", "-1 minutes")
	newDone = append(newDone, id)
	id = enqueue(t, r, "a", 0)
	require.NoError(t, db.Exec("UPDATE jobs SET status='dead' WHERE id = ?", id).Error)
	setTime(t, db, id, "finished_at", "-2 hours")
	oldDead = append(oldDead, id)
	pending := enqueue(t, r, "a", 0)

	n, err := r.DeleteFinishedBefore(ctx, models.JobStatusSucceeded, time.Hour, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "a batch is bounded by the limit")
	n, err = r.DeleteFinishedBefore(ctx, models.JobStatusSucceeded, time.Hour, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	n, err = r.DeleteFinishedBefore(ctx, models.JobStatusSucceeded, time.Hour, 2)
	require.NoError(t, err)
	assert.Zero(t, n)

	var left []uint64
	require.NoError(t, db.Table("jobs").Order("id").Pluck("id", &left).Error)
	assert.Equal(t, append(append(append([]uint64{}, newDone...), oldDead...), pending), left, "recent, dead and unfinished rows stay")

	n, err = r.DeleteFinishedBefore(ctx, models.JobStatusDead, time.Hour, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	_ = oldDone
}

func TestJobDeleteFinishedBeforeValidation(t *testing.T) {
	t.Parallel()
	r, _ := newJobRepo(t)
	ctx := context.Background()
	for _, status := range []string{"pending", "running", ""} {
		_, err := r.DeleteFinishedBefore(ctx, status, time.Hour, 10)
		assert.ErrorIs(t, err, dberrors.ErrValidation, status)
	}
	_, err := r.DeleteFinishedBefore(ctx, models.JobStatusDead, time.Hour, 0)
	assert.ErrorIs(t, err, dberrors.ErrValidation)
}

func TestJobRepositoryCancelledContext(t *testing.T) {
	t.Parallel()
	r, _ := newJobRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Enqueue(ctx, models.JobSpec{Type: "a", Payload: []byte(`{}`), MaxAttempts: 1})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = r.Stats(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestTruncateChars(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "abc", truncateChars("abcdef", 3))
	assert.Equal(t, "abc", truncateChars("abc", 3))
	assert.Equal(t, "åä", truncateChars("åäö", 2))
	assert.Equal(t, "a�b", truncateChars("a\xffb", 10))
}

func TestJobTimeScanner(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 21, 10, 11, 12, 123456000, time.UTC)
	tests := []struct {
		in      any
		name    string
		valid   bool
		wantErr bool
	}{
		{want, "time", true, false},
		{"2026-11-21 10:11:12.123456", "text", true, false},
		{[]byte("2026-11-21 10:11:12.123456"), "bytes", true, false},
		{nil, "null", false, false},
		{"yesterday", "bad text", false, true},
		{42, "bad type", false, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var v dbTime
			err := v.Scan(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.valid, v.Valid)
			if tt.valid {
				assert.True(t, v.Time.Equal(want), v.Time)
			}
		})
	}
}

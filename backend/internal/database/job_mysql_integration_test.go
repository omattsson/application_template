//go:build integration

package database

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func jobsTestRepo(t *testing.T) (*GORMJobRepository, *gorm.DB) {
	t.Helper()
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	sqlDB, err := db.DB.DB()
	require.NoError(t, err)
	require.NoError(t, db.Exec("DELETE FROM jobs").Error)
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM jobs").Error
		_ = sqlDB.Close()
	})
	return NewGORMJobRepository(db.DB), db.DB
}

func jobSpec(typ string) models.JobSpec {
	return models.JobSpec{Type: typ, Payload: []byte(`{"k":"v"}`), MaxAttempts: 3}
}

// TestDatabaseJobsTable: the migrated column types and indexes on MySQL, the
// microsecond database clock and the repository operations.
func TestDatabaseJobsTable(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()

	for col, want := range map[string]string{
		"id": "bigint unsigned", "type": "varchar(100)", "payload": "longtext", "status": "varchar(16)",
		"attempts": "int", "max_attempts": "int", "run_at": "datetime(6)", "locked_by": "varchar(260)",
		"locked_until": "datetime(6)", "last_error": "varchar(2000)", "created_at": "datetime(6)",
		"updated_at": "datetime(6)", "finished_at": "datetime(6)",
	} {
		var got string
		require.NoError(t, db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'jobs' AND column_name = ?", col).Scan(&got).Error)
		assert.Equal(t, want, got, col)
	}
	for _, idx := range []string{"idx_jobs_claim", "idx_jobs_finished", "idx_jobs_created_at", "idx_jobs_run_at", "idx_jobs_type_status"} {
		var n int64
		require.NoError(t, db.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'jobs' AND index_name = ?", idx).Scan(&n).Error)
		assert.Positive(t, n, idx)
	}

	id, err := repo.Enqueue(ctx, jobSpec("it.mail"))
	require.NoError(t, err)
	delayed, err := repo.Enqueue(ctx, models.JobSpec{Type: "it.mail", Payload: []byte(`{}`), MaxAttempts: 3, Delay: time.Hour})
	require.NoError(t, err)

	var micros int64
	require.NoError(t, db.Raw("SELECT TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(6), run_at) FROM jobs WHERE id = ?", delayed).Scan(&micros).Error)
	assert.InDelta(t, time.Hour.Microseconds(), micros, float64(time.Minute.Microseconds()), "run_at is the database clock plus the delay")

	c, err := repo.Claim(ctx, []string{"w1", "w2"}, []string{"it.mail"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1, "the delayed job is not due")
	assert.Equal(t, id, c.Jobs[0].ID)
	assert.Equal(t, int32(1), c.Jobs[0].Attempts)

	ok, err := repo.Extend(ctx, id, "w1", time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = repo.Complete(ctx, id, "w2")
	require.NoError(t, err)
	assert.False(t, ok, "fenced")
	ok, err = repo.Fail(ctx, id, "w1", "boom", time.Hour, false)
	require.NoError(t, err)
	assert.True(t, ok)

	list, total, err := repo.ListPaged(ctx, models.JobFilter{Type: "it.mail", SortBy: "run_at", SortDesc: true}, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	require.Len(t, list, 2)
	assert.Equal(t, "boom", list[0].LastError)
	assert.False(t, list[0].RunAt.IsZero())
	assert.Empty(t, list[0].Payload)

	stats, err := repo.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Pending)
	assert.Zero(t, stats.Due)
}

// TestDatabaseJobsClaimSkipsLockedRows: concurrent claims never return the
// same job twice, and no claim waits for the rows another one locked.
func TestDatabaseJobsClaimSkipsLockedRows(t *testing.T) {
	repo, _ := jobsTestRepo(t)
	ctx := context.Background()
	const jobs, claimers = 60, 6
	for range jobs {
		_, err := repo.Enqueue(ctx, jobSpec("it.skip"))
		require.NoError(t, err)
	}

	// An empty claim does not mean that the queue is empty: SKIP LOCKED skips
	// the rows that another claimer holds at that moment. So each claimer goes
	// on until all jobs are claimed (or the deadline).
	var mu sync.Mutex
	seen := map[uint64]int{}
	var claimed atomic.Int64
	deadline := time.Now().Add(30 * time.Second)
	var wg sync.WaitGroup
	for c := 0; c < claimers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := []string{"a", "b", "c", "d", "e"}
			for claimed.Load() < jobs && time.Now().Before(deadline) {
				claim, err := repo.Claim(ctx, ids, []string{"it.skip"}, time.Minute)
				if !assert.NoError(t, err) {
					return
				}
				mu.Lock()
				for _, j := range claim.Jobs {
					seen[j.ID]++
				}
				mu.Unlock()
				claimed.Add(int64(len(claim.Jobs)))
			}
		}()
	}
	wg.Wait()

	assert.Len(t, seen, jobs, "every job was claimed")
	for id, n := range seen {
		assert.Equal(t, 1, n, "job %d was claimed %d times", id, n)
	}
}

// TestDatabaseJobsLeaseTakeover: a running job whose lease passed is claimed
// again by another worker, and dead when its attempts are used up.
func TestDatabaseJobsLeaseTakeover(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()

	id, err := repo.Enqueue(ctx, jobSpec("it.lease"))
	require.NoError(t, err)
	last, err := repo.Enqueue(ctx, models.JobSpec{Type: "it.lease", Payload: []byte(`{}`), MaxAttempts: 1})
	require.NoError(t, err)
	c, err := repo.Claim(ctx, []string{"w1", "w2"}, []string{"it.lease"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 2)

	c, err = repo.Claim(ctx, []string{"x"}, []string{"it.lease"}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, c.Jobs, "the leases are valid")

	require.NoError(t, db.Exec("UPDATE jobs SET locked_until = UTC_TIMESTAMP(6) - INTERVAL 1 SECOND").Error)
	c, err = repo.Claim(ctx, []string{"x", "y"}, []string{"it.lease"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	assert.Equal(t, id, c.Jobs[0].ID)
	assert.Equal(t, int32(2), c.Jobs[0].Attempts)
	require.Len(t, c.Expired, 1)
	assert.Equal(t, last, c.Expired[0].ID)

	var row struct {
		Status    string
		LastError string
	}
	require.NoError(t, db.Raw("SELECT status, last_error FROM jobs WHERE id = ?", last).Scan(&row).Error)
	assert.Equal(t, "dead", row.Status)
	assert.Equal(t, "lease expired after 1 attempts", row.LastError)

	ok, err := repo.Complete(ctx, id, "w1")
	require.NoError(t, err)
	assert.False(t, ok, "the first worker lost the job")
}

// TestDatabaseJobsStatsRetentionAndRelease: age of the oldest due job by the
// database clock, the bounded delete and the release.
func TestDatabaseJobsStatsRetentionAndRelease(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()

	a, err := repo.Enqueue(ctx, jobSpec("it.ret"))
	require.NoError(t, err)
	require.NoError(t, db.Exec("UPDATE jobs SET run_at = UTC_TIMESTAMP(6) - INTERVAL 90 SECOND WHERE id = ?", a).Error)
	stats, err := repo.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Due)
	assert.InDelta(t, 90, stats.OldestDueAge.Seconds(), 5)

	c, err := repo.Claim(ctx, []string{"w1"}, []string{"it.ret"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	ok, err := repo.Release(ctx, a, "w1")
	require.NoError(t, err)
	assert.True(t, ok)
	var attempts int
	require.NoError(t, db.Raw("SELECT attempts FROM jobs WHERE id = ?", a).Scan(&attempts).Error)
	assert.Zero(t, attempts)

	var ids []uint64
	for range 3 {
		id, err := repo.Enqueue(ctx, jobSpec("it.ret"))
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, db.Exec("UPDATE jobs SET status = 'succeeded', finished_at = UTC_TIMESTAMP(6) - INTERVAL 2 HOUR WHERE id IN ?", ids).Error)
	n, err := repo.DeleteFinishedBefore(ctx, models.JobStatusSucceeded, time.Hour, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	n, err = repo.DeleteFinishedBefore(ctx, models.JobStatusSucceeded, time.Hour, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

// setJobTimes pins the clock columns of a job by SQL (database clock offsets
// in seconds, negative is the past).
func setJobTimes(t *testing.T, db *gorm.DB, id uint64, runAtOffsetSec int) {
	t.Helper()
	require.NoError(t, db.Exec("UPDATE jobs SET run_at = UTC_TIMESTAMP(6) + INTERVAL ? SECOND WHERE id = ?", runAtOffsetSec, id).Error)
}

// TestDatabaseJobsClaimOrderAndTypes: due jobs of the asked types only, in
// run_at then id order, at most one job per worker ID, expired leases first,
// and a job that is not due yet is skipped.
func TestDatabaseJobsClaimOrderAndTypes(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()

	enq := func(typ string, runAt int) uint64 {
		id, err := repo.Enqueue(ctx, jobSpec(typ))
		require.NoError(t, err)
		setJobTimes(t, db, id, runAt)
		return id
	}
	newest := enq("it.order", -10)
	oldest := enq("it.order", -100)
	tieA := enq("it.order", -50)
	tieB := enq("it.order", -50)
	_ = enq("it.other", -500)
	future := enq("it.order", 3600)

	c, err := repo.Claim(ctx, []string{"w1", "w2", "w3"}, []string{"it.order"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 3, "one job per worker ID")
	assert.Equal(t, []uint64{oldest, tieA, tieB}, []uint64{c.Jobs[0].ID, c.Jobs[1].ID, c.Jobs[2].ID}, "run_at, then id")
	assert.Equal(t, []string{"w1", "w2", "w3"}, []string{c.Jobs[0].LockedBy, c.Jobs[1].LockedBy, c.Jobs[2].LockedBy})
	assert.Equal(t, `{"k":"v"}`, c.Jobs[0].Payload)

	// An expired lease is claimed before the due backlog.
	require.NoError(t, db.Exec("UPDATE jobs SET locked_until = UTC_TIMESTAMP(6) - INTERVAL 1 SECOND WHERE id = ?", tieB).Error)
	c, err = repo.Claim(ctx, []string{"x1"}, []string{"it.order"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	assert.Equal(t, tieB, c.Jobs[0].ID, "expired leases first")
	assert.Equal(t, int32(2), c.Jobs[0].Attempts)

	c, err = repo.Claim(ctx, []string{"x1"}, []string{"it.order"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	assert.Equal(t, newest, c.Jobs[0].ID)

	c, err = repo.Claim(ctx, []string{"x1"}, []string{"it.order"}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, c.Jobs, "the future job is not due and it.other is not asked for (id %d)", future)

	c, err = repo.Claim(ctx, []string{"x1"}, []string{"it.unknown"}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, c.Jobs)
}

// TestDatabaseJobsConcurrentTakeover: several claimers race for expired
// leases. Every job is taken over by exactly one of them, and a job with its
// attempts used up goes dead exactly once (listed in one claim only).
func TestDatabaseJobsConcurrentTakeover(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()
	const retry, exhausted, claimers = 20, 20, 5

	for range retry {
		_, err := repo.Enqueue(ctx, models.JobSpec{Type: "it.take", Payload: []byte(`{}`), MaxAttempts: 3})
		require.NoError(t, err)
	}
	for range exhausted {
		_, err := repo.Enqueue(ctx, models.JobSpec{Type: "it.take", Payload: []byte(`{}`), MaxAttempts: 1})
		require.NoError(t, err)
	}
	// All run once by a "crashed" worker group, then every lease expires.
	for {
		c, err := repo.Claim(ctx, []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8"}, []string{"it.take"}, time.Minute)
		require.NoError(t, err)
		if len(c.Jobs) == 0 {
			break
		}
	}
	require.NoError(t, db.Exec("UPDATE jobs SET locked_until = UTC_TIMESTAMP(6) - INTERVAL 1 SECOND").Error)

	// An empty claim only means that the remaining rows are locked by another
	// claimer right now; each claimer goes on until every job is accounted for.
	var mu sync.Mutex
	taken, dead := map[uint64]int{}, map[uint64]int{}
	var done atomic.Int64
	deadline := time.Now().Add(30 * time.Second)
	var wg sync.WaitGroup
	for c := 0; c < claimers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done.Load() < retry+exhausted && time.Now().Before(deadline) {
				claim, err := repo.Claim(ctx, []string{"r1", "r2", "r3"}, []string{"it.take"}, time.Minute)
				if !assert.NoError(t, err) {
					return
				}
				mu.Lock()
				for _, j := range claim.Jobs {
					taken[j.ID]++
				}
				for _, j := range claim.Expired {
					dead[j.ID]++
				}
				mu.Unlock()
				done.Add(int64(len(claim.Jobs) + len(claim.Expired)))
			}
		}()
	}
	wg.Wait()

	assert.Len(t, taken, retry)
	assert.Len(t, dead, exhausted)
	for id, n := range taken {
		assert.Equal(t, 1, n, "job %d taken over %d times", id, n)
	}
	for id, n := range dead {
		assert.Equal(t, 1, n, "job %d reported dead %d times", id, n)
	}
	var nDead int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM jobs WHERE status = 'dead' AND last_error = 'lease expired after 1 attempts'").Scan(&nDead).Error)
	assert.Equal(t, int64(exhausted), nDead)
}

// TestDatabaseJobsFencing: after a takeover the first worker's Extend,
// Complete, Fail and Release change nothing; the same calls of a worker that
// does not hold the job, and of a job that is not running, are fenced too.
func TestDatabaseJobsFencing(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()

	id, err := repo.Enqueue(ctx, jobSpec("it.fence"))
	require.NoError(t, err)
	c, err := repo.Claim(ctx, []string{"w1"}, []string{"it.fence"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	require.NoError(t, db.Exec("UPDATE jobs SET locked_until = UTC_TIMESTAMP(6) - INTERVAL 1 SECOND WHERE id = ?", id).Error)
	c, err = repo.Claim(ctx, []string{"w2"}, []string{"it.fence"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)

	ok, err := repo.Extend(ctx, id, "w1", time.Hour)
	require.NoError(t, err)
	assert.False(t, ok, "extend")
	ok, err = repo.Complete(ctx, id, "w1")
	require.NoError(t, err)
	assert.False(t, ok, "complete")
	ok, err = repo.Fail(ctx, id, "w1", "late", time.Second, false)
	require.NoError(t, err)
	assert.False(t, ok, "fail")
	ok, err = repo.Fail(ctx, id, "w1", "late", 0, true)
	require.NoError(t, err)
	assert.False(t, ok, "fail dead")
	ok, err = repo.Release(ctx, id, "w1")
	require.NoError(t, err)
	assert.False(t, ok, "release")

	var row struct {
		Status, LockedBy, LastError string
		Attempts                    int
	}
	require.NoError(t, db.Raw("SELECT status, locked_by, last_error, attempts FROM jobs WHERE id = ?", id).Scan(&row).Error)
	assert.Equal(t, "running", row.Status)
	assert.Equal(t, "w2", row.LockedBy)
	assert.Empty(t, row.LastError, "the late result left no trace")
	assert.Equal(t, 2, row.Attempts)

	ok, err = repo.Extend(ctx, id, "w2", time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = repo.Complete(ctx, id, "w2")
	require.NoError(t, err)
	assert.True(t, ok)

	// Finished: nothing applies any more, not even to its last worker.
	for name, call := range map[string]func() (bool, error){
		"extend":   func() (bool, error) { return repo.Extend(ctx, id, "w2", time.Hour) },
		"complete": func() (bool, error) { return repo.Complete(ctx, id, "w2") },
		"fail":     func() (bool, error) { return repo.Fail(ctx, id, "w2", "x", 0, true) },
		"release":  func() (bool, error) { return repo.Release(ctx, id, "w2") },
	} {
		ok, err := call()
		require.NoError(t, err, name)
		assert.False(t, ok, name)
	}
	ok, err = repo.Complete(ctx, id+1000, "w2")
	require.NoError(t, err)
	assert.False(t, ok, "unknown id")
}

// TestDatabaseJobsFailRetryAndDead: Fail with a delay returns the job to
// pending at the database clock plus the delay without a new attempt; both
// forms cut last_error to 2000 characters (not bytes); Release then a new
// claim counts the attempt once.
func TestDatabaseJobsFailRetryAndDead(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()
	long := strings.Repeat("é", 2500)

	id, err := repo.Enqueue(ctx, jobSpec("it.fail"))
	require.NoError(t, err)
	c, err := repo.Claim(ctx, []string{"w1"}, []string{"it.fail"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)

	ok, err := repo.Fail(ctx, id, "w1", long, 2*time.Hour, false)
	require.NoError(t, err)
	require.True(t, ok)
	var row struct {
		Status, LockedBy, LastError string
		Attempts                    int
		LockedUntilNull             bool
		FinishedNull                bool
		DelayMicros                 int64
	}
	load := func() {
		require.NoError(t, db.Raw("SELECT status, locked_by, last_error, attempts, locked_until IS NULL AS locked_until_null, finished_at IS NULL AS finished_null, TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(6), run_at) AS delay_micros FROM jobs WHERE id = ?", id).Scan(&row).Error)
	}
	load()
	assert.Equal(t, "pending", row.Status)
	assert.Empty(t, row.LockedBy)
	assert.Equal(t, 1, row.Attempts, "the failed attempt stays counted")
	assert.True(t, row.LockedUntilNull)
	assert.True(t, row.FinishedNull)
	assert.Equal(t, 2000, len([]rune(row.LastError)))
	assert.InDelta(t, (2 * time.Hour).Microseconds(), row.DelayMicros, float64(time.Minute.Microseconds()))

	c, err = repo.Claim(ctx, []string{"w1"}, []string{"it.fail"}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, c.Jobs, "the retry is not due")

	setJobTimes(t, db, id, -1)
	c, err = repo.Claim(ctx, []string{"w1"}, []string{"it.fail"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	assert.Equal(t, int32(2), c.Jobs[0].Attempts)

	ok, err = repo.Release(ctx, id, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	c, err = repo.Claim(ctx, []string{"w1"}, []string{"it.fail"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	assert.Equal(t, int32(2), c.Jobs[0].Attempts, "a released attempt is not counted")

	ok, err = repo.Fail(ctx, id, "w1", long, 0, true)
	require.NoError(t, err)
	require.True(t, ok)
	load()
	assert.Equal(t, "dead", row.Status)
	assert.False(t, row.FinishedNull)
	assert.Equal(t, 2000, len([]rune(row.LastError)))
}

// TestDatabaseJobsListPaged: filter by status and type, the sort whitelist in
// both directions with the id tie-break, paging with a count that ignores the
// page, the projection and the validation errors, on MySQL.
func TestDatabaseJobsListPaged(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()

	// 12 jobs: types it.a (8) and it.b (4); run_at distinct per pair so that
	// the sort has ties; statuses spread by SQL.
	ids := make([]uint64, 12)
	for i := range ids {
		typ := "it.a"
		if i >= 8 {
			typ = "it.b"
		}
		id, err := repo.Enqueue(ctx, jobSpec(typ))
		require.NoError(t, err)
		ids[i] = id
		setJobTimes(t, db, id, -1000+(i/2)*10)
	}
	require.NoError(t, db.Exec("UPDATE jobs SET status = 'dead', last_error = 'boom', finished_at = UTC_TIMESTAMP(6) WHERE id IN ?", []uint64{ids[0], ids[1], ids[9]}).Error)
	require.NoError(t, db.Exec("UPDATE jobs SET status = 'running', locked_by = 'w#1', locked_until = UTC_TIMESTAMP(6) + INTERVAL 1 MINUTE WHERE id = ?", ids[2]).Error)

	idsOf := func(js []models.Job) []uint64 {
		out := make([]uint64, len(js))
		for i, j := range js {
			out[i] = j.ID
		}
		return out
	}

	t.Run("no filter, default sort is newest id first, paging", func(t *testing.T) {
		var got []uint64
		for _, off := range []int{0, 5, 10, 15} {
			page, total, err := repo.ListPaged(ctx, models.JobFilter{SortDesc: true}, 5, off)
			require.NoError(t, err)
			assert.Equal(t, int64(12), total, "the count ignores the page")
			got = append(got, idsOf(page)...)
		}
		want := make([]uint64, 12)
		for i := range want {
			want[i] = ids[11-i]
		}
		assert.Equal(t, want, got, "pages are disjoint and complete")
	})

	t.Run("filter", func(t *testing.T) {
		page, total, err := repo.ListPaged(ctx, models.JobFilter{Status: "dead"}, 10, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(3), total)
		assert.Equal(t, []uint64{ids[0], ids[1], ids[9]}, idsOf(page), "ascending id by default")

		page, total, err = repo.ListPaged(ctx, models.JobFilter{Type: "it.b"}, 2, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(4), total, "the count covers the filter, not the page")
		assert.Len(t, page, 2)

		page, total, err = repo.ListPaged(ctx, models.JobFilter{Type: "it.b", Status: "dead"}, 10, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
		require.Len(t, page, 1)
		assert.Equal(t, ids[9], page[0].ID)

		page, total, err = repo.ListPaged(ctx, models.JobFilter{Type: "it.nothing"}, 10, 0)
		require.NoError(t, err)
		assert.Zero(t, total)
		assert.Empty(t, page)
		assert.NotNil(t, page, "an empty page is an empty list, not null")
	})

	t.Run("sort with id tie-break", func(t *testing.T) {
		// run_at is equal within a pair, so the order inside a pair is the id's
		// direction.
		page, _, err := repo.ListPaged(ctx, models.JobFilter{SortBy: "run_at"}, 12, 0)
		require.NoError(t, err)
		assert.Equal(t, ids, idsOf(page))
		page, _, err = repo.ListPaged(ctx, models.JobFilter{SortBy: "run_at", SortDesc: true}, 12, 0)
		require.NoError(t, err)
		want := make([]uint64, 12)
		for i := range want {
			want[i] = ids[11-i]
		}
		assert.Equal(t, want, idsOf(page))
		for _, key := range []string{"created_at", "id"} {
			_, _, err = repo.ListPaged(ctx, models.JobFilter{SortBy: key}, 12, 0)
			assert.NoError(t, err, key)
		}
	})

	t.Run("projection", func(t *testing.T) {
		page, _, err := repo.ListPaged(ctx, models.JobFilter{Status: "running"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, page, 1)
		j := page[0]
		assert.Empty(t, j.Payload, "the list never reads the payload")
		assert.Equal(t, "w#1", j.LockedBy)
		require.NotNil(t, j.LockedUntil)
		assert.Nil(t, j.FinishedAt)

		page, _, err = repo.ListPaged(ctx, models.JobFilter{Status: "dead", Type: "it.b"}, 10, 0)
		require.NoError(t, err)
		require.Len(t, page, 1)
		assert.Equal(t, "boom", page[0].LastError)
		require.NotNil(t, page[0].FinishedAt)
		assert.Nil(t, page[0].LockedUntil)
	})

	t.Run("validation", func(t *testing.T) {
		_, _, err := repo.ListPaged(ctx, models.JobFilter{SortBy: "payload"}, 10, 0)
		assert.ErrorIs(t, err, dberrors.ErrValidation)
		_, _, err = repo.ListPaged(ctx, models.JobFilter{Status: "bogus"}, 10, 0)
		assert.ErrorIs(t, err, dberrors.ErrValidation)
		_, _, err = repo.ListPaged(ctx, models.JobFilter{Type: "x' OR '1'='1"}, 10, 0)
		assert.NoError(t, err, "the type is a parameter")
	})
}

// TestDatabaseJobsStatsCounts: the counts per status, due pending jobs and a
// future job that is not due.
func TestDatabaseJobsStatsCounts(t *testing.T) {
	repo, db := jobsTestRepo(t)
	ctx := context.Background()

	stats, err := repo.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, models.JobStats{}, stats, "an empty table")

	mk := func(status string, runAt int) {
		id, err := repo.Enqueue(ctx, jobSpec("it.stats"))
		require.NoError(t, err)
		setJobTimes(t, db, id, runAt)
		require.NoError(t, db.Exec("UPDATE jobs SET status = ? WHERE id = ?", status, id).Error)
	}
	mk("pending", -30)
	mk("pending", -10)
	mk("pending", 600)
	mk("running", -5)
	mk("succeeded", -5)
	mk("succeeded", -5)
	mk("dead", -5)

	stats, err = repo.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Pending)
	assert.Equal(t, int64(1), stats.Running)
	assert.Equal(t, int64(2), stats.Succeeded)
	assert.Equal(t, int64(1), stats.Dead)
	assert.Equal(t, int64(2), stats.Due, "the future job is not due")
	assert.InDelta(t, 30, stats.OldestDueAge.Seconds(), 5)
}

// insertJobsAt inserts n due pending jobs of typ in one multi-row INSERT per
// 500 rows. offsetSec is the run_at offset in database clock seconds; the
// microsecond part grows with the row so that the run_at, id order is fixed.
func insertJobsAt(t *testing.T, db *gorm.DB, typ string, n, offsetSec int) {
	t.Helper()
	for done := 0; done < n; {
		batch := min(500, n-done)
		var sb strings.Builder
		args := make([]any, 0, batch*3)
		sb.WriteString("INSERT INTO jobs (type, payload, status, attempts, max_attempts, run_at, locked_by, last_error, created_at, updated_at) VALUES ")
		for i := 0; i < batch; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, '{}', 'pending', 0, 3, UTC_TIMESTAMP(6) + INTERVAL ? MICROSECOND, '', '', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))")
			args = append(args, typ, int64(offsetSec)*1_000_000+int64(done+i))
		}
		require.NoError(t, db.Exec(sb.String(), args...).Error)
		done += batch
	}
}

// TestDatabaseJobsClaimScanLeavesForeignRowsUnlocked: a replica that scans a
// large backlog of a type it does not handle holds no lock on those rows (the
// type filter is not in idx_jobs_claim), so another replica still claims its
// own type and Enqueue still inserts while that scan transaction is open. At
// REPEATABLE READ the scan keeps record and gap locks: the other claim skips
// the rows that were locked and returns nothing, and the insert waits.
//
// The open transaction is built from the claim's own parts (TxOpts with
// jobClaimTxOptions), because Claim commits before it returns. Two scans: the
// one of the claim (the planner picks its index; on this data it may use
// idx_jobs_type_status) and the same query forced to idx_jobs_claim, the plan
// that scans a foreign backlog. No assert has a time limit that decides the
// outcome: each waits up to a generous deadline for a result.
func TestDatabaseJobsClaimScanLeavesForeignRowsUnlocked(t *testing.T) {
	const foreign, wanted = 5000, 3

	tests := []struct {
		scan func(r *GORMJobRepository, tx *gorm.DB, dialect string) (int, error)
		name string
	}{
		{
			name: "planner index",
			scan: func(r *GORMJobRepository, tx *gorm.DB, dialect string) (int, error) {
				rows, err := r.selectClaimable(tx, dialect, "status = 'pending' AND run_at <= "+jobNowSQL(dialect), []string{"it.none"}, 1)
				return len(rows), err
			},
		},
		{
			name: "idx_jobs_claim",
			scan: func(_ *GORMJobRepository, tx *gorm.DB, _ string) (int, error) {
				var rows []jobClaimRow
				err := tx.Raw("SELECT id, type, payload, attempts, max_attempts FROM jobs FORCE INDEX (idx_jobs_claim) " +
					"WHERE status = 'pending' AND run_at <= UTC_TIMESTAMP(6) AND type IN ('it.none') " +
					"ORDER BY run_at ASC, id ASC LIMIT 1 FOR UPDATE SKIP LOCKED").Scan(&rows).Error
				return len(rows), err
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			repo1, db := jobsTestRepo(t)
			repo2 := NewGORMJobRepository(db)
			ctx := context.Background()
			// The foreign backlog is older than the wanted jobs, so a scan of a
			// third type passes all of it and then the wanted rows too.
			insertJobsAt(t, db, "it.foreign", foreign, -600)
			insertJobsAt(t, db, "it.wanted", wanted, -300)

			dialect := db.Dialector.Name()
			scanned := make(chan int, 1)
			release := make(chan struct{})
			txDone := make(chan error, 1)
			go func() {
				txDone <- repo1.TxOpts(ctx, jobClaimTxOptions(dialect), func(tx *gorm.DB) error {
					n, err := tt.scan(repo1, tx, dialect)
					if err != nil {
						return err
					}
					scanned <- n
					select {
					case <-release:
					case <-time.After(90 * time.Second):
					}
					return nil
				})
			}()
			// The scan transaction ends here, also after a failed require.
			t.Cleanup(func() {
				close(release)
				assert.NoError(t, <-txDone)
			})

			select {
			case n := <-scanned:
				require.Zero(t, n, "no job of the unhandled type")
			case <-time.After(60 * time.Second):
				t.Fatal("the scan did not finish")
			}

			// Replica 2 claims its own type while the scan transaction is open.
			claimCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			claim, err := repo2.Claim(claimCtx, []string{"w1", "w2", "w3", "w4"}, []string{"it.wanted"}, time.Minute)
			require.NoError(t, err)
			assert.Len(t, claim.Jobs, wanted, "the rows that the open scan passed are not locked")

			// Enqueue inserts while the scan is open.
			enqCtx, cancelEnq := context.WithTimeout(ctx, 30*time.Second)
			defer cancelEnq()
			_, err = repo2.Enqueue(enqCtx, jobSpec("it.late"))
			assert.NoError(t, err, "Enqueue is not blocked by the open scan")
		})
	}
}

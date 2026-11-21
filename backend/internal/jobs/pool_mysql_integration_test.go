//go:build integration

package jobs

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The integration tests run pools on a real MySQL, one connection pool per
// replica as separate processes would have. They share the jobs table with
// the other packages (run with -p 1) and run one after the other here; each
// uses job types of its own and removes its rows. Waits poll with a deadline:
// no assertion depends on how fast MySQL or the pool is.

const itWait = 30 * time.Second

func itConfig() *config.Config {
	getenv := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return &config.Config{
		App: config.AppConfig{Name: "testapp", Environment: "testing"},
		Database: config.DatabaseConfig{
			Host:            getenv("DB_HOST", "localhost"),
			Port:            getenv("DB_PORT", "3306"),
			User:            getenv("DB_USER", "appuser"),
			Password:        getenv("DB_PASSWORD", "apppass"),
			DBName:          getenv("DB_NAME", "app"),
			MaxOpenConns:    8,
			MaxIdleConns:    4,
			ConnMaxLifetime: 5 * time.Minute,
		},
	}
}

// itReplica opens a migrated connection pool of its own: the repository of
// one replica and its raw handle for assertions.
func itReplica(t *testing.T) (models.JobRepository, *gorm.DB) {
	t.Helper()
	d, err := database.NewFromAppConfig(itConfig())
	require.NoError(t, err)
	require.NoError(t, d.AutoMigrate())
	sqlDB, err := d.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	return database.NewGORMJobRepository(d.DB), d.DB
}

// itTypes removes the rows of the given job types at the end of the test (and
// before it, in case an earlier run was killed).
func itTypes(t *testing.T, db *gorm.DB, types ...string) {
	t.Helper()
	del := func() { _ = db.Exec("DELETE FROM jobs WHERE type IN ?", types).Error }
	del()
	t.Cleanup(del)
}

type itRow struct {
	Status    string
	LockedBy  string
	LastError string
	Attempts  int
}

func itLoad(t *testing.T, db *gorm.DB, id uint64) itRow {
	t.Helper()
	var r itRow
	require.NoError(t, db.Raw("SELECT status, locked_by, last_error, attempts FROM jobs WHERE id = ?", id).Scan(&r).Error)
	return r
}

func itWaitStatus(t *testing.T, db *gorm.DB, id uint64, want string) {
	t.Helper()
	require.Eventuallyf(t, func() bool { return itLoad(t, db, id).Status == want }, itWait, 20*time.Millisecond,
		"job %d: want %s, have %s", id, want, itLoad(t, db, id).Status)
}

// itPool starts a pool for one replica and stops it at the end of the test.
func itPool(t *testing.T, repo models.JobRepository, process string, handlers map[string]Handler, mutate ...func(*PoolConfig)) *Pool {
	t.Helper()
	reg := NewRegistry()
	for typ, h := range handlers {
		reg.Register(typ, h)
	}
	cfg := PoolConfig{
		Repo: repo, Registry: reg, ProcessID: process, Workers: 4,
		PollInterval: 20 * time.Millisecond, VisibilityTimeout: 5 * time.Second,
		BackoffBase: 100 * time.Millisecond, BackoffMax: time.Second, StatsInterval: time.Hour,
		DrainTimeout: time.Second, ReleaseTimeout: 2 * time.Second,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p := NewPool(cfg)
	require.NotNil(t, p)
	p.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p
}

// itAwait receives from ch or fails the test after itWait.
func itAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(itWait):
		t.Fatalf("timed out: %s", what)
		panic("unreachable")
	}
}

func itEnqueue(t *testing.T, repo models.JobRepository, typ string, maxAttempts int) uint64 {
	t.Helper()
	id, err := repo.Enqueue(context.Background(), models.JobSpec{Type: typ, Payload: []byte(`{"n":1}`), MaxAttempts: maxAttempts})
	require.NoError(t, err)
	return id
}

// TestDatabasePoolsNeverRunAJobTwiceAtOnce: two replicas of four workers each
// work off 300 jobs. A gauge per job ID counts the handlers that run it at the
// same moment: it never exceeds one, every job runs once and succeeds, and
// both replicas take part (the first handlers wait until both have started).
func TestDatabasePoolsNeverRunAJobTwiceAtOnce(t *testing.T) {
	repoA, db := itReplica(t)
	repoB, _ := itReplica(t)
	const typ, total = "it.once", 300
	itTypes(t, db, typ)

	var (
		mu         sync.Mutex
		active     = map[uint64]int{}
		runs       = map[uint64]int{}
		overlapped []uint64
		started    [2]atomic.Int64
		both       = make(chan struct{})
		bothOnce   sync.Once
	)
	handler := func(replica int) Handler {
		return func(ctx context.Context, job Job) error {
			mu.Lock()
			active[job.ID]++
			if active[job.ID] > 1 {
				overlapped = append(overlapped, job.ID)
			}
			runs[job.ID]++
			mu.Unlock()

			started[replica].Add(1)
			if started[0].Load() > 0 && started[1].Load() > 0 {
				bothOnce.Do(func() { close(both) })
			}
			select {
			case <-both:
			case <-time.After(itWait):
			}
			time.Sleep(time.Millisecond) // widens the window of a double run

			mu.Lock()
			active[job.ID]--
			mu.Unlock()
			return nil
		}
	}

	ids := make([]uint64, total)
	for i := range ids {
		ids[i] = itEnqueue(t, repoA, typ, 3)
	}
	itPool(t, repoA, "replica-a", map[string]Handler{typ: handler(0)})
	itPool(t, repoB, "replica-b", map[string]Handler{typ: handler(1)})

	require.Eventually(t, func() bool {
		var n int64
		require.NoError(t, db.Raw("SELECT COUNT(*) FROM jobs WHERE type = ? AND status = 'succeeded'", typ).Scan(&n).Error)
		return n == total
	}, itWait, 50*time.Millisecond, "all jobs succeed")

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, overlapped, "a job ran twice at the same time")
	assert.Len(t, runs, total)
	for id, n := range runs {
		assert.Equal(t, 1, n, "job %d ran %d times", id, n)
	}
	assert.Positive(t, started[0].Load(), "replica A worked")
	assert.Positive(t, started[1].Load(), "replica B worked")
	var attempts int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM jobs WHERE type = ? AND attempts <> 1", typ).Scan(&attempts).Error)
	assert.Zero(t, attempts, "every job was claimed once")
}

// TestDatabasePoolHeartbeatKeepsALongJobFromBeingTakenOver: a handler that
// runs for longer than two visibility timeouts keeps its job; the pool of the
// other replica, which polls all the time, never claims it.
func TestDatabasePoolHeartbeatKeepsALongJobFromBeingTakenOver(t *testing.T) {
	repoA, db := itReplica(t)
	repoB, _ := itReplica(t)
	const typ = "it.long"
	itTypes(t, db, typ)

	const visibility = 1500 * time.Millisecond
	var runs atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	handler := func(ctx context.Context, job Job) error {
		if runs.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	id := itEnqueue(t, repoA, typ, 3)
	withVisibility := func(c *PoolConfig) { c.VisibilityTimeout = visibility }
	itPool(t, repoA, "replica-a", map[string]Handler{typ: handler}, withVisibility)
	itPool(t, repoB, "replica-b", map[string]Handler{typ: handler}, withVisibility)

	itAwait(t, started, "the handler starts")
	begin := time.Now()
	require.Eventually(t, func() bool { return time.Since(begin) > 2*visibility }, itWait, 50*time.Millisecond)
	row := itLoad(t, db, id)
	assert.Equal(t, "running", row.Status)
	assert.Equal(t, 1, row.Attempts, "nobody took the job over")
	assert.EqualValues(t, 1, runs.Load())

	close(release)
	itWaitStatus(t, db, id, "succeeded")
	assert.EqualValues(t, 1, runs.Load())
}

// TestDatabasePoolRunsTheJobOfACrashedPoolAgain: a pool claimed jobs and died
// without a heartbeat. After the lease the job runs on another pool with the
// next attempt number; one whose attempts were used up by crashes goes dead
// with "lease expired after N attempts" and its handler never runs.
func TestDatabasePoolRunsTheJobOfACrashedPoolAgain(t *testing.T) {
	repoA, db := itReplica(t)
	repoB, _ := itReplica(t)
	const typ = "it.crash"
	itTypes(t, db, typ)
	ctx := context.Background()

	recovered := itEnqueue(t, repoA, typ, 3)
	exhausted := itEnqueue(t, repoA, typ, 2)

	// "Crashed" pool: it claims both (attempt 1) with a short lease, and never
	// reports or extends.
	c, err := repoA.Claim(ctx, []string{"crashed#1", "crashed#2"}, []string{typ}, time.Second)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 2)
	// The second crash of the exhausted job: lease passes, claimed again.
	require.NoError(t, db.Exec("UPDATE jobs SET locked_until = UTC_TIMESTAMP(6) - INTERVAL 1 SECOND WHERE id = ?", exhausted).Error)
	c, err = repoA.Claim(ctx, []string{"crashed#3"}, []string{typ}, time.Second)
	require.NoError(t, err)
	require.Len(t, c.Jobs, 1)
	require.Equal(t, exhausted, c.Jobs[0].ID)
	require.NoError(t, db.Exec("UPDATE jobs SET locked_until = UTC_TIMESTAMP(6) - INTERVAL 1 SECOND WHERE id = ?", exhausted).Error)

	var lockedUntil string
	require.NoError(t, db.Raw("SELECT DATE_FORMAT(locked_until, '%Y-%m-%d %H:%i:%s.%f') FROM jobs WHERE id = ?", recovered).Scan(&lockedUntil).Error)
	require.NotEmpty(t, lockedUntil)

	var (
		mu         sync.Mutex
		ran        = map[uint64]Job{}
		afterLease = map[uint64]bool{}
	)
	handler := func(ctx context.Context, job Job) error {
		var ok bool
		assert.NoError(t, db.Raw("SELECT UTC_TIMESTAMP(6) >= ?", lockedUntil).Scan(&ok).Error)
		mu.Lock()
		ran[job.ID], afterLease[job.ID] = job, ok
		mu.Unlock()
		return nil
	}
	itPool(t, repoB, "replica-b", map[string]Handler{typ: handler})

	itWaitStatus(t, db, recovered, "succeeded")
	itWaitStatus(t, db, exhausted, "dead")

	mu.Lock()
	defer mu.Unlock()
	got, ok := ran[recovered]
	require.True(t, ok, "the job of the crashed pool ran again")
	assert.Equal(t, 2, got.Attempt, "the crash counted as an attempt")
	assert.Equal(t, 3, got.MaxAttempts)
	assert.True(t, afterLease[recovered], "not before its lease ended")
	assert.NotContains(t, ran, exhausted, "its handler never runs again")
	row := itLoad(t, db, exhausted)
	assert.Equal(t, "lease expired after 2 attempts", row.LastError)
	assert.Equal(t, 2, row.Attempts)
	assert.Empty(t, row.LockedBy)
}

// TestDatabaseJobEnqueuedOnOneReplicaRunsOnAnother: replica A enqueues
// through its Queue and has a pool that handles another type only; replica B,
// which handles the type, runs the job with its payload. A job of a type that
// no pool handles yet stays pending until one does.
func TestDatabaseJobEnqueuedOnOneReplicaRunsOnAnother(t *testing.T) {
	repoA, db := itReplica(t)
	repoB, _ := itReplica(t)
	const typ, other = "it.cross", "it.cross-other"
	itTypes(t, db, typ, other)
	ctx := context.Background()

	var aRuns atomic.Int64
	itPool(t, repoA, "replica-a", map[string]Handler{other: func(context.Context, Job) error {
		aRuns.Add(1)
		return nil
	}})
	queue := NewQueue(repoA, 4)

	type payload struct {
		To string `json:"to"`
	}
	id, err := queue.Enqueue(ctx, typ, payload{To: "x@example.com"}, WithMaxAttempts(2))
	require.NoError(t, err)
	otherID, err := queue.Enqueue(ctx, other, payload{To: "o@example.com"})
	require.NoError(t, err)
	itWaitStatus(t, db, otherID, "succeeded") // A is polling, and ignored typ

	assert.Equal(t, "pending", itLoad(t, db, id).Status, "no pool handles the type yet")

	got := make(chan Job, 1)
	itPool(t, repoB, "replica-b", map[string]Handler{typ: func(_ context.Context, job Job) error {
		got <- job
		return nil
	}})
	itWaitStatus(t, db, id, "succeeded")

	job := itAwait(t, got, "the handler runs")
	assert.Equal(t, id, job.ID)
	assert.Equal(t, typ, job.Type)
	assert.JSONEq(t, `{"to":"x@example.com"}`, string(job.Payload))
	assert.Equal(t, 1, job.Attempt)
	assert.Equal(t, 2, job.MaxAttempts)
	assert.EqualValues(t, 1, aRuns.Load(), "A ran only its own type")
}

// TestDatabasePoolRetriesWithBackoffThenSucceeds: a job that fails twice runs
// again after the backoff of each failure (the gap between two runs is at least
// the delay, by the database clock) and then succeeds on its third attempt.
func TestDatabasePoolRetriesWithBackoffThenSucceeds(t *testing.T) {
	repo, db := itReplica(t)
	const typ = "it.retry"
	itTypes(t, db, typ)

	const base = 200 * time.Millisecond
	var mu sync.Mutex
	var starts []time.Time
	var attempts []int
	handler := func(ctx context.Context, job Job) error {
		mu.Lock()
		starts = append(starts, time.Now())
		attempts = append(attempts, job.Attempt)
		n := len(starts)
		mu.Unlock()
		if n < 3 {
			return assert.AnError
		}
		return nil
	}
	id := itEnqueue(t, repo, typ, 5)
	itPool(t, repo, "replica-a", map[string]Handler{typ: handler}, func(c *PoolConfig) {
		c.BackoffBase, c.BackoffMax = base, time.Minute
		c.Rand = func() float64 { return 0 } // no jitter: the delays are base and 2*base
	})
	itWaitStatus(t, db, id, "succeeded")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, starts, 3)
	assert.Equal(t, []int{1, 2, 3}, attempts)
	assert.GreaterOrEqual(t, starts[1].Sub(starts[0]), base-20*time.Millisecond, "first backoff")
	assert.GreaterOrEqual(t, starts[2].Sub(starts[1]), 2*base-20*time.Millisecond, "second backoff doubles")
	row := itLoad(t, db, id)
	assert.Equal(t, 3, row.Attempts)
	assert.Equal(t, assert.AnError.Error(), row.LastError, "the last failure stays visible")
}

// TestDatabasePoolDeadOutcomes: a job that fails on every attempt is dead
// after max_attempts, a Permanent error and a panic need no further attempt
// (the panic retries, its message is the fixed text).
func TestDatabasePoolDeadOutcomes(t *testing.T) {
	repo, db := itReplica(t)
	const exhausted, permanent, panics = "it.dead-exhausted", "it.dead-permanent", "it.dead-panic"
	itTypes(t, db, exhausted, permanent, panics)

	var runs sync.Map
	count := func(typ string) {
		v, _ := runs.LoadOrStore(typ, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	}
	get := func(typ string) int64 {
		v, _ := runs.LoadOrStore(typ, new(atomic.Int64))
		return v.(*atomic.Int64).Load()
	}
	a := itEnqueue(t, repo, exhausted, 3)
	b := itEnqueue(t, repo, permanent, 5)
	c := itEnqueue(t, repo, panics, 2)
	itPool(t, repo, "replica-a", map[string]Handler{
		exhausted: func(context.Context, Job) error { count(exhausted); return assert.AnError },
		permanent: func(context.Context, Job) error {
			count(permanent)
			return Permanent(assert.AnError)
		},
		panics: func(context.Context, Job) error { count(panics); panic("secret-payload-value") },
	}, func(cfg *PoolConfig) { cfg.BackoffBase, cfg.BackoffMax = 10*time.Millisecond, 20*time.Millisecond })

	for _, id := range []uint64{a, b, c} {
		itWaitStatus(t, db, id, "dead")
	}
	ra, rb, rc := itLoad(t, db, a), itLoad(t, db, b), itLoad(t, db, c)
	assert.Equal(t, 3, ra.Attempts)
	assert.EqualValues(t, 3, get(exhausted))
	assert.Equal(t, assert.AnError.Error(), ra.LastError)
	assert.Equal(t, 1, rb.Attempts, "Permanent is dead at once")
	assert.EqualValues(t, 1, get(permanent))
	assert.Equal(t, 2, rc.Attempts, "a panic is a failure that retries")
	assert.EqualValues(t, 2, get(panics))
	assert.Equal(t, panicMessage, rc.LastError)
	assert.NotContains(t, rc.LastError, "secret")
	for _, r := range []itRow{ra, rb, rc} {
		assert.Empty(t, r.LockedBy)
	}

	var finished int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM jobs WHERE id IN ? AND finished_at IS NOT NULL AND locked_until IS NULL", []uint64{a, b, c}).Scan(&finished).Error)
	assert.EqualValues(t, 3, finished)
}

// TestDatabasePoolFencesATakenOverJob: another worker takes over the job
// while the handler runs. The next heartbeat finds out, cancels the job
// context, and whatever the handler returns, the pool writes nothing: the row
// stays with the new owner, who can finish it.
func TestDatabasePoolFencesATakenOverJob(t *testing.T) {
	for name, result := range map[string]func(ctx context.Context) error{
		"handler honours the cancel": func(ctx context.Context) error { return ctx.Err() },
		"handler returns success":    func(context.Context) error { return nil },
		"handler returns an error":   func(context.Context) error { return assert.AnError },
	} {
		t.Run(name, func(t *testing.T) {
			repo, db := itReplica(t)
			const typ = "it.fenced"
			itTypes(t, db, typ)

			started, finished := make(chan struct{}), make(chan struct{})
			var cause atomic.Value
			handler := func(ctx context.Context, job Job) error {
				close(started)
				<-ctx.Done()
				cause.Store(context.Cause(ctx))
				defer close(finished)
				return result(ctx)
			}
			id := itEnqueue(t, repo, typ, 3)
			p := itPool(t, repo, "replica-a", map[string]Handler{typ: handler}, func(c *PoolConfig) {
				c.VisibilityTimeout = 600 * time.Millisecond // beat every 200 ms
			})
			itAwait(t, started, "the handler starts")

			// The thief holds a lease of an hour, so nobody claims it again.
			require.NoError(t, db.Exec("UPDATE jobs SET locked_by = 'thief#1', attempts = attempts + 1, locked_until = UTC_TIMESTAMP(6) + INTERVAL 1 HOUR WHERE id = ?", id).Error)

			select {
			case <-finished:
			case <-time.After(itWait):
				t.Fatal("the heartbeat did not cancel the job context")
			}
			assert.Equal(t, errLeaseLost, cause.Load())

			// Stop waits for the worker, so its report (dropped) is done.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, p.Stop(ctx))

			row := itLoad(t, db, id)
			assert.Equal(t, "running", row.Status)
			assert.Equal(t, "thief#1", row.LockedBy)
			assert.Equal(t, 2, row.Attempts)
			assert.Empty(t, row.LastError, "the late result left no trace")

			ok, err := repo.Complete(ctx, id, "thief#1")
			require.NoError(t, err)
			assert.True(t, ok, "the new owner finishes the job")
		})
	}
}

// TestDatabasePoolStopReleasesUnfinishedJobs: Stop cancels a handler that
// honours its context and gives the job back, the attempt not counted, so the
// next pool runs it as attempt 1. A handler that ignores the cancel does not
// block the release; its late result is dropped by the fence.
func TestDatabasePoolStopReleasesUnfinishedJobs(t *testing.T) {
	t.Run("handler honours the cancel", func(t *testing.T) {
		repoA, db := itReplica(t)
		repoB, _ := itReplica(t)
		const typ = "it.stop"
		itTypes(t, db, typ)

		started := make(chan struct{})
		var once sync.Once
		id := itEnqueue(t, repoA, typ, 3)
		pa := itPool(t, repoA, "replica-a", map[string]Handler{typ: func(ctx context.Context, job Job) error {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ctx.Err()
		}}, func(c *PoolConfig) { c.DrainTimeout = 200 * time.Millisecond })
		itAwait(t, started, "the handler starts")

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		require.NoError(t, pa.Stop(ctx))

		row := itLoad(t, db, id)
		assert.Equal(t, "pending", row.Status)
		assert.Zero(t, row.Attempts, "a release does not count")
		assert.Empty(t, row.LockedBy)
		assert.Empty(t, row.LastError, "an interrupted job is not a failed one")

		got := make(chan Job, 1)
		itPool(t, repoB, "replica-b", map[string]Handler{typ: func(_ context.Context, job Job) error {
			got <- job
			return nil
		}})
		itWaitStatus(t, db, id, "succeeded")
		assert.Equal(t, 1, itAwait(t, got, "the handler runs").Attempt)
	})

	t.Run("handler ignores the cancel", func(t *testing.T) {
		repo, db := itReplica(t)
		const typ = "it.stop-stuck"
		itTypes(t, db, typ)

		started, unblock := make(chan struct{}), make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(started) }); closeOnce(unblock) })
		id := itEnqueue(t, repo, typ, 3)
		p := itPool(t, repo, "replica-a", map[string]Handler{typ: func(context.Context, Job) error {
			once.Do(func() { close(started) })
			<-unblock
			return nil
		}}, func(c *PoolConfig) {
			c.DrainTimeout, c.ReleaseTimeout = 200*time.Millisecond, 600*time.Millisecond
		})
		itAwait(t, started, "the handler starts")
		p.mu.Lock()
		run := p.current
		p.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		require.Error(t, p.Stop(ctx), "a stuck handler is reported")
		row := itLoad(t, db, id)
		assert.Equal(t, "pending", row.Status, "released anyway")
		assert.Zero(t, row.Attempts)

		closeOnce(unblock)
		run.workers.Wait() // the handler returned and its late Complete ran
		row = itLoad(t, db, id)
		assert.Equal(t, "pending", row.Status, "the late success was dropped by the fence")
		assert.Empty(t, row.LockedBy)
	})
}

// closeOnce closes ch unless it is closed already.
func closeOnce(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}

// itCounting counts the batches of the janitor.
type itCounting struct {
	models.JobRepository
	mu      sync.Mutex
	deletes []int64
	status  []string
}

func (c *itCounting) DeleteFinishedBefore(ctx context.Context, status string, maxAge time.Duration, limit int) (int64, error) {
	n, err := c.JobRepository.DeleteFinishedBefore(ctx, status, maxAge, limit)
	c.mu.Lock()
	c.deletes, c.status = append(c.deletes, n), append(c.status, status)
	c.mu.Unlock()
	return n, err
}

// TestDatabaseJanitorDeletesByRetentionInBatches: succeeded rows older than
// the retention and dead rows older than the dead retention go, in batches of
// 1000; younger rows, pending and running jobs stay.
func TestDatabaseJanitorDeletesByRetentionInBatches(t *testing.T) {
	repo, db := itReplica(t)
	const typ = "it.janitor"
	itTypes(t, db, typ)
	// Rows left by another package would make the batch counts differ.
	require.NoError(t, db.Exec("DELETE FROM jobs WHERE status IN ('succeeded', 'dead') AND finished_at < UTC_TIMESTAMP(6) - INTERVAL 1 HOUR").Error)

	now := time.Now().UTC()
	mk := func(n int, status string) {
		rows := make([]models.Job, n)
		for i := range rows {
			rows[i] = models.Job{Type: typ, Status: status, Payload: "{}", MaxAttempts: 3, RunAt: now, CreatedAt: now, UpdatedAt: now}
		}
		require.NoError(t, db.CreateInBatches(&rows, 500).Error)
	}
	// Created in this order so that each group gets its own finished_at below.
	age := func(status string, sql string) {
		require.NoError(t, db.Exec("UPDATE jobs SET finished_at = "+sql+" WHERE type = ? AND status = ? AND finished_at IS NULL", typ, status).Error)
	}
	mk(2500, "succeeded")
	age("succeeded", "UTC_TIMESTAMP(6) - INTERVAL 2 HOUR") // retention 1h: all deleted
	mk(30, "succeeded")
	age("succeeded", "UTC_TIMESTAMP(6) - INTERVAL 10 MINUTE") // kept
	mk(7, "dead")
	age("dead", "UTC_TIMESTAMP(6) - INTERVAL 48 HOUR") // dead retention 24h: deleted
	mk(5, "dead")
	age("dead", "UTC_TIMESTAMP(6) - INTERVAL 10 HOUR") // kept
	mk(4, "pending")
	mk(3, "running")

	counting := &itCounting{JobRepository: repo}
	require.NoError(t, Cleanup(counting, time.Hour, 24*time.Hour)(context.Background()))

	assert.Equal(t, []int64{1000, 1000, 500, 7}, counting.deletes, "batches of 1000, a short one ends the status")
	assert.Equal(t, []string{"succeeded", "succeeded", "succeeded", "dead"}, counting.status)

	count := func(status string) (n int64) {
		require.NoError(t, db.Raw("SELECT COUNT(*) FROM jobs WHERE type = ? AND status = ?", typ, status).Scan(&n).Error)
		return n
	}
	assert.EqualValues(t, 30, count("succeeded"))
	assert.EqualValues(t, 5, count("dead"))
	assert.EqualValues(t, 4, count("pending"))
	assert.EqualValues(t, 3, count("running"))

	// A second run finds nothing and is not an error.
	require.NoError(t, Cleanup(repo, time.Hour, 24*time.Hour)(context.Background()))
	assert.EqualValues(t, 30, count("succeeded"))
}

// TestDatabaseJanitorStopsWhenItsContextEnds: the run returns the context's
// error before it deletes more.
func TestDatabaseJanitorStopsWhenItsContextEnds(t *testing.T) {
	repo, _ := itReplica(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	counting := &itCounting{JobRepository: repo}
	err := Cleanup(counting, time.Hour, time.Hour)(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, counting.deletes)
}

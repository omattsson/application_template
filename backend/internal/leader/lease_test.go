package leader

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// testLeaseConfig is short enough for unit tests and long enough that the
// margins below hold under -race on a loaded machine: a holder that cannot
// renew stops after RenewDeadline (450 ms), 450 ms before its lease expires.
var testLeaseConfig = LeaseConfig{
	Duration:      900 * time.Millisecond,
	RenewDeadline: 450 * time.Millisecond,
	RetryPeriod:   75 * time.Millisecond,
}

// faultMode is what the fault injector does to every statement of one
// replica's connection pool.
type faultMode int32

const (
	faultNone faultMode = iota
	// faultFail fails every statement at once (the database refuses).
	faultFail
	// faultHang blocks every statement until its context ends (the
	// database is unreachable and the network does not answer).
	faultHang
)

var errInjected = errors.New("injected database failure")

// replicaDB is the connection pool of one replica on the shared test
// database, with a fault injector.
type replicaDB struct {
	db   *gorm.DB
	mode atomic.Int32
}

func (r *replicaDB) set(m faultMode) { r.mode.Store(int32(m)) }

func (r *replicaDB) inject(db *gorm.DB) {
	switch faultMode(r.mode.Load()) {
	case faultFail:
		_ = db.AddError(errInjected)
	case faultHang:
		ctx := db.Statement.Context
		<-ctx.Done()
		_ = db.AddError(ctx.Err())
	case faultNone:
	}
}

// sharedDB is one SQLite database file that several replicas open with
// pools of their own, as replicas share one MySQL server.
type sharedDB struct {
	path string
}

func newSharedDB(t *testing.T) *sharedDB {
	t.Helper()
	s := &sharedDB{path: filepath.Join(t.TempDir(), "lease.db")}
	r := s.open(t)
	require.NoError(t, r.db.AutoMigrate(&LeaseRecord{}))
	return s
}

// open returns a new pool on the database, closed at the end of the test.
func (s *sharedDB) open(t *testing.T) *replicaDB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", s.path)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	r := &replicaDB{db: db}
	require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register("test:fault", r.inject))
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register("test:fault", r.inject))
	return r
}

// holderOf returns the holder column of the lease row ("" without a row).
func (s *sharedDB) holderOf(t *testing.T, r *replicaDB, name string) string {
	t.Helper()
	var holders []string
	require.NoError(t, r.db.Raw("SELECT holder FROM leader_leases WHERE name = ?", name).Scan(&holders).Error)
	if len(holders) == 0 {
		return ""
	}
	return holders[0]
}

// term records one leadership term of a replica.
type term struct {
	started time.Time
	stopped time.Time
}

// tracker records the callbacks of all replicas of a test and the largest
// number of replicas that were leaders at the same time.
type tracker struct {
	terms     map[string][]*term
	changed   chan struct{}
	mu        sync.Mutex
	active    int
	maxActive int
}

func newTracker() *tracker {
	return &tracker{terms: map[string][]*term{}, changed: make(chan struct{}, 100)}
}

func (tr *tracker) notify() {
	select {
	case tr.changed <- struct{}{}:
	default:
	}
}

// callbacks returns onStarted and onStopped for replica id. onStarted blocks
// until its ctx ends, like a leader callback that runs for the whole term, so
// the test also checks that the ctx is cancelled when leadership ends.
func (tr *tracker) callbacks(t *testing.T, id string) (func(context.Context), func()) {
	t.Helper()
	var startedFlag atomic.Bool
	onStarted := func(ctx context.Context) {
		tr.mu.Lock()
		tr.active++
		if tr.active > tr.maxActive {
			tr.maxActive = tr.active
		}
		tr.terms[id] = append(tr.terms[id], &term{started: time.Now()})
		tr.mu.Unlock()
		startedFlag.Store(true)
		tr.notify()
		<-ctx.Done()
	}
	onStopped := func() {
		assert.True(t, startedFlag.Swap(false), "%s: onStopped without a started term", id)
		tr.mu.Lock()
		tr.active--
		ts := tr.terms[id]
		ts[len(ts)-1].stopped = time.Now()
		tr.mu.Unlock()
		tr.notify()
	}
	return onStarted, onStopped
}

// snapshot returns the terms of id (copies) and the max active count.
func (tr *tracker) snapshot(id string) ([]term, int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]term, 0, len(tr.terms[id]))
	for _, x := range tr.terms[id] {
		out = append(out, *x)
	}
	return out, tr.maxActive
}

func (tr *tracker) leaders() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var ids []string
	for id, ts := range tr.terms {
		if len(ts) > 0 && ts[len(ts)-1].stopped.IsZero() {
			ids = append(ids, id)
		}
	}
	return ids
}

// waitFor waits until cond holds or the timeout ends.
func (tr *tracker) waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out after %s: %s", timeout, msg)
		case <-tr.changed:
		case <-tick.C:
		}
	}
}

// replica is one running Lease.Run.
type replica struct {
	db     *replicaDB
	cancel context.CancelFunc
	done   chan struct{}
	id     string
}

func startReplica(t *testing.T, tr *tracker, db *replicaDB, name, id string, cfg LeaseConfig) *replica {
	t.Helper()
	l := NewLease(db.db, name, id, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{id: id, db: db, cancel: cancel, done: make(chan struct{})}
	onStarted, onStopped := tr.callbacks(t, id)
	go func() {
		defer close(r.done)
		l.Run(ctx, onStarted, onStopped)
	}()
	t.Cleanup(func() { r.stop(t) })
	return r
}

// stop cancels Run and waits until it returned (no goroutine is left).
func (r *replica) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: Run did not return after its context was cancelled", r.id)
	}
}

func TestLeaseConfigValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		wantErr string
		cfg     LeaseConfig
	}{
		{name: "defaults", cfg: DefaultLeaseConfig()},
		{name: "test values", cfg: testLeaseConfig},
		{name: "zero duration", cfg: LeaseConfig{RenewDeadline: time.Second, RetryPeriod: time.Millisecond}, wantErr: "positive"},
		{name: "negative retry", cfg: LeaseConfig{Duration: 2 * time.Second, RenewDeadline: time.Second, RetryPeriod: -1}, wantErr: "positive"},
		{name: "renew deadline equals duration", cfg: LeaseConfig{Duration: time.Second, RenewDeadline: time.Second, RetryPeriod: time.Millisecond}, wantErr: "RenewDeadline"},
		{name: "retry equals renew deadline", cfg: LeaseConfig{Duration: 2 * time.Second, RenewDeadline: time.Second, RetryPeriod: time.Second}, wantErr: "RetryPeriod"},
		{name: "below a millisecond", cfg: LeaseConfig{Duration: 3 * time.Millisecond, RenewDeadline: 2 * time.Millisecond, RetryPeriod: 500 * time.Microsecond}, wantErr: "millisecond"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestNewLeasePanicsOnInvalidConfig(t *testing.T) {
	t.Parallel()
	db := newSharedDB(t).open(t)
	assert.Panics(t, func() {
		NewLease(db.db, "x", "a", LeaseConfig{Duration: time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: time.Millisecond})
	})
	assert.Panics(t, func() { NewLease(db.db, "", "a", testLeaseConfig) }, "empty name")
	assert.Panics(t, func() { NewLease(db.db, "x", "", testLeaseConfig) }, "empty holder")
	assert.Panics(t, func() { NewLease(nil, "x", "a", testLeaseConfig) }, "nil db")
	l := NewLease(db.db, "x", "a", LeaseConfig{})
	assert.Equal(t, DefaultLeaseConfig(), l.cfg, "a zero config uses the defaults")
	assert.Equal(t, "a", l.Holder())
}

// Two replicas on one database: exactly one becomes leader and stays leader
// while it renews.
func TestLeaseTwoReplicasOneLeader(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	tr := newTracker()
	a := startReplica(t, tr, shared.open(t), "workers", "a", testLeaseConfig)
	b := startReplica(t, tr, shared.open(t), "workers", "b", testLeaseConfig)

	tr.waitFor(t, 2*time.Second, "one leader", func() bool { return len(tr.leaders()) == 1 })
	// Several lease durations: the leader renews, the other never starts.
	time.Sleep(3 * testLeaseConfig.Duration)

	leaders := tr.leaders()
	require.Len(t, leaders, 1)
	termsA, maxActive := tr.snapshot("a")
	termsB, _ := tr.snapshot("b")
	assert.Equal(t, 1, len(termsA)+len(termsB), "exactly one onStarted in total")
	assert.Equal(t, 1, maxActive)
	assert.Equal(t, leaders[0], shared.holderOf(t, a.db, "workers"))
	_ = b
}

// Many replicas start at the same moment on an empty table: the inserts race
// and the losers of the duplicate key are not leaders.
func TestLeaseConcurrentFirstAcquire(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	tr := newTracker()
	for i := range 5 {
		startReplica(t, tr, shared.open(t), "race", fmt.Sprintf("r%d", i), testLeaseConfig)
	}
	tr.waitFor(t, 2*time.Second, "one leader", func() bool { return len(tr.leaders()) == 1 })
	time.Sleep(2 * testLeaseConfig.Duration)
	_, maxActive := tr.snapshot("r0")
	assert.Equal(t, 1, maxActive)
	assert.Len(t, tr.leaders(), 1)
}

// Leases of different names are independent.
func TestLeaseNamesAreIndependent(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	tr := newTracker()
	startReplica(t, tr, shared.open(t), "one", "a", testLeaseConfig)
	startReplica(t, tr, shared.open(t), "two", "b", testLeaseConfig)
	tr.waitFor(t, 2*time.Second, "both leaders", func() bool { return len(tr.leaders()) == 2 })
}

// The holder stops renewing (its database fails): it stops leading after
// RenewDeadline, before its lease expires, and the other replica takes over
// within Duration + RetryPeriod of the last renewal.
func TestLeaseTakeoverAfterHolderStopsRenewing(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	tr := newTracker()
	a := startReplica(t, tr, shared.open(t), "workers", "a", testLeaseConfig)
	tr.waitFor(t, 2*time.Second, "a leads", func() bool { return len(tr.leaders()) == 1 })
	b := startReplica(t, tr, shared.open(t), "workers", "b", testLeaseConfig)
	time.Sleep(2 * testLeaseConfig.RetryPeriod)
	require.Equal(t, []string{"a"}, tr.leaders())

	broken := time.Now() // a's last successful renewal is before this
	a.db.set(faultFail)

	tr.waitFor(t, 3*time.Second, "b takes over", func() bool {
		ts, _ := tr.snapshot("b")
		return len(ts) == 1
	})
	termsA, maxActive := tr.snapshot("a")
	termsB, _ := tr.snapshot("b")
	require.Len(t, termsA, 1)
	require.False(t, termsA[0].stopped.IsZero(), "a stopped leading")
	assert.Less(t, termsA[0].stopped.Sub(broken), testLeaseConfig.Duration, "a stops before its lease can expire")
	assert.True(t, termsA[0].stopped.Before(termsB[0].started), "a stops before b starts")
	// b polls every RetryPeriod; 200 ms covers statement latency under -race.
	assert.Less(t, termsB[0].started.Sub(broken), testLeaseConfig.Duration+testLeaseConfig.RetryPeriod+200*time.Millisecond)
	assert.Equal(t, 1, maxActive)
	assert.Equal(t, "b", shared.holderOf(t, b.db, "workers"))
}

// The database becomes unreachable (every statement hangs): the per-call
// timeout ends the hanging renewal, and the holder calls onStopped before
// Duration has passed since its last renewal. The other replica starts
// only after that.
func TestLeaseUnreachableDatabaseStopsHolderBeforeExpiry(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	tr := newTracker()
	a := startReplica(t, tr, shared.open(t), "workers", "a", testLeaseConfig)
	tr.waitFor(t, 2*time.Second, "a leads", func() bool { return len(tr.leaders()) == 1 })
	startReplica(t, tr, shared.open(t), "workers", "b", testLeaseConfig)
	time.Sleep(2 * testLeaseConfig.RetryPeriod)

	hung := time.Now()
	a.db.set(faultHang)

	tr.waitFor(t, 3*time.Second, "a stops", func() bool {
		ts, _ := tr.snapshot("a")
		return !ts[0].stopped.IsZero()
	})
	termsA, _ := tr.snapshot("a")
	stoppedAfter := termsA[0].stopped.Sub(hung)
	assert.Less(t, stoppedAfter, testLeaseConfig.Duration, "onStopped before the lease can expire")
	assert.GreaterOrEqual(t, stoppedAfter, testLeaseConfig.RenewDeadline-2*testLeaseConfig.RetryPeriod, "not before the renew deadline")

	tr.waitFor(t, 3*time.Second, "b takes over", func() bool {
		ts, _ := tr.snapshot("b")
		return len(ts) == 1
	})
	termsB, maxActive := tr.snapshot("b")
	assert.True(t, termsA[0].stopped.Before(termsB[0].started))
	assert.Equal(t, 1, maxActive)

	// Run of the hung replica still returns at once on cancel: the release
	// is bounded too.
	start := time.Now()
	a.stop(t)
	assert.Less(t, time.Since(start), releaseTimeout+time.Second)
}

// A deposed holder (the row shows another holder) stops at once, campaigns
// again and wins after the new leader stops. The new leader is a row written
// by the test (another process whose acquisition a did not see), so no
// second callback set runs while a is still leading.
func TestLeaseDeposedHolderCampaignsAgain(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	tr := newTracker()
	a := startReplica(t, tr, shared.open(t), "workers", "a", testLeaseConfig)
	tr.waitFor(t, 2*time.Second, "a leads", func() bool { return len(tr.leaders()) == 1 })
	admin := shared.open(t)

	// Another holder with a valid lease: a's next renewal sees it.
	deposed := time.Now()
	require.NoError(t, admin.db.Exec("UPDATE leader_leases SET holder = 'other', expires_at = '2999-01-01 00:00:00.000' WHERE name = 'workers'").Error)

	tr.waitFor(t, 2*time.Second, "a deposed", func() bool {
		ts, _ := tr.snapshot("a")
		return !ts[0].stopped.IsZero()
	})
	termsA, _ := tr.snapshot("a")
	assert.Less(t, termsA[0].stopped.Sub(deposed), testLeaseConfig.RenewDeadline, "a deposed holder stops at once, not at the renew deadline")

	// a campaigns but cannot win while the other holder's lease is valid.
	time.Sleep(3 * testLeaseConfig.RetryPeriod)
	termsA, _ = tr.snapshot("a")
	assert.Len(t, termsA, 1)
	assert.Equal(t, "other", shared.holderOf(t, admin, "workers"))

	// The new leader stops (releases): a campaigns again and wins.
	require.NoError(t, admin.db.Exec("UPDATE leader_leases SET expires_at = '2000-01-01 00:00:00.000' WHERE name = 'workers'").Error)
	tr.waitFor(t, 2*time.Second, "a leads again", func() bool {
		ts, _ := tr.snapshot("a")
		return len(ts) == 2 && ts[1].stopped.IsZero()
	})
	termsA, maxActive := tr.snapshot("a")
	assert.Len(t, termsA, 2)
	assert.Equal(t, 1, maxActive)
	assert.Equal(t, "a", shared.holderOf(t, a.db, "workers"))
}

// On cancel the holder calls onStopped and releases the lease, so the next
// replica takes over long before the lease would have expired.
func TestLeaseReleaseOnCancel(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	tr := newTracker()
	a := startReplica(t, tr, shared.open(t), "workers", "a", testLeaseConfig)
	tr.waitFor(t, 2*time.Second, "a leads", func() bool { return len(tr.leaders()) == 1 })
	startReplica(t, tr, shared.open(t), "workers", "b", testLeaseConfig)
	time.Sleep(2 * testLeaseConfig.RetryPeriod)

	cancelled := time.Now()
	a.stop(t) // Run returned: onStopped ran and the lease is released
	termsA, _ := tr.snapshot("a")
	require.Len(t, termsA, 1)
	assert.False(t, termsA[0].stopped.IsZero(), "onStopped ran before Run returned")

	tr.waitFor(t, 2*time.Second, "b takes over", func() bool {
		ts, _ := tr.snapshot("b")
		return len(ts) == 1
	})
	termsB, _ := tr.snapshot("b")
	assert.Less(t, termsB[0].started.Sub(cancelled), testLeaseConfig.Duration, "the release lets b in before expiry")
}

// A replica that never became leader returns from Run on cancel without
// calling either callback, also when the database is gone.
func TestLeaseClosedDatabase(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	r := shared.open(t)
	sqlDB, err := r.db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	l := NewLease(r.db, "workers", "a", testLeaseConfig)
	ok, holder, err := l.tryAcquire(context.Background())
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, holder)
	require.Error(t, l.release(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx,
			func(context.Context) { t.Error("onStarted without the lease") },
			func() { t.Error("onStopped without a term") })
	}()
	time.Sleep(3 * testLeaseConfig.RetryPeriod)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
}

// Each SQL path reports a database error: the update, the read and the
// insert of tryAcquire.
func TestLeaseTryAcquireErrors(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	r := shared.open(t)
	l := NewLease(r.db, "workers", "a", testLeaseConfig)
	ctx := context.Background()

	// Fail only the n-th statement of the next tryAcquire.
	var calls, failAt atomic.Int32
	require.NoError(t, r.db.Callback().Raw().Before("gorm:raw").Register("test:nth", func(db *gorm.DB) {
		if calls.Add(1) == failAt.Load() {
			_ = db.AddError(errInjected)
		}
	}))
	require.NoError(t, r.db.Callback().Row().Before("gorm:row").Register("test:nth", func(db *gorm.DB) {
		if calls.Add(1) == failAt.Load() {
			_ = db.AddError(errInjected)
		}
	}))
	for n := int32(1); n <= 3; n++ {
		calls.Store(0)
		failAt.Store(n)
		ok, _, err := l.tryAcquire(ctx)
		require.ErrorIs(t, err, errInjected, "statement %d", n)
		assert.False(t, ok)
	}
	failAt.Store(0)
	calls.Store(0)
	ok, holder, err := l.tryAcquire(ctx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "a", holder)

	// Another holder: a sees it and does not acquire.
	other := NewLease(r.db, "workers", "b", testLeaseConfig)
	ok, holder, err = other.tryAcquire(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "a", holder)

	// Renewing in the same millisecond changes nothing in the row and is
	// still a renewal (MySQL reports 0 changed rows then).
	ok, _, err = l.tryAcquire(ctx)
	require.NoError(t, err)
	assert.True(t, ok)

	// After the release the other replica acquires at once.
	require.NoError(t, l.release(ctx))
	ok, holder, err = other.tryAcquire(ctx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "b", holder)
}

// An UPDATE that reports 0 rows (MySQL counts changed rows, so a renewal
// that changes nothing gives 0) is still a renewal when the row shows this
// holder; another holder in the row is not.
func TestLeaseZeroRowsAffectedStillHeld(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	r := shared.open(t)
	ctx := context.Background()
	l := NewLease(r.db, "workers", "a", testLeaseConfig)
	ok, _, err := l.tryAcquire(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	var zero atomic.Bool
	require.NoError(t, r.db.Callback().Raw().After("gorm:raw").Register("test:zero", func(db *gorm.DB) {
		if zero.Load() {
			db.RowsAffected = 0
		}
	}))
	zero.Store(true)
	ok, holder, err := l.tryAcquire(ctx)
	zero.Store(false)
	require.NoError(t, err)
	assert.True(t, ok, "the row still names this holder")
	assert.Equal(t, "a", holder)
}

// The insert loses a duplicate-key race: not acquired, no error.
func TestLeaseInsertDuplicateIsNotAcquired(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	r := shared.open(t)
	l := NewLease(r.db, "workers", "a", testLeaseConfig)
	other := NewLease(r.db, "workers", "b", testLeaseConfig)
	ctx := context.Background()
	ok, err := other.insert(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = l.insert(ctx)
	require.NoError(t, err, "a duplicate key is a lost race, not an error")
	assert.False(t, ok)
}

// The SQL time expressions: MySQL uses the server clock in UTC; the SQLite
// form (tests) is a sortable text with milliseconds.
func TestLeaseDialectSQL(t *testing.T) {
	t.Parallel()
	my := sqlFor("mysql")
	assert.Contains(t, my.acquire, "UTC_TIMESTAMP(3)")
	assert.Contains(t, my.insert, "UTC_TIMESTAMP(3)")
	assert.Contains(t, my.release, "UTC_TIMESTAMP(3)")
	assert.Equal(t, int64(1_500_000), my.offset(1500*time.Millisecond))
	lite := sqlFor("sqlite")
	assert.Contains(t, lite.acquire, "strftime")
	assert.Equal(t, "+1.500 seconds", lite.offset(1500*time.Millisecond))
	assert.Equal(t, "-1.000 seconds", lite.offset(-time.Second))
}

// onStarted runs in its own goroutine (Run keeps renewing while it blocks),
// and its ctx ends before onStopped is called.
func TestLeaseOnStartedContextEndsBeforeOnStopped(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	r := shared.open(t)
	l := NewLease(r.db, "workers", "a", testLeaseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	var termCtx atomic.Value
	started := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx, func(c context.Context) {
			termCtx.Store(c)
			close(started)
		}, func() {
			c, _ := termCtx.Load().(context.Context)
			assert.Error(t, c.Err(), "the term context is cancelled before onStopped")
			close(stopped)
		})
	}()
	<-started
	// Renewals go on after onStarted returned.
	time.Sleep(3 * testLeaseConfig.Duration)
	c, _ := termCtx.Load().(context.Context)
	require.NoError(t, c.Err(), "still leader")
	cancel()
	<-done
	<-stopped
}

// An acquisition answered so late that the first renewal would be due after
// the renew deadline is not used: the term would stop at once. The next
// attempt acquires again (the row names this holder) and starts one stable
// term.
func TestLeaseLateAcquisitionIsNotUsed(t *testing.T) {
	t.Parallel()
	shared := newSharedDB(t)
	r := shared.open(t)
	var slowOnce atomic.Bool
	slowOnce.Store(true)
	delay := testLeaseConfig.RenewDeadline - testLeaseConfig.RetryPeriod/2
	require.NoError(t, r.db.Callback().Raw().Before("gorm:raw").Register("test:slow", func(*gorm.DB) {
		if slowOnce.Swap(false) {
			time.Sleep(delay) // the first UPDATE answers just before RenewDeadline
		}
	}))
	tr := newTracker()
	start := time.Now()
	startReplica(t, tr, r, "workers", "a", testLeaseConfig)
	tr.waitFor(t, 3*time.Second, "a leads", func() bool { return len(tr.leaders()) == 1 })
	terms, _ := tr.snapshot("a")
	assert.GreaterOrEqual(t, terms[0].started.Sub(start), delay+testLeaseConfig.RetryPeriod, "the late answer did not start a term")
	time.Sleep(3 * testLeaseConfig.RetryPeriod)
	terms, _ = tr.snapshot("a")
	assert.Len(t, terms, 1, "no term that stops at once")
	assert.True(t, terms[0].stopped.IsZero())
}

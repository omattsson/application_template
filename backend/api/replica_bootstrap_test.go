package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/leader"
	"backend/internal/models"
	"backend/internal/replica"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHeartbeat records the stop of the replica heartbeat.
type fakeHeartbeat struct {
	rec     *callRecorder
	timeout time.Duration
}

func (f *fakeHeartbeat) Stop(timeout time.Duration) {
	f.timeout = timeout
	f.rec.add("replica heartbeat")
}

// The heartbeat row is deleted after everything else stopped and before the
// database closes, within its own fixed budget.
func TestShutdownStopsHeartbeatBeforeDatabase(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	hb := &fakeHeartbeat{rec: rec}
	s.heartbeat = hb
	s.sessionStore = &fakeStore{rec: rec}

	require.NoError(t, shutdown(context.Background(), s))
	assert.Equal(t, []string{
		"server", "leader group", "in-flight", "hub", "telemetry", "rate limiters", "session store", "replica heartbeat", "database",
	}, rec.list())
	assert.Equal(t, heartbeatStopTimeout, hb.timeout)

	// A typed nil heartbeat (disabled) is skipped.
	rec2 := &callRecorder{}
	s2 := fakeServices(rec2)
	s2.heartbeat = (*replica.Heartbeat)(nil)
	require.NoError(t, shutdown(context.Background(), s2))
	assert.NotContains(t, rec2.list(), "replica heartbeat")
}

// fakeLease is a leader election that grants leadership at once and holds
// it until ctx ends, like leader.Lease.Run.
type fakeLease struct {
	block    chan struct{}
	mu       sync.Mutex
	stopped  int
	released bool
	// ignoreCancel makes Run never return (a hung release).
	ignoreCancel bool
}

func (f *fakeLease) Run(ctx context.Context, onStarted func(context.Context), onStopped func()) {
	termCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	onStarted(termCtx)
	<-ctx.Done()
	cancel()
	onStopped()
	f.mu.Lock()
	f.stopped++
	f.mu.Unlock()
	if f.ignoreCancel {
		<-f.block
	}
	f.mu.Lock()
	f.released = true
	f.mu.Unlock()
}

func waitUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestElectedLeaderStartsAndStopsTheGroup(t *testing.T) {
	t.Parallel()
	runs := make(chan struct{}, 10)
	g := leader.NewGroup(time.Second, leader.Worker{Name: "w", Run: func(ctx context.Context) {
		runs <- struct{}{}
		<-ctx.Done()
	}})
	lease := &fakeLease{}
	e := startElection(g, lease)
	select {
	case <-runs:
	case <-time.After(2 * time.Second):
		t.Fatal("the workers did not start with the leadership")
	}
	assert.True(t, g.Running())

	assert.True(t, e.Close(time.Second))
	assert.False(t, g.Running())
	lease.mu.Lock()
	assert.Equal(t, 1, lease.stopped)
	assert.True(t, lease.released, "Close waits for the release")
	lease.mu.Unlock()
	assert.True(t, e.Close(time.Second), "a second Close is a no-op")
}

// Close stays within its timeout when the election does not end.
func TestElectedLeaderCloseIsBounded(t *testing.T) {
	t.Parallel()
	g := leader.NewGroup(time.Second)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	lease := &fakeLease{ignoreCancel: true, block: block}
	e := startElection(g, lease)
	waitUntil(t, 2*time.Second, "leader", g.Running)

	start := time.Now()
	assert.False(t, e.Close(150*time.Millisecond))
	assert.Less(t, time.Since(start), time.Second)
}

// With LEADER_ELECTION_ENABLED=true the group starts only when this process
// holds the lease row, and shutdown releases it.
func TestBootstrapLeaderElection(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := openSQLiteRepo(t)
	require.NoError(t, db.AutoMigrate(&leader.LeaseRecord{}, &models.ReplicaHeartbeat{}))
	cfg := authModeConfig(true)
	cfg.Replica.LeaderElectionEnabled = true
	cfg.Replica.HeartbeatEnabled = true

	s, router, err := buildServices(cfg, repo)
	require.NoError(t, err)
	require.NotNil(t, router)
	e, ok := s.leader.(*electedLeader)
	require.True(t, ok, "the group runs under the election")
	assert.Contains(t, e.group.Names(), "replica-heartbeat-cleanup")
	waitUntil(t, 5*time.Second, "this replica becomes leader", e.group.Running)

	var holder string
	require.NoError(t, db.Raw("SELECT holder FROM leader_leases WHERE name = ?", leaderLeaseName).Scan(&holder).Error)
	hb, ok := s.heartbeat.(*replica.Heartbeat)
	require.True(t, ok)
	assert.Equal(t, hb.ID(), holder, "the holder is the process ID")
	host := strings.SplitN(holder, "-", 2)[0]
	assert.NotEmpty(t, host)

	waitUntil(t, 2*time.Second, "heartbeat row", func() bool {
		var n int64
		_ = db.Model(&models.ReplicaHeartbeat{}).Where("id = ?", hb.ID()).Count(&n).Error
		return n == 1
	})

	// The leader step releases the lease (expiry in the past) before the
	// database closes.
	require.True(t, s.leader.Close(5*time.Second))
	var expired int64
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM leader_leases WHERE name = ? AND expires_at < strftime('%Y-%m-%d %H:%M:%f', 'now')", leaderLeaseName).Scan(&expired).Error)
	assert.Equal(t, int64(1), expired, "the lease is released")

	s.heartbeat.Stop(time.Second)
	var n int64
	require.NoError(t, db.Model(&models.ReplicaHeartbeat{}).Count(&n).Error)
	assert.Zero(t, n, "the heartbeat row is deleted on stop")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx, s))
}

// With both switches off nothing changes: the group runs at once and there
// is no heartbeat.
func TestBootstrapReplicaFeaturesOff(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, _ := openSQLiteRepo(t)
	s, _ := startServices(t, authModeConfig(false), repo)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.True(t, g.Running())
	assert.True(t, isNilInterface(s.heartbeat))
	assert.NotContains(t, g.Names(), "replica-heartbeat-cleanup")
}

// Leader election and the heartbeat need the database of the repository.
func TestBootstrapReplicaFeaturesNeedGormRepository(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	for _, set := range []func(*replicaSwitches){
		func(r *replicaSwitches) { r.election = true },
		func(r *replicaSwitches) { r.heartbeat = true },
	} {
		var sw replicaSwitches
		set(&sw)
		cfg := bootstrapTestConfig()
		cfg.Replica.LeaderElectionEnabled = sw.election
		cfg.Replica.HeartbeatEnabled = sw.heartbeat
		s, router, err := buildServices(cfg, &stubRepository{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database")
		assert.Nil(t, s)
		assert.Nil(t, router)
	}
}

type replicaSwitches struct{ election, heartbeat bool }

func TestReplicaProcessID(t *testing.T) {
	t.Parallel()
	a, b := newProcessID(), newProcessID()
	assert.NotEqual(t, a, b)
	assert.NotEmpty(t, a)
}

// A worker that ignores its context outlives the leader step: Close reports
// it and does NOT release the lease. The election keeps renewing, so no
// other replica can start while the worker runs; the lease expires only
// after the renewals stop (the database closes, the process exits).
func TestElectedLeaderKeepsLeaseWhileWorkersHang(t *testing.T) {
	t.Parallel()
	_, db := openSQLiteRepo(t)
	require.NoError(t, db.AutoMigrate(&leader.LeaseRecord{}))
	cfg := leader.LeaseConfig{Duration: 600 * time.Millisecond, RenewDeadline: 300 * time.Millisecond, RetryPeriod: 50 * time.Millisecond}

	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	g := leader.NewGroup(time.Second, leader.Worker{Name: "hung", Run: func(context.Context) {
		once.Do(func() { close(started) })
		<-release // ignores its ctx
	}})
	e := startElection(g, leader.NewLease(db, "workers", "me", cfg))
	t.Cleanup(func() {
		close(release)
		e.cancel()
		<-e.done
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("not leader")
	}

	assert.False(t, e.Close(100*time.Millisecond), "the hung worker is reported")
	assert.False(t, g.Running())

	// Another replica tries to take the lease for the whole wait below: about
	// 2*Duration/RetryPeriod = 24 attempts, twice as long as the lease lasts
	// without renewal. It must never start.
	other := leader.NewLease(db, "workers", "other", cfg)
	ctx, cancel := context.WithCancel(context.Background())
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		other.Run(ctx, func(context.Context) { t.Error("another replica started while the worker runs") }, func() {})
	}()

	// Held: this replica is still the holder and the expiry is in the
	// future, also after more than one lease duration (the election renews).
	time.Sleep(2 * cfg.Duration)
	var held int64
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM leader_leases WHERE name = 'workers' AND holder = 'me' AND expires_at > strftime('%Y-%m-%d %H:%M:%f', 'now')").Scan(&held).Error)
	assert.Equal(t, int64(1), held, "the lease is not released while the worker runs")
	select {
	case <-e.done:
		t.Fatal("the election ended")
	default:
	}
	cancel()
	<-otherDone
}

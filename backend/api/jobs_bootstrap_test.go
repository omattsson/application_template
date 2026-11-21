package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/jobs"
	"backend/internal/leader"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeJobPool records the stop of the job pool and the context it got.
type fakeJobPool struct {
	rec *callRecorder
	err error
	got ctxState
	mu  sync.Mutex
}

func (f *fakeJobPool) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = stateOf(ctx)
	f.rec.add("job pool")
	return f.err
}

// The pool stops after the HTTP server and the in-flight work (no handler
// enqueues more) and before the outbox poller (events that a running job
// writes are still delivered), the hub (a job may send to it) and the
// database (a job writes to it), within its own fixed budget.
func TestShutdownStopsJobPoolBeforeOutboxHubAndDatabase(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	p := &fakeJobPool{rec: rec}
	s.jobs = p
	s.outboxPoller = &fakePoller{rec: rec}

	require.NoError(t, shutdown(context.Background(), s))
	assert.Equal(t, []string{
		"server", "leader group", "in-flight", "job pool", "outbox poller", "hub", "telemetry", "rate limiters", "database",
	}, rec.list())
	assert.NoError(t, p.got.err)
	assert.Greater(t, p.got.left, jobsStopTimeout-time.Second)
	assert.LessOrEqual(t, p.got.left, jobsStopTimeout)
}

func TestShutdownJobPoolBudgetIsDetached(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	p := &fakeJobPool{rec: rec}
	s.jobs = p

	// A parent that is already over must not cut the drain of the jobs.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, shutdown(ctx, s))
	assert.NoError(t, p.got.err, "the step context is detached from the shutdown context")
	assert.Greater(t, p.got.left, jobsStopTimeout-time.Second)
}

func TestShutdownRunsEveryStepAfterAJobPoolFailure(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	failure := errors.New("2 handlers did not return")
	s.jobs = &fakeJobPool{rec: rec, err: failure}
	s.outboxPoller = &fakePoller{rec: rec}

	err := shutdown(context.Background(), s)
	require.Error(t, err)
	assert.ErrorIs(t, err, failure)
	assert.Equal(t, []string{
		"server", "leader group", "in-flight", "job pool", "outbox poller", "hub", "telemetry", "rate limiters", "database",
	}, rec.list())
}

func TestShutdownSkipsATypedNilJobModule(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	s.jobs = (*jobsModule)(nil)

	require.NoError(t, shutdown(context.Background(), s))
	assert.NotContains(t, rec.list(), "job pool")
}

// jobsConfig returns the bootstrap config with the job module switched.
func jobsConfig(enabled bool) *config.Config {
	cfg := authModeConfig(false)
	cfg.Jobs = config.JobsConfig{
		Enabled: enabled, Workers: 2, MaxAttempts: 5,
		PollInterval: 50 * time.Millisecond, VisibilityTimeout: time.Minute,
		BackoffBase: time.Second, BackoffMax: time.Minute,
		Retention: time.Hour, DeadRetention: 2 * time.Hour, StaleAfter: time.Minute,
	}
	return cfg
}

func jobsSQLiteRepo(t *testing.T) (models.Repository, *gorm.DB) {
	t.Helper()
	repo, db := openSQLiteRepo(t)
	require.NoError(t, db.AutoMigrate(&models.Job{}))
	return repo, db
}

func readyChecks(t *testing.T, base string) map[string]health.CheckStatus {
	t.Helper()
	_, body := call(t, http.MethodGet, base+"/health/ready", "", nil)
	var st health.HealthStatus
	require.NoError(t, json.Unmarshal([]byte(body), &st))
	return st.Checks
}

// With the module off there is no table access, no pool, no janitor, no route,
// no health check and no queue.
func TestBootstrapJobsOff(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := openSQLiteRepo(t) // no jobs table at all
	require.False(t, db.Migrator().HasTable("jobs"))

	s, base := startServices(t, jobsConfig(false), repo)

	assert.True(t, isNilInterface(s.jobs))
	assert.Nil(t, s.jobQueue)
	assert.Nil(t, s.jobRegistry)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.NotContains(t, g.Names(), jobs.CleanupWorkerName)

	status, _ := call(t, http.MethodGet, base+"/api/v1/jobs", "", nil)
	assert.Equal(t, http.StatusNotFound, status)
	assert.NotContains(t, readyChecks(t, base), "jobs")

	// Everything else is unchanged.
	status, _ = call(t, http.MethodGet, base+"/api/v1/items", "", nil)
	assert.Equal(t, http.StatusOK, status)

	// A nil queue refuses to enqueue instead of panicking.
	_, err := s.jobQueue.Enqueue(context.Background(), "anything", nil)
	assert.ErrorIs(t, err, jobs.ErrDisabled)
}

// With the module on there is a running pool, the janitor, the list route and
// an informational health check.
func TestBootstrapJobsOn(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := jobsSQLiteRepo(t)
	require.NoError(t, db.Create(&models.Job{
		Type: "email.send", Payload: `{"secret":1}`, Status: models.JobStatusDead, MaxAttempts: 5,
	}).Error)
	// SQLite reads back only its own time text.
	require.NoError(t, db.Exec("UPDATE jobs SET run_at = strftime('%Y-%m-%d %H:%M:%f', 'now'), "+
		"created_at = strftime('%Y-%m-%d %H:%M:%f', 'now'), updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')").Error)

	s, base := startServices(t, jobsConfig(true), repo)

	require.False(t, isNilInterface(s.jobs))
	require.NotNil(t, s.jobQueue)
	require.NotNil(t, s.jobRegistry)
	assert.Empty(t, s.jobRegistry.Types(), "the template registers no handler")
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.Contains(t, g.Names(), jobs.CleanupWorkerName)

	status, body := call(t, http.MethodGet, base+"/api/v1/jobs", "", nil)
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, `"type":"email.send"`)
	assert.NotContains(t, body, "secret")

	checks := readyChecks(t, base)
	require.Contains(t, checks, "jobs")
	assert.Equal(t, "UP", checks["jobs"].Status, "no sample yet is not degraded")
}

// The module needs the database repository; without it the start fails
// instead of dropping the queue silently.
func TestBootstrapJobsNeedsTheDatabase(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)

	s, router, err := buildServices(jobsConfig(true), &stubRepository{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JOBS_ENABLED")
	assert.Nil(t, s)
	assert.Nil(t, router)
}

// The admin list is gated with auth on.
func TestBootstrapJobsOnWithAuth(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, _ := jobsSQLiteRepo(t)

	cfg := authModeConfig(true)
	cfg.Jobs = jobsConfig(true).Jobs
	_, base := startServices(t, cfg, repo)

	status, _ := call(t, http.MethodGet, base+"/api/v1/jobs", "", nil)
	assert.Equal(t, http.StatusUnauthorized, status)
}

type fakeJobStatus struct {
	snap    jobs.StatsSnapshot
	hasSnap bool
	running bool
}

func (f fakeJobStatus) Running() bool                     { return f.running }
func (f fakeJobStatus) Stats() (jobs.StatsSnapshot, bool) { return f.snap, f.hasSnap }

func TestJobsHealthCheck(t *testing.T) {
	t.Parallel()

	const stale = 5 * time.Minute
	snap := func(age time.Duration) jobs.StatsSnapshot {
		return jobs.StatsSnapshot{JobStats: models.JobStats{Pending: 7, Running: 2, Dead: 3, Due: 4, OldestDueAge: age}}
	}

	tests := []struct {
		name    string
		status  fakeJobStatus
		wantErr string
	}{
		{"running, no sample yet", fakeJobStatus{running: true}, ""},
		{"running and fresh", fakeJobStatus{running: true, hasSnap: true, snap: snap(time.Second)}, ""},
		{"exactly at the threshold", fakeJobStatus{running: true, hasSnap: true, snap: snap(stale)}, ""},
		{"stale", fakeJobStatus{running: true, hasSnap: true, snap: snap(stale + time.Second)}, "pending 7"},
		{"not running", fakeJobStatus{hasSnap: true, snap: snap(0)}, "not running"},
		{"not running and stale", fakeJobStatus{hasSnap: true, snap: snap(time.Hour)}, "not running"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := jobsHealthCheck(tt.status, stale)(context.Background())
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			// Counts only: no payload, no job error text.
			assert.NotContains(t, strings.ToLower(err.Error()), "payload")
		})
	}
}

func TestRegisterJobsRegistersNothingInTheTemplate(t *testing.T) {
	t.Parallel()
	reg := jobs.NewRegistry()
	registerJobs(reg)
	assert.Empty(t, reg.Types())
}

// A nil module (the module is off) starts and stops without doing anything or
// failing: the shutdown calls it unconditionally.
func TestJobsModuleNilStartAndStop(t *testing.T) {
	var m *jobsModule
	assert.NotPanics(t, m.Start)
	assert.NoError(t, m.Stop(context.Background()))
}

// Start runs the pool and Stop ends it; a second Stop is harmless.
func TestJobsModuleStartAndStop(t *testing.T) {
	repo, _ := jobsSQLiteRepo(t)
	m, err := buildJobs(jobsConfig(true), repo, "replica-test", health.New())
	require.NoError(t, err)
	require.NotNil(t, m)
	require.False(t, m.pool.Running(), "built, not started")

	m.Start()
	assert.True(t, m.pool.Running())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, m.Stop(ctx))
	assert.False(t, m.pool.Running())
	assert.NoError(t, m.Stop(ctx))
}

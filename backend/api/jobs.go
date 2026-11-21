package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/jobs"
	"backend/internal/leader"
	"backend/internal/models"
)

// jobsStopTimeout is the budget of the shutdown step that stops the job pool:
// the fixed 5 s that running jobs get, plus the fixed 2 s to give the
// unfinished ones back.
const jobsStopTimeout = jobs.DefaultDrainTimeout + jobs.DefaultReleaseTimeout

// registerJobs is the one place where the application registers the handlers
// of its background jobs (optional module, JOBS_ENABLED=true). The template
// registers none: the registry is empty, so the pool claims nothing until an
// application adds a handler here. Register runs once, at start, before the
// pool starts; a duplicate or invalid type name panics (a programming error).
//
// A handler runs at least once and may run twice (a crash, a lease that
// passed): it must be idempotent. Return jobs.Permanent(err) for a failure
// that a retry cannot fix. EXTENDING.md, "Adding a background job", has the
// whole walkthrough. Example:
//
//	reg.Register("report.generate", func(ctx context.Context, job jobs.Job) error {
//		var p struct{ ReportID string }
//		if err := json.Unmarshal(job.Payload, &p); err != nil {
//			return jobs.Permanent(err) // a bad payload never gets better
//		}
//		return reports.Generate(ctx, p.ReportID) // honour ctx: shutdown cancels it
//	})
func registerJobs(reg *jobs.Registry) {
	_ = reg // no handler yet
}

// jobsStopper is the job module (*jobsModule). Stop ends the pool (the running
// jobs get their drain time, the unfinished ones are released) and returns an
// error only when a handler ignored its cancel.
type jobsStopper interface {
	Stop(ctx context.Context) error
}

// jobsStatus is the part of the pool that the health check reads (*jobs.Pool).
type jobsStatus interface {
	Running() bool
	Stats() (jobs.StatsSnapshot, bool)
}

// jobsModule holds what the job module builds. It exists only with
// JOBS_ENABLED=true.
type jobsModule struct {
	repo     models.JobRepository
	registry *jobs.Registry
	queue    *jobs.Queue
	pool     *jobs.Pool
	metrics  *jobs.Metrics
	cleanup  leader.Worker
}

// buildJobs builds the repository, the registry (with the handlers of
// registerJobs), the queue, the pool and the cleanup worker on the database of
// repo, and registers the informational health check. It starts nothing: the
// caller passes cleanup to the leader group and calls Start once the leader
// workers run. It returns nil, nil with the module off, so nothing of the
// module exists then.
func buildJobs(cfg *config.Config, repo models.Repository, processID string, checker *health.HealthChecker) (*jobsModule, error) {
	if !cfg.Jobs.Enabled {
		return nil, nil
	}

	g, ok := repo.(gormRepository)
	if !ok || g.DB() == nil {
		return nil, errors.New("JOBS_ENABLED=true needs the database repository")
	}

	jc := cfg.Jobs
	jobRepo := database.NewGORMJobRepository(g.DB())
	registry := jobs.NewRegistry()
	registerJobs(registry)

	m := &jobsModule{
		repo:     jobRepo,
		registry: registry,
		queue:    jobs.NewQueue(jobRepo, jc.MaxAttempts),
		metrics:  jobs.NewMetrics(), // nil with telemetry off
		cleanup: jobs.NewCleanupWorker(jobs.CleanupConfig{
			Repo: jobRepo, Retention: jc.Retention, DeadRetention: jc.DeadRetention,
		}),
	}
	// NewPool returns nil only without a repository or a registry; both exist.
	m.pool = jobs.NewPool(jobs.PoolConfig{
		Repo:              jobRepo,
		Registry:          registry,
		Metrics:           m.metrics,
		ProcessID:         processID,
		Workers:           jc.Workers,
		PollInterval:      jc.PollInterval,
		VisibilityTimeout: jc.VisibilityTimeout,
		BackoffBase:       jc.BackoffBase,
		BackoffMax:        jc.BackoffMax,
	})
	checker.AddInformationalCheck("jobs", jobsHealthCheck(m.pool, jc.StaleAfter))

	return m, nil
}

// Start starts the pool (non-blocking). It does nothing on a nil module.
func (m *jobsModule) Start() {
	if m == nil {
		return
	}

	m.pool.Start()
}

// Stop is the shutdown step of the module: it stops the pool, then releases
// the metrics. It is safe on a nil module.
func (m *jobsModule) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}

	defer m.close()

	return m.pool.Stop(ctx)
}

// close unregisters the metrics. It is idempotent.
func (m *jobsModule) close() {
	if m == nil {
		return
	}

	m.metrics.Close()
}

// jobsHealthCheck returns the check of the job module. It reads the snapshot
// that the pool samples every 30 s (no query per request) and reports
// DEGRADED when the pool is not running, or when the oldest due job has waited
// longer than staleAfter. Before the first sample it does not judge the age.
// The message holds counts only: the readiness endpoint is public, and a job
// error or payload never reaches it.
func jobsHealthCheck(p jobsStatus, staleAfter time.Duration) func(context.Context) error {
	return func(context.Context) error {
		if !p.Running() {
			return errors.New("job pool is not running")
		}

		snap, ok := p.Stats()
		if !ok || snap.OldestDueAge <= staleAfter {
			return nil
		}

		return fmt.Errorf("oldest due job has waited %s (limit %s): pending %d, due %d, running %d, dead %d",
			snap.OldestDueAge.Round(time.Second), staleAfter, snap.Pending, snap.Due, snap.Running, snap.Dead)
	}
}

// jobsRepo returns the job repository of m, or a nil interface with the
// module off (a typed nil in routes.Deps.Jobs would pass its nil check).
func jobsRepo(m *jobsModule) models.JobRepository {
	if m == nil {
		return nil
	}

	return m.repo
}

// jobsQueue returns the queue of m, or nil with the module off.
func jobsQueue(m *jobsModule) *jobs.Queue {
	if m == nil {
		return nil
	}

	return m.queue
}

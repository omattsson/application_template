package config

import (
	"errors"
	"time"
)

// Defaults and limits of the job module.
const (
	defaultJobsWorkers           = 4
	minJobsWorkers               = 1
	maxJobsWorkers               = 64
	defaultJobsPollInterval      = time.Second
	minJobsPollInterval          = 100 * time.Millisecond
	defaultJobsVisibilityTimeout = 5 * time.Minute
	minJobsVisibilityTimeout     = 10 * time.Second
	defaultJobsMaxAttempts       = 5
	minJobsMaxAttempts           = 1
	maxJobsMaxAttempts           = 50
	defaultJobsBackoffBase       = 10 * time.Second
	minJobsBackoffBase           = time.Second
	defaultJobsBackoffMax        = time.Hour
	defaultJobsRetention         = 24 * time.Hour
	minJobsRetention             = time.Minute
	defaultJobsDeadRetention     = 168 * time.Hour
	defaultJobsStaleAfter        = 5 * time.Minute
	minJobsStaleAfter            = 10 * time.Second
)

// JobsConfig holds the settings of the optional background job module (a
// MySQL table of jobs and a pool of workers on every replica).
type JobsConfig struct {
	// PollInterval is JOBS_POLL_INTERVAL (default 1s, at least 100ms): how
	// often an idle worker pool looks for due jobs.
	PollInterval time.Duration
	// VisibilityTimeout is JOBS_VISIBILITY_TIMEOUT (default 5m, at least
	// 10s): the lease of a running job. The pool extends it every third of
	// this time; a job whose lease passed is claimed again.
	VisibilityTimeout time.Duration
	// BackoffBase is JOBS_BACKOFF_BASE (default 10s, at least 1s): the delay
	// before the second attempt. It doubles with every attempt.
	BackoffBase time.Duration
	// BackoffMax is JOBS_BACKOFF_MAX (default 1h, at least BackoffBase): the
	// longest delay between two attempts.
	BackoffMax time.Duration
	// Retention is JOBS_RETENTION (default 24h, at least 1m): how long a
	// succeeded job stays.
	Retention time.Duration
	// DeadRetention is JOBS_DEAD_RETENTION (default 168h, at least
	// Retention): how long a dead job stays.
	DeadRetention time.Duration
	// StaleAfter is JOBS_STALE_AFTER (default 5m, at least 10s): the health
	// check reports DEGRADED when the oldest due job waited longer.
	StaleAfter time.Duration
	// Workers is JOBS_WORKERS (default 4, 1 to 64): the goroutines that run
	// jobs, per replica.
	Workers int
	// MaxAttempts is JOBS_MAX_ATTEMPTS (default 5, 1 to 50): the claims after
	// which a failing job is dead. A job can set its own.
	MaxAttempts int
	// Enabled is JOBS_ENABLED (default false, strict boolean). With false no
	// table is read, no pool or janitor runs and the route does not exist.
	Enabled bool
}

// loadJobsConfig reads the job variables. The switch is parsed strictly (a
// typo must not silently drop the queue that the operator asked for). The
// other values are read leniently while the module is off, because nothing
// uses them, and strictly while it is on.
func loadJobsConfig() (JobsConfig, error) {
	enabled, err := parseEnvBoolStrict("JOBS_ENABLED", false)
	if err != nil {
		return JobsConfig{}, err
	}

	c := JobsConfig{Enabled: enabled}

	durations := []struct {
		dst      *time.Duration
		key      string
		fallback time.Duration
	}{
		{&c.PollInterval, "JOBS_POLL_INTERVAL", defaultJobsPollInterval},
		{&c.VisibilityTimeout, "JOBS_VISIBILITY_TIMEOUT", defaultJobsVisibilityTimeout},
		{&c.BackoffBase, "JOBS_BACKOFF_BASE", defaultJobsBackoffBase},
		{&c.BackoffMax, "JOBS_BACKOFF_MAX", defaultJobsBackoffMax},
		{&c.Retention, "JOBS_RETENTION", defaultJobsRetention},
		{&c.DeadRetention, "JOBS_DEAD_RETENTION", defaultJobsDeadRetention},
		{&c.StaleAfter, "JOBS_STALE_AFTER", defaultJobsStaleAfter},
	}
	ints := []struct {
		dst      *int
		key      string
		fallback int32
	}{
		{&c.Workers, "JOBS_WORKERS", defaultJobsWorkers},
		{&c.MaxAttempts, "JOBS_MAX_ATTEMPTS", defaultJobsMaxAttempts},
	}

	for _, d := range durations {
		if !enabled {
			*d.dst = getEnvDuration(d.key, d.fallback)
			continue
		}

		v, err := parseEnvDurationStrict(d.key, d.fallback)
		if err != nil {
			return JobsConfig{}, err
		}

		*d.dst = v
	}

	for _, i := range ints {
		if !enabled {
			*i.dst = int(getEnvInt32(i.key, i.fallback))
			continue
		}

		v, err := parseEnvInt32Strict(i.key, i.fallback)
		if err != nil {
			return JobsConfig{}, err
		}

		*i.dst = int(v)
	}

	return c, nil
}

// Validate checks the settings that the module uses. It is called only with
// the module on.
func (c *JobsConfig) Validate() error {
	if c.Workers < minJobsWorkers || c.Workers > maxJobsWorkers {
		return errors.New("JOBS_WORKERS must be from 1 to 64")
	}

	if c.PollInterval < minJobsPollInterval {
		return errors.New("JOBS_POLL_INTERVAL must be at least 100ms")
	}

	if c.VisibilityTimeout < minJobsVisibilityTimeout {
		return errors.New("JOBS_VISIBILITY_TIMEOUT must be at least 10s")
	}

	if c.MaxAttempts < minJobsMaxAttempts || c.MaxAttempts > maxJobsMaxAttempts {
		return errors.New("JOBS_MAX_ATTEMPTS must be from 1 to 50")
	}

	if c.BackoffBase < minJobsBackoffBase {
		return errors.New("JOBS_BACKOFF_BASE must be at least 1s")
	}

	if c.BackoffMax < c.BackoffBase {
		return errors.New("JOBS_BACKOFF_MAX must be at least JOBS_BACKOFF_BASE")
	}

	if c.Retention < minJobsRetention {
		return errors.New("JOBS_RETENTION must be at least 1m")
	}

	if c.DeadRetention < c.Retention {
		return errors.New("JOBS_DEAD_RETENTION must be at least JOBS_RETENTION")
	}

	if c.StaleAfter < minJobsStaleAfter {
		return errors.New("JOBS_STALE_AFTER must be at least 10s")
	}

	return nil
}

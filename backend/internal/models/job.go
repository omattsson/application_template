package models

import (
	"context"
	"time"
)

// Status values of a Job.
const (
	// JobStatusPending is a job that waits for its run_at and a worker.
	JobStatusPending = "pending"
	// JobStatusRunning is a job that a worker claimed. Its lease
	// (locked_until) ends when the worker stops extending it.
	JobStatusRunning = "running"
	// JobStatusSucceeded is a job whose handler returned nil.
	JobStatusSucceeded = "succeeded"
	// JobStatusDead is a job that will not run again: it used all attempts, or
	// its handler reported a permanent failure.
	JobStatusDead = "dead"
)

// Limits of a job.
const (
	// JobTypeMaxLen is the longest job type name (the size of jobs.type).
	JobTypeMaxLen = 100
	// JobPayloadMaxBytes is the largest payload that the queue accepts.
	JobPayloadMaxBytes = 64 * 1024
	// JobLastErrorMaxChars is the number of characters of last_error that are
	// stored; the rest of an error text is cut.
	JobLastErrorMaxChars = 2000
	// JobWorkerIDMaxLen is the size of jobs.locked_by: a replica ID (at most
	// 253 characters) plus the worker number.
	JobWorkerIDMaxLen = 260
	// JobMaxAttemptsLimit is the highest max_attempts of a job.
	JobMaxAttemptsLimit = 50
)

// JobStatuses lists the valid status values, in life cycle order.
var JobStatuses = []string{JobStatusPending, JobStatusRunning, JobStatusSucceeded, JobStatusDead}

// ValidJobStatus reports whether s is a job status.
func ValidJobStatus(s string) bool {
	for _, v := range JobStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// Job is one row of the jobs table, the queue of the optional job module
// (JOBS_ENABLED=true). Its definition is copied into migration 20261121000001
// (jobV1); a change here needs a new migration, and TestJobsMatchModel fails
// without one.
//
// Every time column is written and compared with the database server clock
// (UTC_TIMESTAMP(6) on MySQL), never with the clock of a replica, so replicas
// with skewed clocks agree on what is due and what is expired.
type Job struct {
	// RunAt is when the job is due.
	RunAt     time.Time `json:"run_at" gorm:"type:datetime(6);not null;index:idx_jobs_claim,priority:2;index:idx_jobs_run_at"`
	CreatedAt time.Time `json:"created_at" gorm:"type:datetime(6);not null;index:idx_jobs_created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"type:datetime(6);not null"`
	// LockedUntil is the end of the lease of a running job.
	LockedUntil *time.Time `json:"locked_until,omitempty" gorm:"type:datetime(6)"`
	// FinishedAt is set when the job became succeeded or dead.
	FinishedAt *time.Time `json:"finished_at,omitempty" gorm:"type:datetime(6);index:idx_jobs_finished,priority:2"`
	// Payload is the JSON document that the handler receives. The list
	// endpoint never selects or returns it: it may hold sensitive data.
	Payload string `json:"-" gorm:"type:longtext;not null"`
	// Type is the registered handler name.
	Type string `json:"type" gorm:"size:100;not null;index:idx_jobs_type_status,priority:1"`
	// Status is one of JobStatuses.
	Status string `json:"status" gorm:"size:16;not null;index:idx_jobs_claim,priority:1;index:idx_jobs_finished,priority:1;index:idx_jobs_type_status,priority:2"`
	// LockedBy is the worker (replica ID and worker number) that holds the
	// lease. Empty when the job is not running.
	LockedBy string `json:"locked_by,omitempty" gorm:"size:260;not null;default:''"`
	// LastError is the error text of the last failed attempt, cut to
	// JobLastErrorMaxChars characters.
	LastError string `json:"last_error,omitempty" gorm:"size:2000;not null;default:''"`
	// ID is the auto increment key.
	ID uint64 `json:"id" gorm:"primaryKey;autoIncrement;index:idx_jobs_claim,priority:3"`
	// Attempts is the number of claims: a crash of a worker counts.
	Attempts int32 `json:"attempts" gorm:"type:int;not null;default:0"`
	// MaxAttempts is the number of claims after which a failing job is dead.
	MaxAttempts int32 `json:"max_attempts" gorm:"type:int;not null;default:5"`
}

// TableName returns the table name of Job.
func (Job) TableName() string { return "jobs" }

// JobSpec describes a job to enqueue.
type JobSpec struct {
	// Type is the handler name (1 to JobTypeMaxLen characters).
	Type string
	// Payload is a JSON document of at most JobPayloadMaxBytes bytes.
	Payload []byte
	// Delay is how long after the database time the job becomes due. Zero
	// means at once.
	Delay time.Duration
	// MaxAttempts is the number of claims after which a failing job is dead
	// (1 to JobMaxAttemptsLimit).
	MaxAttempts int
}

// JobClaim is the result of JobRepository.Claim.
type JobClaim struct {
	// Jobs are the claimed jobs, now running. Job.Attempts is the number of
	// this claim and Job.LockedBy the worker ID that holds the lease.
	Jobs []Job
	// Expired are the running jobs whose lease had passed and whose attempts
	// were used up: the claim made them dead (not run again).
	Expired []Job
}

// JobFilter selects jobs for JobRepository.ListPaged. A zero field does not
// filter.
type JobFilter struct {
	// Status matches the status exactly (one of JobStatuses).
	Status string
	// Type matches the type exactly.
	Type string
	// SortBy is the sort key, one of JobSortKeys. Empty sorts by id, ascending. An
	// unknown key makes ListPaged fail with dberrors.ErrValidation.
	SortBy string
	// SortDesc reverses the order. The id tiebreaker follows it.
	SortDesc bool
}

// JobSortKeys are the keys that JobFilter.SortBy accepts (the values of the
// sort query parameter of GET /api/v1/jobs). Each one has an index.
var JobSortKeys = []string{"created_at", "run_at", "id"}

// JobStats is a snapshot of the queue, by the database clock.
type JobStats struct {
	// OldestDueAge is how long the oldest due pending job has been due. Zero
	// when Due is zero.
	OldestDueAge time.Duration
	// Pending, Running, Succeeded and Dead are the row counts per status.
	Pending   int64
	Running   int64
	Succeeded int64
	Dead      int64
	// Due is the number of pending jobs whose run_at has passed.
	Due int64
}

// JobRepository is the data access of the job queue. Every time is the
// database clock. A "fenced" update changes a row only when it is still
// running under the given worker ID, and returns false when it is not (the job
// was released, taken over by another worker, or deleted): the caller then
// drops its result.
//
// Errors: a repository returns *dberrors.DatabaseError; invalid arguments are
// dberrors.ErrValidation.
type JobRepository interface {
	// Enqueue inserts a pending job and returns its ID.
	Enqueue(ctx context.Context, spec JobSpec) (uint64, error)
	// Claim takes up to len(workerIDs) jobs of the given types for this
	// process in one transaction (SELECT ... FOR UPDATE SKIP LOCKED): first
	// the running jobs whose lease passed, then the pending jobs that are due,
	// each in run_at, id order. Job i gets workerIDs[i] as locked_by, its
	// attempts grow by one and its lease ends after lease. An expired job with
	// no attempt left becomes dead instead (JobClaim.Expired). Without types
	// or worker IDs it does nothing.
	Claim(ctx context.Context, workerIDs, types []string, lease time.Duration) (JobClaim, error)
	// Extend moves the end of the lease to now plus lease (fenced).
	Extend(ctx context.Context, id uint64, workerID string, lease time.Duration) (bool, error)
	// Complete marks the job succeeded (fenced).
	Complete(ctx context.Context, id uint64, workerID string) (bool, error)
	// Fail records lastError and either schedules the next attempt after
	// retryIn (dead false) or makes the job dead (fenced).
	Fail(ctx context.Context, id uint64, workerID, lastError string, retryIn time.Duration, dead bool) (bool, error)
	// Release puts the job back to pending, due at once, and does not count the
	// attempt (fenced). It is for a job that shutdown interrupted.
	Release(ctx context.Context, id uint64, workerID string) (bool, error)
	// ListPaged returns one page of jobs without the payload and the number of
	// matches.
	ListPaged(ctx context.Context, filter JobFilter, limit, offset int) ([]Job, int64, error)
	// Stats returns the counts per status and the age of the oldest due job.
	Stats(ctx context.Context) (JobStats, error)
	// DeleteFinishedBefore deletes at most limit jobs of the given status
	// (succeeded or dead) that finished more than maxAge before the database
	// time, oldest first, and returns the number deleted.
	DeleteFinishedBefore(ctx context.Context, status string, maxAge time.Duration, limit int) (int64, error)
}

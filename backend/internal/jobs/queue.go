package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"backend/internal/models"
)

// Queue enqueues jobs. It does not need a registered handler: a rolling
// deploy can enqueue a type that only the new replicas handle, and the job
// waits until one of them claims it.
type Queue struct {
	repo        models.JobRepository
	now         func() time.Time
	maxAttempts int
}

// NewQueue returns a queue on repo. defaultMaxAttempts (JOBS_MAX_ATTEMPTS) is
// the attempts of a job that sets none with WithMaxAttempts; a value outside 1
// to 50 becomes 5.
func NewQueue(repo models.JobRepository, defaultMaxAttempts int) *Queue {
	if defaultMaxAttempts < 1 || defaultMaxAttempts > models.JobMaxAttemptsLimit {
		defaultMaxAttempts = 5
	}

	return &Queue{repo: repo, maxAttempts: defaultMaxAttempts, now: time.Now}
}

type enqueueOptions struct {
	runAt       time.Time
	delay       time.Duration
	maxAttempts int
	hasRunAt    bool
}

// Option changes how Enqueue schedules a job.
type Option func(*enqueueOptions)

// WithDelay makes the job due after d (database clock). A negative d is an
// error of Enqueue.
func WithDelay(d time.Duration) Option {
	return func(o *enqueueOptions) { o.delay, o.hasRunAt = d, false }
}

// WithRunAt makes the job due at t. The delay is t minus the clock of this
// process, so a skew between this process and the database moves the job by
// that skew; use WithDelay when that matters. A time in the past means now.
func WithRunAt(t time.Time) Option {
	return func(o *enqueueOptions) { o.runAt, o.hasRunAt = t, true }
}

// WithMaxAttempts sets the attempts after which a failing job is dead (1 to
// 50) instead of JOBS_MAX_ATTEMPTS.
func WithMaxAttempts(n int) Option {
	return func(o *enqueueOptions) { o.maxAttempts = n }
}

// Enqueue stores a job of the registered type name typ with payload, which is
// marshalled to JSON (at most 64 KiB), and returns the job ID. The job runs at
// least once on some replica; there is no deduplication. A nil Queue (module
// off) returns ErrDisabled.
func (q *Queue) Enqueue(ctx context.Context, typ string, payload any, opts ...Option) (uint64, error) {
	if q == nil {
		return 0, ErrDisabled
	}

	if !ValidType(typ) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidType, typ)
	}

	o := enqueueOptions{maxAttempts: q.maxAttempts}
	for _, opt := range opts {
		opt(&o)
	}

	if o.maxAttempts < 1 || o.maxAttempts > models.JobMaxAttemptsLimit {
		return 0, invalidOption("max attempts must be from 1 to %d", models.JobMaxAttemptsLimit)
	}

	if o.delay < 0 {
		return 0, invalidOption("delay must not be negative")
	}

	delay := o.delay
	if o.hasRunAt {
		delay = max(0, o.runAt.Sub(q.now()))
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidPayload, err)
	}

	if len(body) > models.JobPayloadMaxBytes {
		return 0, ErrPayloadTooLarge
	}

	return q.repo.Enqueue(ctx, models.JobSpec{Type: typ, Payload: body, Delay: delay, MaxAttempts: o.maxAttempts})
}

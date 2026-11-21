package jobs

import (
	"context"
	"slices"
	"sync"
	"time"

	"backend/internal/models"
)

// fakeRepo is an in-memory models.JobRepository with the semantics of the
// SQL one (due, lease, fence) on the real clock, and hooks to inject faults.
type fakeRepo struct {
	claimErr   error
	releaseErr error
	statsErr   error
	extendFn   func(id uint64) (bool, error)
	completeFn func(id uint64) error
	expiredFn  func() []models.Job
	jobs       map[uint64]*models.Job
	failIn     map[uint64]time.Duration
	stats      models.JobStats

	claimCalls   [][]string // the worker IDs of every Claim call
	claimTypes   [][]string
	deleted      []deleteCall
	releaseCalls []uint64
	deleteFn     func(status string, limit int) (int64, error)

	extendCalls int
	statsCalls  int
	next        uint64
	mu          sync.Mutex
}

type deleteCall struct {
	status string
	age    time.Duration
	limit  int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{jobs: map[uint64]*models.Job{}, failIn: map[uint64]time.Duration{}}
}

func (f *fakeRepo) add(typ string, maxAttempts int32, runIn time.Duration) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	now := time.Now()
	f.jobs[f.next] = &models.Job{
		ID: f.next, Type: typ, Payload: `{"n":1}`, Status: models.JobStatusPending,
		MaxAttempts: maxAttempts, RunAt: now.Add(runIn), CreatedAt: now,
	}
	return f.next
}

func (f *fakeRepo) get(id uint64) models.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.jobs[id]
}

func (f *fakeRepo) status(id uint64) string { return f.get(id).Status }

func (f *fakeRepo) claims() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.claimCalls)
}

func (f *fakeRepo) extends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.extendCalls
}

func (f *fakeRepo) Enqueue(_ context.Context, spec models.JobSpec) (uint64, error) {
	return f.add(spec.Type, int32(spec.MaxAttempts), spec.Delay), nil
}

func (f *fakeRepo) Claim(_ context.Context, workerIDs, types []string, lease time.Duration) (models.JobClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls = append(f.claimCalls, slices.Clone(workerIDs))
	f.claimTypes = append(f.claimTypes, slices.Clone(types))
	if f.claimErr != nil {
		return models.JobClaim{}, f.claimErr
	}
	var out models.JobClaim
	if f.expiredFn != nil {
		out.Expired = f.expiredFn()
	}
	now := time.Now()
	ids := make([]uint64, 0, len(f.jobs))
	for id := range f.jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if len(out.Jobs) >= len(workerIDs) {
			break
		}
		j := f.jobs[id]
		if !slices.Contains(types, j.Type) {
			continue
		}
		due := j.Status == models.JobStatusPending && !j.RunAt.After(now)
		expired := j.Status == models.JobStatusRunning && j.LockedUntil != nil && j.LockedUntil.Before(now)
		if !due && !expired {
			continue
		}
		until := now.Add(lease)
		j.Status, j.Attempts, j.LockedBy, j.LockedUntil = models.JobStatusRunning, j.Attempts+1, workerIDs[len(out.Jobs)], &until
		out.Jobs = append(out.Jobs, *j)
	}
	return out, nil
}

// fenced returns the job when it is running under workerID.
func (f *fakeRepo) fenced(id uint64, workerID string) *models.Job {
	j := f.jobs[id]
	if j == nil || j.Status != models.JobStatusRunning || j.LockedBy != workerID {
		return nil
	}
	return j
}

func (f *fakeRepo) Extend(_ context.Context, id uint64, workerID string, lease time.Duration) (bool, error) {
	f.mu.Lock()
	f.extendCalls++
	fn := f.extendFn
	f.mu.Unlock()
	if fn != nil {
		if ok, err := fn(id); err != nil || !ok {
			return ok, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.fenced(id, workerID)
	if j == nil {
		return false, nil
	}
	until := time.Now().Add(lease)
	j.LockedUntil = &until
	return true, nil
}

func (f *fakeRepo) Complete(_ context.Context, id uint64, workerID string) (bool, error) {
	f.mu.Lock()
	fn := f.completeFn
	f.mu.Unlock()
	if fn != nil {
		if err := fn(id); err != nil {
			return false, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.fenced(id, workerID)
	if j == nil {
		return false, nil
	}
	j.Status, j.LockedBy, j.LockedUntil = models.JobStatusSucceeded, "", nil
	return true, nil
}

func (f *fakeRepo) Fail(_ context.Context, id uint64, workerID, lastError string, retryIn time.Duration, dead bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.fenced(id, workerID)
	if j == nil {
		return false, nil
	}
	j.LastError = lastError
	j.LockedBy, j.LockedUntil = "", nil
	f.failIn[id] = retryIn
	if dead {
		j.Status = models.JobStatusDead
	} else {
		j.Status, j.RunAt = models.JobStatusPending, time.Now().Add(retryIn)
	}
	return true, nil
}

func (f *fakeRepo) Release(_ context.Context, id uint64, workerID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, id)
	if f.releaseErr != nil {
		return false, f.releaseErr
	}
	j := f.fenced(id, workerID)
	if j == nil {
		return false, nil
	}
	j.Status, j.LockedBy, j.LockedUntil, j.RunAt = models.JobStatusPending, "", nil, time.Now()
	j.Attempts--
	return true, nil
}

func (f *fakeRepo) ListPaged(context.Context, models.JobFilter, int, int) ([]models.Job, int64, error) {
	return nil, 0, nil
}

func (f *fakeRepo) Stats(context.Context) (models.JobStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls++
	if f.statsErr != nil {
		return models.JobStats{}, f.statsErr
	}
	return f.stats, nil
}

func (f *fakeRepo) DeleteFinishedBefore(_ context.Context, status string, maxAge time.Duration, limit int) (int64, error) {
	f.mu.Lock()
	f.deleted = append(f.deleted, deleteCall{status, maxAge, limit})
	fn := f.deleteFn
	f.mu.Unlock()
	if fn != nil {
		return fn(status, limit)
	}
	return 0, nil
}

var _ models.JobRepository = (*fakeRepo)(nil)

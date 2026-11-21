package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	testPoll       = 10 * time.Millisecond
	testVisibility = 150 * time.Millisecond // the heartbeat runs every 50 ms
	waitFor        = 3 * time.Second
	tick           = 5 * time.Millisecond
)

// startPool builds a pool over repo with one handler per type and starts it.
func startPool(t *testing.T, repo *fakeRepo, handlers map[string]Handler, mutate ...func(*PoolConfig)) *Pool {
	t.Helper()
	reg := NewRegistry()
	for typ, h := range handlers {
		reg.Register(typ, h)
	}
	cfg := PoolConfig{
		Repo: repo, Registry: reg, ProcessID: "replica-a", Workers: 2,
		PollInterval: testPoll, VisibilityTimeout: testVisibility,
		BackoffBase: time.Second, BackoffMax: time.Minute, StatsInterval: time.Hour,
		DrainTimeout: 300 * time.Millisecond, ReleaseTimeout: 200 * time.Millisecond,
		Rand: func() float64 { return 0.5 },
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p := NewPool(cfg)
	require.NotNil(t, p)
	p.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p
}

func waitStatus(t *testing.T, repo *fakeRepo, id uint64, want string) {
	t.Helper()
	require.Eventuallyf(t, func() bool { return repo.status(id) == want }, waitFor, tick,
		"job %d: want status %s, have %s", id, want, repo.status(id))
}

// waitAttempted waits until the job has been claimed (attempts at least one)
// and has the status: a pending job that was never claimed also has "pending".
func waitAttempted(t *testing.T, repo *fakeRepo, id uint64, want string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		j := repo.get(id)
		return j.Status == want && (j.Attempts >= 1 || want != models.JobStatusPending)
	}, waitFor, tick, "job %d: want status %s after an attempt, have %s", id, want, repo.status(id))
}

// claimTypesNotIn returns the claimed types outside want.
func (f *fakeRepo) claimTypesNotIn(want ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, ts := range f.claimTypes {
		for _, typ := range ts {
			if !slices.Contains(want, typ) {
				out = append(out, typ)
			}
		}
	}
	return out
}

func TestNewPoolNeedsARepositoryAndARegistry(t *testing.T) {
	t.Parallel()
	assert.Nil(t, NewPool(PoolConfig{Registry: NewRegistry()}))
	assert.Nil(t, NewPool(PoolConfig{Repo: newFakeRepo()}))
	assert.NotNil(t, NewPool(PoolConfig{Repo: newFakeRepo(), Registry: NewRegistry()}))
}

func TestNilPoolDoesNothing(t *testing.T) {
	t.Parallel()
	var p *Pool
	p.Start()
	assert.False(t, p.Running())
	_, ok := p.Stats()
	assert.False(t, ok)
	assert.NoError(t, p.Stop(context.Background()))
}

func TestPoolRunsAJob(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	got := make(chan Job, 1)
	p := startPool(t, repo, map[string]Handler{"mail": func(_ context.Context, j Job) error {
		got <- j
		return nil
	}})
	id := repo.add("mail", 3, 0)

	waitStatus(t, repo, id, models.JobStatusSucceeded)
	j := <-got
	assert.Equal(t, id, j.ID)
	assert.Equal(t, "mail", j.Type)
	assert.JSONEq(t, `{"n":1}`, string(j.Payload))
	assert.Equal(t, 1, j.Attempt)
	assert.Equal(t, 3, j.MaxAttempts)
	assert.True(t, p.Running())
}

func TestPoolRetriesWithBackoff(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	startPool(t, repo, map[string]Handler{"mail": func(context.Context, Job) error { return errors.New("smtp down") }})
	id := repo.add("mail", 3, 0)

	waitAttempted(t, repo, id, models.JobStatusPending)
	j := repo.get(id)
	assert.Equal(t, "smtp down", j.LastError)
	assert.Equal(t, int32(1), j.Attempts)
	repo.mu.Lock()
	in := repo.failIn[id]
	repo.mu.Unlock()
	assert.Equal(t, Backoff(1, time.Second, time.Minute, 0.5), in, "base, with the jitter")
	time.Sleep(10 * testPoll)
	assert.Equal(t, int32(1), repo.get(id).Attempts, "not due for a second: no second run yet")
}

func TestPoolRetryThenSuccess(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	var calls atomic.Int32
	attempts := make(chan int, 4)
	startPool(t, repo, map[string]Handler{"mail": func(_ context.Context, j Job) error {
		attempts <- j.Attempt
		if calls.Add(1) == 1 {
			return errors.New("first fails")
		}
		return nil
	}}, func(c *PoolConfig) { c.BackoffBase, c.BackoffMax = 10*time.Millisecond, 10*time.Millisecond })
	id := repo.add("mail", 3, 0)

	waitStatus(t, repo, id, models.JobStatusSucceeded)
	assert.Equal(t, []int{1, 2}, []int{<-attempts, <-attempts})
}

func TestPoolFailureOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err         error
		name        string
		wantError   string
		wantStatus  string
		maxAttempts int32
	}{
		{errors.New("boom"), "last attempt is dead", "boom", models.JobStatusDead, 1},
		{Permanent(errors.New("bad payload")), "permanent is dead at once", "bad payload", models.JobStatusDead, 5},
		{fmt.Errorf("wrapped: %w", Permanent(errors.New("gone"))), "wrapped permanent is dead", "wrapped: gone", models.JobStatusDead, 5},
		{errors.New("again"), "an early failure is retried", "again", models.JobStatusPending, 5},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return tt.err }})
			id := repo.add("a", tt.maxAttempts, 0)
			waitAttempted(t, repo, id, tt.wantStatus)
			j := repo.get(id)
			assert.Equal(t, tt.wantError, j.LastError)
			assert.Equal(t, int32(1), j.Attempts)
		})
	}
}

func TestPoolRecoversFromAPanic(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	startPool(t, repo, map[string]Handler{
		"boom": func(context.Context, Job) error { panic("secret payload value") },
		"ok":   func(context.Context, Job) error { return nil },
	}, func(c *PoolConfig) { c.Workers = 1 })
	bad := repo.add("boom", 3, 0)
	waitAttempted(t, repo, bad, models.JobStatusPending)

	j := repo.get(bad)
	assert.Equal(t, panicMessage, j.LastError, "the panic value is not stored")
	assert.NotContains(t, j.LastError, "secret")

	good := repo.add("ok", 3, 0)
	waitStatus(t, repo, good, models.JobStatusSucceeded)
}

func TestPoolPanicWithRuntimeErrorCountsAsFailure(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error {
		var p *int
		*p = 1
		return nil
	}})
	id := repo.add("a", 1, 0)
	waitStatus(t, repo, id, models.JobStatusDead)
	assert.Equal(t, panicMessage, repo.get(id).LastError)
}

func TestPoolExtendsTheLeaseWhileAHandlerRuns(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	release := make(chan struct{})
	started := make(chan struct{})
	startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}})
	id := repo.add("a", 3, 0)
	<-started
	first := *repo.get(id).LockedUntil

	require.Eventually(t, func() bool { return repo.extends() >= 3 }, waitFor, tick, "a beat every third of the visibility timeout")
	assert.True(t, repo.get(id).LockedUntil.After(first), "the lease moved")
	assert.Equal(t, models.JobStatusRunning, repo.status(id))
	close(release)
	waitStatus(t, repo, id, models.JobStatusSucceeded)
	n := repo.extends()
	time.Sleep(testVisibility)
	assert.LessOrEqual(t, repo.extends(), n+1, "no beat after the job ended")
}

func TestPoolLostLeaseCancelsTheJobAndDropsTheResult(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.extendFn = func(uint64) (bool, error) { return false, nil }
	ctxErr := make(chan error, 1)
	startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		select {
		case <-ctx.Done():
			ctxErr <- context.Cause(ctx)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}})
	id := repo.add("a", 3, 0)

	select {
	case err := <-ctxErr:
		assert.ErrorIs(t, err, errLeaseLost)
	case <-time.After(waitFor):
		t.Fatal("the job context was not cancelled")
	}
	// The row is still "running" in the fake (it was never taken over), and
	// nothing wrote a result for it.
	time.Sleep(50 * time.Millisecond)
	repo.mu.Lock()
	_, failed := repo.failIn[id]
	repo.mu.Unlock()
	assert.False(t, failed, "the result of a lost job is dropped")
	assert.Equal(t, models.JobStatusRunning, repo.status(id))
}

func TestPoolSurvivesAFailedExtend(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	var calls atomic.Int32
	repo.extendFn = func(uint64) (bool, error) {
		if calls.Add(1) == 1 {
			return false, errors.New("db hiccup")
		}
		return true, nil
	}
	cancelled := atomic.Bool{}
	startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		deadline := time.After(400 * time.Millisecond)
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			return ctx.Err()
		case <-deadline:
			return nil
		}
	}}, func(c *PoolConfig) { c.VisibilityTimeout = 300 * time.Millisecond })
	id := repo.add("a", 3, 0)
	waitStatus(t, repo, id, models.JobStatusSucceeded)
	assert.False(t, cancelled.Load(), "a database error is not a lost lease")
	assert.GreaterOrEqual(t, int(calls.Load()), 2, "the beat goes on after the failure")
}

func TestPoolDoesNotClaimWithoutHandlers(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	id := repo.add("mail", 3, 0)
	startPool(t, repo, nil)
	time.Sleep(15 * testPoll)
	assert.Zero(t, repo.claims(), "no registered type: no claim")
	assert.Equal(t, models.JobStatusPending, repo.status(id))
}

func TestPoolClaimsOnlyRegisteredTypes(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	other := repo.add("other", 3, 0)
	mine := repo.add("mine", 3, 0)
	startPool(t, repo, map[string]Handler{"mine": func(context.Context, Job) error { return nil }})
	waitStatus(t, repo, mine, models.JobStatusSucceeded)
	assert.Equal(t, models.JobStatusPending, repo.status(other), "a type of another replica stays pending")
	assert.Empty(t, repo.claimTypesNotIn("mine"))
}

func TestPoolIdleDoesNotSpin(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return nil }},
		func(c *PoolConfig) { c.PollInterval = 50 * time.Millisecond })
	time.Sleep(500 * time.Millisecond)
	n := repo.claims()
	assert.Positive(t, n)
	assert.LessOrEqual(t, n, 14, "about one claim per poll interval, with jitter")
}

func TestPoolAllWorkersBusyDoesNotPoll(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	release := make(chan struct{})
	var running atomic.Int32
	startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		running.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}, func(c *PoolConfig) { c.Workers = 2; c.VisibilityTimeout = time.Minute })
	for range 4 {
		repo.add("a", 3, 0)
	}
	require.Eventually(t, func() bool { return running.Load() == 2 }, waitFor, tick)
	n := repo.claims()
	time.Sleep(15 * testPoll)
	assert.Equal(t, n, repo.claims(), "no claim while every worker is busy")
	close(release)
	require.Eventually(t, func() bool { return running.Load() == 4 }, waitFor, tick, "the rest runs when workers free up")
}

func TestPoolRunsAtMostWorkersJobsAtOnceAndClaimsABatch(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	var cur, peak atomic.Int32
	var done atomic.Int32
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		done.Add(1)
		return nil
	}}, func(c *PoolConfig) { c.Workers = 3 })
	for range 9 {
		repo.add("a", 3, 0)
	}
	require.Eventually(t, func() bool { return done.Load() == 9 }, waitFor, tick)
	assert.Equal(t, 3, int(peak.Load()), "the workers are used in parallel, never more than JOBS_WORKERS")

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.NotEmpty(t, repo.claimCalls)
	for _, ids := range repo.claimCalls {
		assert.LessOrEqual(t, len(ids), 3)
	}
}

func TestPoolClaimsABatchUpToTheIdleWorkers(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return nil }},
		func(c *PoolConfig) { c.Workers = 3 })
	require.Eventually(t, func() bool { return repo.claims() >= 1 }, waitFor, tick)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, []string{"replica-a#1", "replica-a#2", "replica-a#3"}, sortedCopy(repo.claimCalls[0]), "one claim for every idle worker")
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func TestPoolWorkerIDs(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	release := make(chan struct{})
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error {
		<-release
		return nil
	}}, func(c *PoolConfig) { c.Workers = 2; c.VisibilityTimeout = time.Minute })
	a, b := repo.add("a", 3, 0), repo.add("a", 3, 0)
	require.Eventually(t, func() bool {
		return repo.get(a).LockedBy != "" && repo.get(b).LockedBy != ""
	}, waitFor, tick)
	assert.ElementsMatch(t, []string{"replica-a#1", "replica-a#2"}, []string{repo.get(a).LockedBy, repo.get(b).LockedBy})
	close(release)
}

func TestPoolLongProcessIDKeepsTheWorkerIDShort(t *testing.T) {
	t.Parallel()
	p := NewPool(PoolConfig{Repo: newFakeRepo(), Registry: NewRegistry(), ProcessID: strings.Repeat("h", 400), Workers: 64})
	assert.LessOrEqual(t, len(p.workerID(63)), models.JobWorkerIDMaxLen)
}

func TestPoolBacksOffAfterAClaimError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.claimErr = errors.New("db down")
	id := repo.add("a", 3, 0)
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return nil }},
		func(c *PoolConfig) { c.PollInterval = 20 * time.Millisecond })
	time.Sleep(400 * time.Millisecond)
	n := repo.claims()
	assert.GreaterOrEqual(t, n, 2)
	assert.LessOrEqual(t, n, 7, "the wait doubles after each failure")

	repo.mu.Lock()
	repo.claimErr = nil
	repo.mu.Unlock()
	waitStatus(t, repo, id, models.JobStatusSucceeded)
}

func TestErrorBackoff(t *testing.T) {
	t.Parallel()
	assert.Equal(t, time.Second, errorBackoff(time.Second, 1))
	assert.Equal(t, 2*time.Second, errorBackoff(time.Second, 2))
	assert.Equal(t, 8*time.Second, errorBackoff(time.Second, 4))
	assert.Equal(t, maxErrorBackoff, errorBackoff(time.Second, 50))
	assert.Equal(t, time.Minute, errorBackoff(time.Minute, 5), "never below the poll interval")
}

func TestPoolCountsExpiredJobsAsDead(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := newMetrics(provider.Meter("jobs"))
	require.NoError(t, err)
	t.Cleanup(m.Close)

	repo := newFakeRepo()
	var once sync.Once
	repo.expiredFn = func() []models.Job {
		var out []models.Job
		once.Do(func() { out = []models.Job{{ID: 99, Type: "a", Attempts: 3}} })
		return out
	}
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return nil }},
		func(c *PoolConfig) { c.Metrics = m })
	require.Eventually(t, func() bool {
		sum, ok := collect(t, reader)["jobs.processed"].Data.(metricdata.Sum[int64])
		if !ok || len(sum.DataPoints) != 1 {
			return false
		}
		out, _ := sum.DataPoints[0].Attributes.Value("outcome")
		return out.AsString() == OutcomeDead && sum.DataPoints[0].Value == 1
	}, waitFor, tick)
}

func TestPoolRecordsOutcomesInMetrics(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := newMetrics(provider.Meter("jobs"))
	require.NoError(t, err)
	t.Cleanup(m.Close)

	repo := newFakeRepo()
	repo.stats = models.JobStats{Pending: 5, OldestDueAge: 7 * time.Second}
	startPool(t, repo, map[string]Handler{
		"ok":    func(context.Context, Job) error { return nil },
		"retry": func(context.Context, Job) error { return errors.New("x") },
		"dead":  func(context.Context, Job) error { return Permanent(errors.New("x")) },
	}, func(c *PoolConfig) { c.Metrics = m })
	a, b, c := repo.add("ok", 3, 0), repo.add("retry", 3, 0), repo.add("dead", 3, 0)
	waitStatus(t, repo, a, models.JobStatusSucceeded)
	waitAttempted(t, repo, b, models.JobStatusPending)
	waitStatus(t, repo, c, models.JobStatusDead)

	require.Eventually(t, func() bool {
		sum, ok := collect(t, reader)["jobs.processed"].Data.(metricdata.Sum[int64])
		return ok && len(sum.DataPoints) == 3
	}, waitFor, tick)
	got := collect(t, reader)
	counts := map[string]int64{}
	for _, dp := range got["jobs.processed"].Data.(metricdata.Sum[int64]).DataPoints {
		typ, _ := dp.Attributes.Value("type")
		out, _ := dp.Attributes.Value("outcome")
		counts[typ.AsString()+"/"+out.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{"ok/succeeded": 1, "retry/retried": 1, "dead/dead": 1}, counts)
	hist := got["jobs.duration"].Data.(metricdata.Histogram[float64])
	assert.NotEmpty(t, hist.DataPoints)
	age := got["jobs.oldest_due.age"].Data.(metricdata.Gauge[float64])
	assert.InDelta(t, 7.0, age.DataPoints[0].Value, 0.001, "the depth comes from the cached sample")
}

func TestPoolStatsAreSampledAndCached(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.stats = models.JobStats{Pending: 2, Due: 1, OldestDueAge: time.Minute}
	p := startPool(t, repo, nil, func(c *PoolConfig) { c.StatsInterval = 50 * time.Millisecond })

	require.Eventually(t, func() bool { _, ok := p.Stats(); return ok }, waitFor, tick)
	s, _ := p.Stats()
	assert.Equal(t, int64(2), s.Pending)
	assert.Equal(t, time.Minute, s.OldestDueAge)
	assert.WithinDuration(t, time.Now(), s.At, 2*time.Second)

	repo.mu.Lock()
	repo.stats = models.JobStats{Pending: 9}
	repo.mu.Unlock()
	require.Eventually(t, func() bool { s, _ := p.Stats(); return s.Pending == 9 }, waitFor, tick, "resampled")

	// A failing sample keeps the last value.
	repo.mu.Lock()
	repo.statsErr = errors.New("down")
	repo.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	s, ok := p.Stats()
	assert.True(t, ok)
	assert.Equal(t, int64(9), s.Pending)
}

func TestPoolStatsReadMakesNoQuery(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	p := startPool(t, repo, nil)
	require.Eventually(t, func() bool { _, ok := p.Stats(); return ok }, waitFor, tick)
	repo.mu.Lock()
	n := repo.statsCalls
	repo.mu.Unlock()
	for range 100 {
		p.Stats()
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, n, repo.statsCalls, "StatsInterval is an hour: reads come from the cache")
}

func TestPoolStatsUnavailableBeforeTheFirstSample(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.statsErr = errors.New("down")
	p := startPool(t, repo, nil)
	time.Sleep(50 * time.Millisecond)
	_, ok := p.Stats()
	assert.False(t, ok)
}

func TestPoolRunningFlagAndRestart(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	p := startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return nil }})
	assert.True(t, p.Running())
	p.Start() // a second Start does nothing
	require.NoError(t, p.Stop(context.Background()))
	assert.False(t, p.Running())
	assert.NoError(t, p.Stop(context.Background()), "a second Stop does nothing")

	p.Start()
	assert.True(t, p.Running())
	id := repo.add("a", 3, 0)
	waitStatus(t, repo, id, models.JobStatusSucceeded)
}

func TestPoolStopDrainsRunningJobs(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	started := make(chan struct{})
	p := startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		close(started)
		time.Sleep(100 * time.Millisecond)
		return nil
	}}, func(c *PoolConfig) { c.Workers = 1; c.VisibilityTimeout = time.Minute })
	id := repo.add("a", 3, 0)
	<-started
	more := repo.add("a", 3, 0)

	start := time.Now()
	require.NoError(t, p.Stop(context.Background()))
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, models.JobStatusSucceeded, repo.status(id), "a job that finishes within the drain time completes")
	assert.Equal(t, models.JobStatusPending, repo.status(more), "the second job was never claimed")
	assert.Empty(t, repo.releaseCalls)
}

func TestPoolStopStopsClaimingAtOnce(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	p := startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return nil }})
	require.NoError(t, p.Stop(context.Background()))
	n := repo.claims()
	id := repo.add("a", 3, 0)
	time.Sleep(10 * testPoll)
	assert.Equal(t, n, repo.claims())
	assert.Equal(t, models.JobStatusPending, repo.status(id))
}

func TestPoolStopCancelsAndReleasesAJobThatHonoursTheContext(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	p := startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}}, func(c *PoolConfig) { c.DrainTimeout = 60 * time.Millisecond; c.VisibilityTimeout = time.Minute })
	id := repo.add("a", 3, 0)
	<-started

	start := time.Now()
	require.NoError(t, p.Stop(context.Background()))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 60*time.Millisecond, "the drain time is given first")
	assert.Less(t, elapsed, time.Second)
	select {
	case <-cancelled:
	default:
		t.Fatal("the handler context was not cancelled")
	}
	j := repo.get(id)
	assert.Equal(t, models.JobStatusPending, j.Status, "released")
	assert.Equal(t, int32(0), j.Attempts, "the attempt is not counted")
	assert.Empty(t, j.LastError, "a shutdown is not a failure")
}

func TestPoolStopReleasesAHandlerThatIgnoresTheContext(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	started := make(chan struct{})
	unblock := make(chan struct{})
	p := startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error {
		close(started)
		<-unblock
		return nil
	}}, func(c *PoolConfig) {
		c.DrainTimeout, c.ReleaseTimeout, c.VisibilityTimeout = 30*time.Millisecond, 200*time.Millisecond, time.Minute
	})
	id := repo.add("a", 3, 0)
	<-started

	start := time.Now()
	err := p.Stop(context.Background())
	require.Error(t, err, "a handler that never returns is reported")
	assert.Less(t, time.Since(start), time.Second, "Stop is bounded")
	assert.Equal(t, models.JobStatusPending, repo.status(id), "released by Stop")
	assert.Equal(t, int32(0), repo.get(id).Attempts)

	// The late result of the hung handler is dropped by the fence.
	close(unblock)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, models.JobStatusPending, repo.status(id))
}

func TestPoolStopSkipsTheDrainWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	started := make(chan struct{})
	p := startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}, func(c *PoolConfig) { c.DrainTimeout = time.Minute; c.VisibilityTimeout = time.Minute })
	id := repo.add("a", 3, 0)
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	require.NoError(t, p.Stop(ctx))
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, models.JobStatusPending, repo.status(id), "the release has a deadline of its own")
}

func TestPoolHandOutReleasesJobsClaimedWhileStopping(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	reg := NewRegistry()
	reg.Register("a", func(context.Context, Job) error { return nil })
	p := NewPool(PoolConfig{Repo: repo, Registry: reg, Workers: 1, PollInterval: time.Hour, VisibilityTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := &poolRun{claimCtx: ctx, idle: make(chan int, 1), work: []chan task{make(chan task, 1)}, running: map[uint64]string{}}
	id := repo.add("a", 3, 0)
	claim, err := repo.Claim(context.Background(), []string{p.workerID(0)}, []string{"a"}, time.Minute)
	require.NoError(t, err)
	require.Len(t, claim.Jobs, 1)

	p.handOut(run, []int{0}, claim.Jobs)

	assert.Equal(t, models.JobStatusPending, repo.status(id))
	assert.Equal(t, int32(0), repo.get(id).Attempts)
	assert.Len(t, run.idle, 1, "the worker slot is free again")
	assert.Empty(t, run.work[0])
}

func TestPoolKeepsTheRowRunningWhenTheResultCannotBeWritten(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.completeFn = func(uint64) error { return errors.New("db down") }
	startPool(t, repo, map[string]Handler{"a": func(context.Context, Job) error { return nil }},
		func(c *PoolConfig) { c.VisibilityTimeout = time.Minute })
	id := repo.add("a", 3, 0)
	require.Eventually(t, func() bool { return repo.get(id).Attempts == 1 }, waitFor, tick)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, models.JobStatusRunning, repo.status(id), "the row is claimed again after its lease")
}

func TestPoolOneJobPerWorker(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	var started atomic.Int32
	release := make(chan struct{})
	startPool(t, repo, map[string]Handler{"a": func(ctx context.Context, _ Job) error {
		started.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}, func(c *PoolConfig) { c.Workers = 1; c.VisibilityTimeout = time.Minute })
	repo.add("a", 3, 0)
	repo.add("a", 3, 0)
	time.Sleep(15 * testPoll)
	assert.Equal(t, int32(1), started.Load())
	close(release)
}

func TestCallTraceNamesTheCallers(t *testing.T) {
	t.Parallel()

	trace := func() string { return callTrace() }()

	assert.Contains(t, trace, "TestCallTraceNamesTheCallers")
	assert.Contains(t, trace, " <- ")
	assert.NotContains(t, trace, "\n")
}

package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"backend/internal/ctxutil"
	"backend/internal/logutil"
	"backend/internal/models"
)

// Fixed budgets and defaults of the pool.
const (
	// DefaultDrainTimeout is how long Stop lets the running jobs finish
	// before it cancels their contexts.
	DefaultDrainTimeout = 5 * time.Second
	// DefaultReleaseTimeout is the time that Stop has after the cancel to give
	// the unfinished jobs back (Release).
	DefaultReleaseTimeout = 2 * time.Second
	// DefaultStatsInterval is how often the pool samples the queue depth.
	DefaultStatsInterval = 30 * time.Second
	// claimTimeout bounds one claim transaction.
	claimTimeout = 5 * time.Second
	// reportTimeout bounds the write of the result of one job.
	reportTimeout = 10 * time.Second
	// statsTimeout bounds one sample of the queue depth.
	statsTimeout = 10 * time.Second
	// maxErrorBackoff is the longest wait after a failed claim.
	maxErrorBackoff = 30 * time.Second
	// pollJitter is the share of the poll interval that is added or taken off
	// at random, so that replicas do not poll in step.
	pollJitter = 0.2
	// logInterval is the interval of the Error line of a failing claim.
	logInterval = time.Minute
	// workerIDPrefixMax keeps the worker ID within jobs.locked_by (260):
	// the prefix, "#" and the worker number of at most 2 digits.
	workerIDPrefixMax = 250
)

// Causes of the cancel of a job context.
var (
	errShutdown  = errors.New("jobs: the process is shutting down")
	errLeaseLost = errors.New("jobs: the job was taken over")
)

// panicMessage is the last_error of a job whose handler panicked. The panic
// value is not stored: it can hold payload data.
const panicMessage = "handler panicked"

// StatsSnapshot is the queue depth that the pool sampled last.
type StatsSnapshot struct {
	// At is when the pool took the sample.
	At time.Time
	models.JobStats
}

// PoolConfig configures a Pool. Repo and Registry are required; a zero value
// of any other field uses its default.
type PoolConfig struct {
	// Repo is the job repository.
	Repo models.JobRepository
	// Registry holds the handlers of this process; the pool claims only these
	// types.
	Registry *Registry
	// Metrics receives the outcomes; nil means none.
	Metrics *Metrics
	// Rand returns a value in [0, 1) for the jitter (tests replace it).
	Rand func() float64
	// ProcessID is the identity of this process (the replica ID); a worker ID
	// is ProcessID + "#" + the worker number.
	ProcessID string
	// Workers is the number of worker goroutines (JOBS_WORKERS, default 4).
	Workers int
	// PollInterval is the time between two claims of an idle pool
	// (JOBS_POLL_INTERVAL, default 1s).
	PollInterval time.Duration
	// VisibilityTimeout is the lease of a running job (JOBS_VISIBILITY_TIMEOUT,
	// default 5m). It is extended every third of it.
	VisibilityTimeout time.Duration
	// BackoffBase and BackoffMax shape the retry delay (JOBS_BACKOFF_BASE
	// default 10s, JOBS_BACKOFF_MAX default 1h).
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// StatsInterval is the time between two samples of the queue depth
	// (default 30 s).
	StatsInterval time.Duration
	// DrainTimeout is the fixed time that Stop gives running jobs (default
	// 5 s); ReleaseTimeout the fixed time after it to release them (2 s).
	DrainTimeout   time.Duration
	ReleaseTimeout time.Duration
}

// task is a claimed job that a worker runs.
type task struct {
	handler  Handler
	workerID string
	job      Job
}

// poolRun is the state of one Start..Stop.
type poolRun struct {
	// claimCtx ends when Stop starts: the dispatcher claims no more.
	claimCtx context.Context
	// jobCtx is the parent of the job contexts: it ends after the drain time.
	jobCtx context.Context
	// stopClaiming and cancelJobs end the two contexts above.
	stopClaiming context.CancelFunc
	cancelJobs   context.CancelCauseFunc
	// idle holds the numbers of the workers that wait for a job.
	idle chan int
	// running maps the ID of a job that a worker holds to its worker ID (for
	// the release of a handler that does not return). Guarded by mu.
	running map[uint64]string
	// work[i] feeds worker i.
	work []chan task
	// loops ends when the dispatcher and the stats sampler returned; workers
	// when every worker goroutine returned.
	loops   sync.WaitGroup
	workers sync.WaitGroup
	mu      sync.Mutex
}

// Pool runs the jobs of this process: a dispatcher that claims, the worker
// goroutines that run handlers and a sampler of the queue depth. One per
// process; every replica has one (it is not a leader worker).
type Pool struct {
	// current is the active run, nil when stopped. Guarded by mu.
	current *poolRun
	cfg     *PoolConfig

	limiter *logutil.Limiter
	beatLog *logutil.Limiter

	// stats is the last sample. Guarded by statsMu.
	stats   StatsSnapshot
	statsMu sync.RWMutex
	hasStat bool

	mu      sync.Mutex
	running atomic.Bool
}

// NewPool returns a pool for cfg, or nil when Repo or Registry is missing. The
// methods of a nil *Pool do nothing.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.Repo == nil || cfg.Registry == nil {
		return nil
	}

	if cfg.Workers < 1 {
		cfg.Workers = 4
	}

	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}

	if cfg.VisibilityTimeout <= 0 {
		cfg.VisibilityTimeout = 5 * time.Minute
	}

	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 10 * time.Second
	}

	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = time.Hour
	}

	if cfg.StatsInterval <= 0 {
		cfg.StatsInterval = DefaultStatsInterval
	}

	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}

	if cfg.ReleaseTimeout <= 0 {
		cfg.ReleaseTimeout = DefaultReleaseTimeout
	}

	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}

	if cfg.ProcessID == "" {
		cfg.ProcessID = "jobs"
	}

	if len(cfg.ProcessID) > workerIDPrefixMax {
		cfg.ProcessID = cfg.ProcessID[:workerIDPrefixMax]
	}

	return &Pool{cfg: &cfg, limiter: logutil.NewLimiter(logInterval), beatLog: logutil.NewLimiter(logInterval)}
}

// workerID returns the locked_by of worker slot i (0 based).
func (p *Pool) workerID(i int) string {
	return fmt.Sprintf("%s#%d", p.cfg.ProcessID, i+1)
}

// Running reports whether the pool claims jobs: true from Start until Stop
// begins. The health check reads it.
func (p *Pool) Running() bool {
	return p != nil && p.running.Load()
}

// Stats returns the last sample of the queue depth and whether there is one.
// The pool samples it every StatsInterval (30 s); the call makes no query.
func (p *Pool) Stats() (StatsSnapshot, bool) {
	if p == nil {
		return StatsSnapshot{}, false
	}

	p.statsMu.RLock()
	defer p.statsMu.RUnlock()

	return p.stats, p.hasStat
}

// Start starts the pool in goroutines and returns at once. A second Start does
// nothing; Start after Stop starts again.
func (p *Pool) Start() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.current != nil {
		return
	}

	claimCtx, stopClaiming := context.WithCancel(context.Background())
	jobCtx, cancelJobs := context.WithCancelCause(context.Background())
	run := &poolRun{
		claimCtx:     claimCtx,
		jobCtx:       jobCtx,
		stopClaiming: stopClaiming,
		cancelJobs:   cancelJobs,
		idle:         make(chan int, p.cfg.Workers),
		work:         make([]chan task, p.cfg.Workers),
		running:      map[uint64]string{},
	}

	for i := range run.work {
		run.work[i] = make(chan task, 1)
		run.idle <- i
		run.workers.Add(1)

		go p.worker(run, i)
	}

	run.loops.Add(2)

	go func() {
		defer run.loops.Done()
		p.dispatch(run)
	}()
	go func() {
		defer run.loops.Done()
		p.sampleStats(run)
	}()

	p.current = run
	p.running.Store(true)

	slog.Info("Job pool started", "workers", p.cfg.Workers, "poll_interval", p.cfg.PollInterval,
		"visibility_timeout", p.cfg.VisibilityTimeout, "types", p.cfg.Registry.Types())
}

// Stop ends the pool. It stops claiming at once, gives the running jobs up to
// DrainTimeout (5 s) to finish, then cancels their contexts and releases the
// jobs that are still unfinished (back to pending, the attempt not counted),
// within ReleaseTimeout (2 s) more. ctx bounds the whole of it: when ctx ends
// earlier the pool skips the rest of the drain. The release uses its own
// deadline, not ctx, so it still runs. A second Stop does nothing. It returns
// an error when a handler did not return after its context was cancelled (it
// ignored the context); the job is released anyway and its late result is
// dropped by the fence.
func (p *Pool) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	run := p.current
	p.current = nil
	p.mu.Unlock()

	if run == nil {
		return nil
	}

	p.running.Store(false)
	run.stopClaiming()

	done := make(chan struct{})

	go func() {
		run.loops.Wait()

		// The dispatcher is the only sender on the work channels: closing them
		// ends the workers once their current job is done.
		for _, w := range run.work {
			close(w)
		}

		run.workers.Wait()
		close(done)
	}()

	drain := time.NewTimer(p.cfg.DrainTimeout)
	defer drain.Stop()

	select {
	case <-done:
		run.cancelJobs(errShutdown)

		return nil
	case <-drain.C:
	case <-ctx.Done():
	}

	// The release has its own deadline and does not follow ctx.
	rctx, cancel := ctxutil.Detach(ctx, p.cfg.ReleaseTimeout)
	defer cancel()

	run.cancelJobs(errShutdown)

	// The handlers that honour the cancel release their own job. Half of the
	// budget for them, then the release of the ones that are still running.
	half := time.NewTimer(p.cfg.ReleaseTimeout / 2)
	defer half.Stop()

	select {
	case <-done:
		return nil
	case <-half.C:
	case <-rctx.Done():
	}

	p.releaseRunning(rctx, run)

	select {
	case <-done:
		return nil
	case <-rctx.Done():
	}

	run.mu.Lock()
	left := len(run.running)
	run.mu.Unlock()

	err := fmt.Errorf("jobs: %d handler(s) did not return after the cancel", left)
	slog.Error("Job handlers ignored the shutdown; they are released and their results dropped", "running", left)

	return err
}

// releaseRunning gives back every job that a worker still holds.
func (p *Pool) releaseRunning(ctx context.Context, run *poolRun) {
	run.mu.Lock()
	held := make(map[uint64]string, len(run.running))
	for id, wid := range run.running {
		held[id] = wid
	}
	run.mu.Unlock()

	for id, wid := range held {
		ok, err := p.cfg.Repo.Release(ctx, id, wid)
		if err != nil {
			slog.Error("Failed to release a job at shutdown; its lease ends later", "job_id", id, "error", err)

			continue
		}

		if ok {
			slog.Warn("Released a job whose handler did not return at shutdown", "job_id", id)
		}
	}
}

// jitter returns d changed by up to plus or minus pollJitter.
func (p *Pool) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (1 + pollJitter*(2*p.cfg.Rand()-1)))
}

// sleep waits for d or until ctx ends, and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// dispatch is the one goroutine that claims: it waits for an idle worker,
// claims up to the number of idle workers, hands each job to a worker, and
// sleeps one poll interval when the queue gave less than it asked for. It does
// not poll while all workers are busy, and does not spin on an empty queue.
func (p *Pool) dispatch(run *poolRun) {
	failures := 0
	failing := false

	for {
		var slots []int

		select {
		case <-run.claimCtx.Done():
			return
		case s := <-run.idle:
			slots = append(slots, s)
		}

		for more := true; more; {
			select {
			case s := <-run.idle:
				slots = append(slots, s)
			default:
				more = false
			}
		}

		types := p.cfg.Registry.Types()
		if len(types) == 0 {
			// Nothing to run: no claim, and no work for the idle workers.
			p.giveBack(run, slots)

			if !sleep(run.claimCtx, p.jitter(p.cfg.PollInterval)) {
				return
			}

			continue
		}

		ids := make([]string, len(slots))
		for i, s := range slots {
			ids[i] = p.workerID(s)
		}

		// The claim is not tied to claimCtx: a claim that commits while Stop
		// begins must not be lost with its result. Its jobs are released below.
		cctx, cancel := ctxutil.Detach(run.claimCtx, claimTimeout)
		claim, err := p.cfg.Repo.Claim(cctx, ids, types, p.cfg.VisibilityTimeout)

		cancel()

		if err != nil {
			p.giveBack(run, slots)

			failures++

			if ok, suppressed := p.limiter.Allow(); ok {
				slog.Error("Job claim failed; it retries with a backoff", "error", err, "suppressed", suppressed)
			} else {
				slog.Debug("Job claim failed", "error", err)
			}

			failing = true

			if !sleep(run.claimCtx, p.jitter(errorBackoff(p.cfg.PollInterval, failures))) {
				return
			}

			continue
		}

		if failing {
			slog.Info("Job claim works again")

			failing, failures = false, 0
		}

		for _, j := range claim.Expired {
			p.cfg.Metrics.Processed(j.Type, OutcomeDead)
			slog.Warn("A job lost its lease too often and is dead", "job_id", j.ID, "type", j.Type, "attempts", j.Attempts)
		}

		p.handOut(run, slots, claim.Jobs)

		if len(claim.Jobs)+len(claim.Expired) < len(slots) && !sleep(run.claimCtx, p.jitter(p.cfg.PollInterval)) {
			return
		}
	}
}

// errorBackoff is the wait after the n-th failed claim in a row: the poll
// interval doubled each time, at most maxErrorBackoff.
func errorBackoff(poll time.Duration, n int) time.Duration {
	d := poll

	for i := 1; i < n && d < maxErrorBackoff; i++ {
		d *= 2
	}

	return min(d, max(maxErrorBackoff, poll))
}

// giveBack returns worker slots that got no job.
func (p *Pool) giveBack(run *poolRun, slots []int) {
	for _, s := range slots {
		run.idle <- s
	}
}

// handOut gives the claimed jobs to their workers (job k was claimed for
// slots[k]) and returns the unused slots. When Stop began in the meantime the
// jobs are released at once instead.
func (p *Pool) handOut(run *poolRun, slots []int, jobs []models.Job) {
	for k, j := range jobs {
		slot := slots[k]
		wid := p.workerID(slot)

		handler, ok := p.cfg.Registry.Lookup(j.Type)
		if !ok || run.claimCtx.Err() != nil {
			// Unknown (the claim asked only for registered types, so this is a
			// bug) or shutting down: give the job back, the attempt uncounted.
			p.releaseOne(j.ID, wid)
			run.idle <- slot

			continue
		}

		run.mu.Lock()
		run.running[j.ID] = wid
		run.mu.Unlock()

		run.work[slot] <- task{
			job: Job{
				ID: j.ID, Type: j.Type, Payload: []byte(j.Payload),
				Attempt: int(j.Attempts), MaxAttempts: int(j.MaxAttempts),
			},
			handler:  handler,
			workerID: wid,
		}
	}

	p.giveBack(run, slots[len(jobs):])
}

// releaseOne gives a claimed job back from the dispatcher.
func (p *Pool) releaseOne(id uint64, workerID string) {
	ctx, cancel := ctxutil.Detach(context.Background(), p.cfg.ReleaseTimeout)
	defer cancel()

	if _, err := p.cfg.Repo.Release(ctx, id, workerID); err != nil {
		slog.Error("Failed to release a job that was not started; its lease ends later", "job_id", id, "error", err)
	}
}

// worker runs the jobs that the dispatcher gives to slot i, one at a time.
func (p *Pool) worker(run *poolRun, i int) {
	defer run.workers.Done()

	for t := range run.work[i] {
		p.execute(run, t)

		run.mu.Lock()
		delete(run.running, t.job.ID)
		run.mu.Unlock()

		run.idle <- i
	}
}

// execute runs one job: the handler (with the lease heartbeat beside it) and
// then the write of its result.
func (p *Pool) execute(run *poolRun, t task) {
	jctx, cancel := context.WithCancelCause(run.jobCtx)
	defer cancel(nil)

	var lost atomic.Bool

	stopBeat := p.heartbeat(jctx, cancel, &lost, t)
	start := time.Now()
	err := p.invoke(jctx, t)

	stopBeat()
	p.cfg.Metrics.Duration(t.job.Type, time.Since(start))

	switch {
	case lost.Load():
		// Another worker holds the job now: its result is the one that counts.
		slog.Warn("A job was taken over while it ran; its result is dropped", "job_id", t.job.ID, "type", t.job.Type)
	case err == nil:
		p.report(t, OutcomeSucceeded, func(ctx context.Context) (bool, error) {
			return p.cfg.Repo.Complete(ctx, t.job.ID, t.workerID)
		}, reportTimeout)
	case errors.Is(context.Cause(jctx), errShutdown):
		// The process is stopping and the handler gave up: not a failure of
		// the job. It goes back with its attempt uncounted.
		p.report(t, OutcomeReleased, func(ctx context.Context) (bool, error) {
			return p.cfg.Repo.Release(ctx, t.job.ID, t.workerID)
		}, p.cfg.ReleaseTimeout)
	case IsPermanent(err) || t.job.Attempt >= t.job.MaxAttempts:
		p.report(t, OutcomeDead, func(ctx context.Context) (bool, error) {
			return p.cfg.Repo.Fail(ctx, t.job.ID, t.workerID, failureText(err), 0, true)
		}, reportTimeout)
	default:
		delay := Backoff(t.job.Attempt, p.cfg.BackoffBase, p.cfg.BackoffMax, p.cfg.Rand())
		p.report(t, OutcomeRetried, func(ctx context.Context) (bool, error) {
			return p.cfg.Repo.Fail(ctx, t.job.ID, t.workerID, failureText(err), delay, false)
		}, reportTimeout)
	}
}

// failureText is the last_error of a failed attempt: the text of the error
// that the handler returned. The repository cuts it to 2000 characters. It
// never holds the payload (unless the handler put it in its error).
func failureText(err error) string {
	if errors.Is(err, errPanicked) {
		return panicMessage
	}

	return err.Error()
}

// report writes the result of a job with a context that outlives the pool
// contexts (a result must be written also while the pool shuts down), counts
// the outcome and logs what the fence or the database refused.
func (p *Pool) report(t task, outcome string, write func(ctx context.Context) (bool, error), timeout time.Duration) {
	ctx, cancel := ctxutil.Detach(context.Background(), timeout)
	defer cancel()

	ok, err := write(ctx)

	switch {
	case err != nil:
		// The row stays running until its lease ends, then it runs again.
		slog.Error("Failed to record the result of a job; it runs again after its lease", "job_id", t.job.ID, "type", t.job.Type, "outcome", outcome, "error", err)
	case !ok:
		slog.Warn("A job was taken over before its result was recorded; the result is dropped", "job_id", t.job.ID, "type", t.job.Type, "outcome", outcome)
	default:
		p.cfg.Metrics.Processed(t.job.Type, outcome)
	}
}

// errPanicked marks the error of a handler that panicked.
var errPanicked = errors.New(panicMessage)

// invoke runs the handler and turns a panic into an error. The stack is
// logged, the panic value only when it is a runtime error (an index or nil
// dereference); the payload is never logged.
func (p *Pool) invoke(ctx context.Context, t task) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		attrs := []any{"job_id", t.job.ID, "type", t.job.Type, "panic_type", fmt.Sprintf("%T", r), "stack", string(debug.Stack())}
		if re, ok := r.(runtime.Error); ok {
			attrs = append(attrs, "panic", re.Error())
		}

		slog.Error("Job handler panicked", attrs...)

		err = errPanicked
	}()

	return t.handler(ctx, t.job)
}

// heartbeat extends the lease of the job every third of the visibility
// timeout while the handler runs. An Extend that finds the job taken over
// (the fence) cancels the job context and sets lost; an Extend that fails on
// the database is retried at the next beat, because the lease is not proven
// lost. It returns the function that stops the heartbeat and waits for it.
func (p *Pool) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, lost *atomic.Bool, t task) func() {
	interval := max(p.cfg.VisibilityTimeout/3, time.Millisecond)
	stop := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			ectx, ecancel := context.WithTimeout(ctx, interval)
			ok, err := p.cfg.Repo.Extend(ectx, t.job.ID, t.workerID, p.cfg.VisibilityTimeout)

			ecancel()

			switch {
			case err != nil:
				if ctx.Err() != nil {
					return
				}

				if allow, suppressed := p.beatLog.Allow(); allow {
					slog.Error("Failed to extend the lease of a job; it retries at the next beat", "job_id", t.job.ID, "type", t.job.Type, "error", err, "suppressed", suppressed)
				} else {
					slog.Debug("Failed to extend the lease of a job", "job_id", t.job.ID, "error", err)
				}
			case !ok:
				lost.Store(true)
				cancel(errLeaseLost)

				return
			}
		}
	}()

	return func() {
		close(stop)
		<-stopped
	}
}

// sampleStats reads the queue depth once at start and then every
// StatsInterval, and keeps the last sample for the health check and the
// metrics. A failed sample keeps the old one.
func (p *Pool) sampleStats(run *poolRun) {
	sample := func() {
		ctx, cancel := context.WithTimeout(run.claimCtx, statsTimeout)
		defer cancel()

		s, err := p.cfg.Repo.Stats(ctx)
		if err != nil {
			if run.claimCtx.Err() == nil {
				slog.Debug("Failed to sample the job queue depth", "error", err)
			}

			return
		}

		p.statsMu.Lock()
		p.stats, p.hasStat = StatsSnapshot{JobStats: s, At: time.Now()}, true
		p.statsMu.Unlock()
		p.cfg.Metrics.ObserveStats(s)
	}

	sample()

	ticker := time.NewTicker(p.cfg.StatsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-run.claimCtx.Done():
			return
		case <-ticker.C:
			sample()
		}
	}
}

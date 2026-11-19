// Package outbox delivers the WebSocket events of the optional outbox module
// (OUTBOX_ENABLED=true).
//
// The Items repository writes one row per event to outbox_events in the
// transaction of the change (ItemHook). Every replica runs a Poller that reads
// the rows after its cursor in id order and sends each message to the sockets
// of its own hub. A row is committed together with its change, so an event is
// not lost when the writing replica dies before it delivers: any running
// poller delivers it.
//
// Auto increment ids can commit out of order: a transaction that took id 5
// may commit after the one that took id 6. The poller therefore remembers the
// ids below its cursor that it has not seen (the gaps) for a gap window and
// delivers a row when it appears. A transaction that stays open longer than
// the window can still be skipped.
package outbox

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"backend/internal/logutil"
	"backend/internal/models"
	"backend/internal/websocket"
)

// Defaults of a Config field that is zero.
const (
	// DefaultInterval is the poll interval of a Config without one.
	DefaultInterval = 500 * time.Millisecond
	// DefaultBatchSize is the number of rows of one read. It is well below
	// half of the 256 slot broadcast queue of the hub: the hub drops a
	// message that does not fit, and the cursor has moved on by then.
	DefaultBatchSize = 100
	// DefaultBackpressureTimeout is the longest the poller waits for the hub
	// queues to drain, once per poll, before it sends anyway.
	DefaultBackpressureTimeout = 2 * time.Second
	// backpressurePause is the time between two looks at the hub queue.
	backpressurePause = time.Millisecond
	// DefaultMaxMissing bounds the gap ids that a poller tracks.
	DefaultMaxMissing = 10_000
	// MinGapWindow is the shortest gap window: a transaction that commits
	// within it after a later one is still delivered.
	MinGapWindow = 10 * time.Second
	// gapLookupChunk is the number of ids of one gap lookup.
	gapLookupChunk = 500
	// pollTimeout bounds one poll (all its reads).
	pollTimeout = 30 * time.Second
	// logInterval is the interval of the Error line of a failing poll.
	logInterval = time.Minute
)

// GapWindow returns the gap window for a poll interval: at least MinGapWindow
// and at least four polls.
func GapWindow(interval time.Duration) time.Duration {
	return max(MinGapWindow, 4*interval)
}

// Stats is what one poll reports to an Observer.
type Stats struct {
	// Age is how old the newest delivered event was at delivery, by the
	// database clock. Valid only when HasAge is true.
	Age time.Duration
	// IDLag is the number of ids between the cursor and the table maximum
	// at the end of the poll. Valid only when HasLag is true.
	IDLag uint64
	// Delivered is the number of events sent in this poll.
	Delivered int
	// HasAge is true when Age is set (an event was delivered and still exists).
	HasAge bool
	// HasLag is true when IDLag was measured (the query worked); otherwise
	// the lag is unknown and a gauge keeps its last value.
	HasLag bool
}

// backlogReporter is a sender that reports how full its busiest queue is
// (*websocket.Hub: the broadcast queue and the send queue of every client).
// The poller waits while the backlog is at least half of the capacity.
type backlogReporter interface {
	BroadcastBacklog() (pending, capacity int)
}

// The hub must keep this method: without it the poller sends without pacing.
var _ backlogReporter = (*websocket.Hub)(nil)

// Observer receives the stats of every poll (the metrics). A nil Observer
// costs nothing: the poller then makes no statistics query.
type Observer interface {
	Observe(Stats)
}

// Config configures a Poller. Repo and Sender are required; a zero value of
// any other field uses its default.
type Config struct {
	Repo     models.OutboxRepository
	Sender   websocket.BroadcastSender
	Observer Observer
	// Now is the clock of the gap window (tests replace it).
	Now func() time.Time
	// Interval is the time between two polls.
	Interval time.Duration
	// GapWindow is how long an unseen id below the cursor is looked for.
	GapWindow time.Duration
	// BatchSize is the number of rows of one read.
	BatchSize int
	// BackpressureTimeout is the longest wait for the hub queues to drain,
	// once per poll, before events are sent anyway.
	BackpressureTimeout time.Duration
	// MaxMissing bounds the number of gap ids kept in memory.
	MaxMissing int
}

// Poller reads the outbox and sends the events to a hub. One per process.
type Poller struct {
	repo     models.OutboxRepository
	sender   websocket.BroadcastSender
	observer Observer
	now      func() time.Time
	// missing maps an id below the cursor that has not been seen to the time
	// it was first missed.
	missing map[uint64]time.Time
	cancel  context.CancelFunc
	done    chan struct{}
	limiter *logutil.Limiter

	waitLog *logutil.Limiter
	// gaveUp is true after a wait for the hub timed out in the current poll:
	// the rest of the poll sends without waiting, so one stalled socket costs
	// one timeout per poll, not one per event. Guarded by pollMu.
	gaveUp bool

	interval     time.Duration
	gapWindow    time.Duration
	backpressure time.Duration
	cursor       uint64
	batchSize    int
	maxMissing   int

	// pollMu serialises Poll: the state below belongs to one poll at a time.
	pollMu      sync.Mutex
	mu          sync.Mutex // guards cancel and done
	initialized bool
}

// NewPoller returns a poller for cfg, or nil when Repo or Sender is missing;
// the methods of a nil *Poller do nothing.
func NewPoller(cfg Config) *Poller {
	if cfg.Repo == nil || cfg.Sender == nil {
		return nil
	}
	p := &Poller{
		repo:       cfg.Repo,
		sender:     cfg.Sender,
		observer:   cfg.Observer,
		now:        cfg.Now,
		interval:   cfg.Interval,
		gapWindow:  cfg.GapWindow,
		batchSize:  cfg.BatchSize,
		maxMissing: cfg.MaxMissing,
		missing:    map[uint64]time.Time{},
		limiter:    logutil.NewLimiter(logInterval),
		waitLog:    logutil.NewLimiter(logInterval),

		backpressure: cfg.BackpressureTimeout,
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.interval <= 0 {
		p.interval = DefaultInterval
	}
	if p.gapWindow <= 0 {
		p.gapWindow = GapWindow(p.interval)
	}
	if p.batchSize <= 0 {
		p.batchSize = DefaultBatchSize
	}
	if p.maxMissing <= 0 {
		p.maxMissing = DefaultMaxMissing
	}
	if p.backpressure <= 0 {
		p.backpressure = DefaultBackpressureTimeout
	}
	return p
}

// MissingCount returns the number of gap ids that the poller tracks.
func (p *Poller) MissingCount() int {
	if p == nil {
		return 0
	}
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	return len(p.missing)
}

// Start runs the poller in a goroutine: a first poll at once, then one every
// interval. A second Start does nothing; Start after Stop starts again, from
// the table maximum.
func (p *Poller) Start() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	p.pollMu.Lock()
	p.initialized = false
	clear(p.missing)
	p.pollMu.Unlock()

	go p.run(ctx, p.done)
	slog.Info("Outbox poller started", "interval", p.interval, "gap_window", p.gapWindow, "batch_size", p.batchSize)
}

// Stop ends the poller and waits for a poll in progress, at most timeout. A
// second Stop does nothing.
func (p *Poller) Stop(timeout time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		slog.Warn("Outbox poll did not stop in time")
	}
}

func (p *Poller) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	failing := p.tick(ctx, false)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			failing = p.tick(ctx, failing)
		}
	}
}

// tick runs one poll and logs a failure through the limiter: one Error line
// per interval, the rest at Debug. It returns whether the poll failed.
func (p *Poller) tick(ctx context.Context, failing bool) bool {
	pctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	err := p.Poll(pctx)
	if err == nil {
		if failing {
			slog.Info("Outbox poll works again")
		}
		return false
	}
	if ctx.Err() != nil {
		return failing
	}
	if ok, suppressed := p.limiter.Allow(); ok {
		slog.Error("Outbox poll failed; it retries on the next interval", "error", err, "suppressed", suppressed)
	} else {
		slog.Debug("Outbox poll failed", "error", err)
	}
	return true
}

// Poll runs one poll: it delivers the gap rows that appeared, then every row
// after the cursor, in id order and in bounded batches. A failed read returns
// the error with the cursor where it was, so the next poll retries it.
func (p *Poller) Poll(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.pollMu.Lock()
	defer p.pollMu.Unlock()

	if !p.initialized {
		maxID, err := p.repo.MaxID(ctx)
		if err != nil {
			return err
		}
		p.cursor = maxID
		p.initialized = true
	}

	var stats Stats
	var newest uint64
	p.gaveUp = false
	err := p.deliverNew(ctx, &stats, &newest)
	// The stats are reported also when the poll failed: the events that it
	// delivered before the failure count, and the cursor that stopped before
	// the table maximum is the lag.
	p.report(ctx, &stats, newest)
	return err
}

// deliverNew delivers the gap rows that appeared, then every row after the
// cursor, in id order and in bounded batches.
func (p *Poller) deliverNew(ctx context.Context, stats *Stats, newest *uint64) error {
	if err := p.deliverGaps(ctx, stats, newest); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := p.repo.ListAfter(ctx, p.cursor, p.batchSize)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.ID <= p.cursor {
				continue // the repository broke its contract; never go back
			}
			// Send first: when the wait is cancelled the cursor stays before
			// this event, so the next poll sends it.
			if err := p.deliver(ctx, row, stats, newest); err != nil {
				return err
			}
			if row.ID > p.cursor+1 {
				p.trackGap(p.cursor+1, row.ID-1)
			}
			p.cursor = row.ID
		}
		if len(rows) < p.batchSize {
			return nil
		}
	}
}

// deliver sends one event to the hub, after the hub queue has room for it.
func (p *Poller) deliver(ctx context.Context, row models.OutboxEvent, stats *Stats, newest *uint64) error {
	if err := p.waitForHub(ctx); err != nil {
		return err
	}
	p.sender.Broadcast([]byte(row.Message))
	stats.Delivered++
	if row.ID > *newest {
		*newest = row.ID
	}
	return nil
}

// waitForHub waits while the hub backlog is at least half of its capacity, at
// most the backpressure timeout. The hub drops a message that does not fit its
// broadcast queue and disconnects a client whose send queue is full, and the
// cursor moves on after the send, so a catch-up burst would lose events
// silently; waiting lets the hub drain. After the timeout the poll sends the
// rest without waiting (gaveUp): a stalled socket must not stall delivery to
// the others. A sender that does not report its backlog is not waited for.
func (p *Poller) waitForHub(ctx context.Context) error {
	q, ok := p.sender.(backlogReporter)
	if !ok || p.gaveUp {
		return nil
	}
	full := func() bool {
		pending, capacity := q.BroadcastBacklog()
		return capacity > 0 && pending >= capacity/2
	}
	if !full() {
		return nil
	}
	timer := time.NewTimer(p.backpressure)
	defer timer.Stop()
	ticker := time.NewTicker(backpressurePause)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			p.gaveUp = true
			if ok, suppressed := p.waitLog.Allow(); ok {
				slog.Warn("Outbox poller sends while the hub backlog is still high; a slow socket may be dropped", "wait", p.backpressure, "suppressed", suppressed)
			}
			return nil
		case <-ticker.C:
			if !full() {
				return nil
			}
		}
	}
}

// deliverGaps forgets the gaps older than the window and delivers the rows of
// the others that have appeared.
func (p *Poller) deliverGaps(ctx context.Context, stats *Stats, newest *uint64) error {
	now := p.now()
	for id, since := range p.missing {
		if now.Sub(since) >= p.gapWindow {
			delete(p.missing, id)
		}
	}
	if len(p.missing) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(p.missing))
	for id := range p.missing {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for chunk := range slices.Chunk(ids, gapLookupChunk) {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := p.repo.ListByIDs(ctx, chunk)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if _, ok := p.missing[row.ID]; !ok {
				continue
			}
			// Send first: a cancelled wait leaves the id tracked for the next poll.
			if err := p.deliver(ctx, row, stats, newest); err != nil {
				return err
			}
			delete(p.missing, row.ID)
		}
	}
	return nil
}

// trackGap remembers the ids from..to (none of them was seen) for the gap
// window. A jump bigger than the cap keeps only the newest ids, the ones that
// belong to transactions that are still open; the memory never grows past
// maxMissing.
func (p *Poller) trackGap(from, to uint64) {
	// maxMissing is a positive limit (DefaultMaxMissing), so the conversions are safe.
	if to-from+1 > uint64(p.maxMissing) { // #nosec G115 -- positive limit
		slog.Debug("Outbox id gap larger than the tracking limit; the oldest ids are not tracked", "from", from, "to", to, "limit", p.maxMissing)
		from = to - uint64(p.maxMissing) + 1 // #nosec G115 -- positive limit
	}
	n := int(to - from + 1) // #nosec G115 -- at most maxMissing
	if over := len(p.missing) + n - p.maxMissing; over > 0 {
		ids := make([]uint64, 0, len(p.missing))
		for id := range p.missing {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids[:over] {
			delete(p.missing, id)
		}
	}
	now := p.now()
	for id := from; id <= to; id++ {
		p.missing[id] = now
	}
}

// report sends the stats of the poll to the observer: the events delivered,
// the age of the newest one and the id lag (the table maximum minus the
// cursor), measured at the end of every poll. Without an observer it returns
// at once and makes no query. A statistics query that fails (or a context that
// ended) leaves the age or the lag unknown; it never fails the poll.
func (p *Poller) report(ctx context.Context, stats *Stats, newest uint64) {
	if p.observer == nil {
		return
	}
	if ctx.Err() == nil {
		if newest > 0 {
			if age, ok, err := p.repo.Age(ctx, newest); err == nil && ok {
				stats.Age, stats.HasAge = age, true
			}
		}
		if maxID, err := p.repo.MaxID(ctx); err == nil {
			stats.HasLag = true
			if maxID > p.cursor {
				stats.IDLag = maxID - p.cursor
			}
		}
	}
	p.observer.Observe(*stats)
}

package outbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory outbox. visible holds the committed rows; a test
// makes a row visible with commit, in any order, like transactions that
// commit out of ID order.
type fakeStore struct {
	visible  map[uint64]models.OutboxEvent
	maxIDErr error
	// maxIDErrAfterInit makes every MaxID call after the first fail.
	maxIDErrAfterInit error
	listErr           error
	byIDsErr          error
	block             chan struct{}
	listLimits        []int
	// afterList runs after the read number call (1-based) and before it
	// returns, like a transaction that commits while the poller works.
	afterList      func(call int)
	maxID          uint64
	ageOf          time.Duration
	listCalls      int
	byIDsCalls     int
	maxIDCalls     int
	mu             sync.Mutex
	failListNext   int
	failListOnCall int
}

func newFakeStore() *fakeStore { return &fakeStore{visible: map[uint64]models.OutboxEvent{}} }

func (f *fakeStore) commit(ids ...uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitLocked(ids...)
}

func (f *fakeStore) commitLocked(ids ...uint64) {
	for _, id := range ids {
		f.visible[id] = models.OutboxEvent{ID: id, EventType: "item.created", Message: fmt.Sprintf(`{"type":"item.created","payload":{"id":%d}}`, id)}
		if id > f.maxID {
			f.maxID = id
		}
	}
}

func (f *fakeStore) MaxID(ctx context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maxIDCalls++
	if f.maxIDErrAfterInit != nil && f.maxIDCalls > 1 {
		return 0, f.maxIDErrAfterInit
	}
	if f.maxIDErr != nil {
		err := f.maxIDErr
		f.maxIDErr = nil
		return 0, err
	}
	var m uint64
	for id := range f.visible {
		if id > m {
			m = id
		}
	}
	return m, nil
}

func (f *fakeStore) ListAfter(ctx context.Context, after uint64, limit int) ([]models.OutboxEvent, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.listLimits = append(f.listLimits, limit)
	if f.afterList != nil {
		call := f.listCalls
		defer func() { f.afterList(call) }() // runs with f.mu held: it must use commitLocked
	}
	if f.failListOnCall > 0 && f.listCalls == f.failListOnCall {
		return nil, errors.New("database down")
	}
	if f.failListNext > 0 {
		f.failListNext--
		return nil, errors.New("database down")
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	var rows []models.OutboxEvent
	for id, e := range f.visible {
		if id > after {
			rows = append(rows, e)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f *fakeStore) ListByIDs(ctx context.Context, ids []uint64) ([]models.OutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byIDsCalls++
	if f.byIDsErr != nil {
		return nil, f.byIDsErr
	}
	var rows []models.OutboxEvent
	for _, id := range ids {
		if e, ok := f.visible[id]; ok {
			rows = append(rows, e)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func (f *fakeStore) Age(ctx context.Context, id uint64) (time.Duration, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.visible[id]
	return f.ageOf, ok, nil
}

func (f *fakeStore) DeleteOlderThan(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}

// fakeSender records the delivered messages.
type fakeSender struct {
	msgs []string
	mu   sync.Mutex
}

func (s *fakeSender) Broadcast(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, string(b))
}

func (s *fakeSender) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func (s *fakeSender) ids(t *testing.T) []uint64 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, 0, len(s.msgs))
	for _, m := range s.msgs {
		var id uint64
		_, err := fmt.Sscanf(m, `{"type":"item.created","payload":{"id":%d}}`, &id)
		require.NoError(t, err, m)
		out = append(out, id)
	}
	return out
}

type fakeClock struct {
	now time.Time
	mu  sync.Mutex
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestPoller(store *fakeStore, sender *fakeSender, clock *fakeClock, mutate ...func(*Config)) *Poller {
	cfg := Config{
		Repo:      store,
		Sender:    sender,
		Interval:  5 * time.Millisecond,
		GapWindow: 10 * time.Second,
		BatchSize: 100,
		Now:       clock.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return NewPoller(cfg)
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func TestPollerCursorStartsAtTheTableMaximum(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	store.commit(1, 2, 3)
	p := newTestPoller(store, sender, newClock())

	require.NoError(t, p.Poll(context.Background()))
	assert.Empty(t, sender.ids(t), "events from before the start are not replayed")

	store.commit(4)
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, []uint64{4}, sender.ids(t))
}

func TestPollerStartsAtZeroOnAnEmptyTable(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock())
	require.NoError(t, p.Poll(context.Background()))

	store.commit(1, 2)
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, []uint64{1, 2}, sender.ids(t))
}

func TestPollerDeliversInOrderInBoundedBatches(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock(), func(c *Config) { c.BatchSize = 2 })
	require.NoError(t, p.Poll(context.Background()))
	store.mu.Lock()
	store.listLimits = nil
	store.mu.Unlock()

	store.commit(1, 2, 3, 4, 5)
	require.NoError(t, p.Poll(context.Background()))

	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, sender.ids(t))
	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, []int{2, 2, 2}, store.listLimits, "every read is bounded by the batch size and the drain ends on a short batch (2, 2, then 1 row)")
}

func TestPollerDeliversEachEventOnce(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock())
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1, 2)
	for i := 0; i < 3; i++ {
		require.NoError(t, p.Poll(context.Background()))
	}
	assert.Equal(t, []uint64{1, 2}, sender.ids(t))
}

// Two transactions take the IDs 1 and 2; 2 commits first. The poller sees 2,
// then 1 appears. It must deliver both.
func TestPollerDeliversAnEventThatCommitsOutOfOrder(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	clock := newClock()
	p := newTestPoller(store, sender, clock)
	require.NoError(t, p.Poll(context.Background()))

	store.commit(2) // the transaction with ID 2 commits first
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, []uint64{2}, sender.ids(t))

	clock.advance(2 * time.Second)
	store.commit(1) // the older ID becomes visible later
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, []uint64{2, 1}, sender.ids(t), "the late row is delivered, once")

	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, []uint64{2, 1}, sender.ids(t))
}

func TestPollerGapWindow(t *testing.T) {
	t.Parallel()

	t.Run("a row that appears after the window is skipped", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		clock := newClock()
		p := newTestPoller(store, sender, clock)
		require.NoError(t, p.Poll(context.Background()))
		store.commit(2)
		require.NoError(t, p.Poll(context.Background()))

		clock.advance(10 * time.Second)
		require.NoError(t, p.Poll(context.Background()))
		assert.Zero(t, p.MissingCount(), "the gap is forgotten after the window")

		store.commit(1)
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, []uint64{2}, sender.ids(t), "the documented limit: a transaction longer than the window is skipped")
	})

	t.Run("a row that never appears stops costing a query", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		clock := newClock()
		p := newTestPoller(store, sender, clock)
		require.NoError(t, p.Poll(context.Background()))
		store.commit(3) // 1 and 2 were rolled back
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, 2, p.MissingCount())

		clock.advance(11 * time.Second)
		require.NoError(t, p.Poll(context.Background()))
		calls := store.byIDsCalls
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, calls, store.byIDsCalls, "no lookup once the gap expired")
	})

	t.Run("memory is bounded and the newest ids are kept", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		p := newTestPoller(store, sender, newClock(), func(c *Config) { c.MaxMissing = 5 })
		require.NoError(t, p.Poll(context.Background()))

		store.commit(1_000_000) // a jump of a million ids
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, 5, p.MissingCount())

		store.commit(999_999)
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, []uint64{1_000_000, 999_999}, sender.ids(t), "the newest gap ids are the ones that are tracked")

		// A second jump evicts the oldest tracked ids, never grows past the cap.
		store.commit(2_000_000)
		require.NoError(t, p.Poll(context.Background()))
		assert.LessOrEqual(t, p.MissingCount(), 5)
	})

	t.Run("a lookup error is retried on the next poll", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		p := newTestPoller(store, sender, newClock())
		require.NoError(t, p.Poll(context.Background()))
		store.commit(2)
		require.NoError(t, p.Poll(context.Background()))

		store.byIDsErr = errors.New("database down")
		store.commit(1)
		require.Error(t, p.Poll(context.Background()))

		store.byIDsErr = nil
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, []uint64{2, 1}, sender.ids(t))
	})
}

func TestPollerRetriesAfterAFailedPoll(t *testing.T) {
	t.Parallel()

	t.Run("a failed read", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		p := newTestPoller(store, sender, newClock())
		require.NoError(t, p.Poll(context.Background()))
		store.commit(1)
		store.failListNext = 1

		require.Error(t, p.Poll(context.Background()))
		assert.Empty(t, sender.ids(t))
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, []uint64{1}, sender.ids(t), "the cursor did not move on the failure")
	})

	t.Run("a failed start", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		store.commit(1)
		store.maxIDErr = errors.New("database down")
		p := newTestPoller(store, sender, newClock())

		require.Error(t, p.Poll(context.Background()))
		require.NoError(t, p.Poll(context.Background()))
		assert.Empty(t, sender.ids(t), "the cursor is the maximum once the read works")
		store.commit(2)
		require.NoError(t, p.Poll(context.Background()))
		assert.Equal(t, []uint64{2}, sender.ids(t))
	})
}

func TestPollerStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock(), func(c *Config) { c.BatchSize = 1 })
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1, 2, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Poll(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// startAndWait starts the poller and waits until it has made a read after the
// start, so its cursor is set and an event committed afterwards is a new one.
func startAndWait(t *testing.T, p *Poller, store *fakeStore) {
	t.Helper()
	store.mu.Lock()
	base := store.listCalls
	store.mu.Unlock()
	p.Start()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.listCalls > base
	}, 2*time.Second, time.Millisecond, "the poller did not start")
}

func TestPollerRunDeliversAndStops(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock())

	startAndWait(t, p, store)
	p.Start() // a second Start does nothing
	store.commit(1)
	require.Eventually(t, func() bool { return len(sender.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)

	start := time.Now()
	p.Stop(time.Second)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
	p.Stop(time.Second) // a second Stop does nothing

	store.commit(2)
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, []uint64{1}, sender.ids(t), "no delivery after Stop")
}

func TestPollerRunRecoversFromErrors(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock())
	require.NoError(t, p.Poll(context.Background()))
	store.failListNext = 3

	defer p.Stop(time.Second)
	startAndWait(t, p, store)
	store.commit(1)
	require.Eventually(t, func() bool { return len(sender.snapshot()) == 1 },
		2*time.Second, 5*time.Millisecond, "the poller goes on after failed polls")
}

func TestPollerStopIsBounded(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	store.block = make(chan struct{})
	t.Cleanup(func() { close(store.block) })
	p := newTestPoller(store, sender, newClock())
	p.Start()                         // the first poll blocks in ListAfter
	time.Sleep(20 * time.Millisecond) // let the run goroutine block in ListAfter
	start := time.Now()
	p.Stop(150 * time.Millisecond) // the read in progress ignores the context
	assert.Less(t, time.Since(start), time.Second)
}

func TestPollerRestartsFromTheTableMaximum(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock())
	startAndWait(t, p, store)
	store.commit(1)
	require.Eventually(t, func() bool { return len(sender.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)
	p.Stop(time.Second)

	store.commit(2, 3)
	p.Start()
	defer p.Stop(time.Second)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, []uint64{1}, sender.ids(t), "a restart starts at the maximum again and does not replay")
}

type recordingObserver struct {
	stats []Stats
	mu    sync.Mutex
}

func (o *recordingObserver) last(t *testing.T) Stats {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	require.NotEmpty(t, o.stats)
	return o.stats[len(o.stats)-1]
}

func (o *recordingObserver) Observe(s Stats) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stats = append(o.stats, s)
}

func TestPollerReportsStatsToTheObserver(t *testing.T) {
	t.Parallel()

	t.Run("the cursor reached the maximum", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		store.ageOf = 1500 * time.Millisecond
		obs := &recordingObserver{}
		p := newTestPoller(store, sender, newClock(), func(c *Config) { c.Observer = obs; c.BatchSize = 2 })
		require.NoError(t, p.Poll(context.Background()))

		store.commit(1, 2, 3)
		require.NoError(t, p.Poll(context.Background()))

		last := obs.last(t)
		assert.Equal(t, 3, last.Delivered)
		assert.True(t, last.HasAge)
		assert.Equal(t, 1500*time.Millisecond, last.Age)
		assert.True(t, last.HasLag, "the lag was measured")
		assert.Zero(t, last.IDLag)
	})

	// The lag is the table maximum minus the cursor, measured at the end of
	// every poll, not a guess from the size of the last batch: a row that is
	// committed while the poll runs is lag.
	t.Run("an event committed during the poll is lag", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		obs := &recordingObserver{}
		p := newTestPoller(store, sender, newClock(), func(c *Config) { c.Observer = obs })
		require.NoError(t, p.Poll(context.Background()))
		store.commit(1, 2, 3)
		store.mu.Lock()
		store.afterList = func(int) { store.commitLocked(4, 5) } // after the drain read
		store.mu.Unlock()

		require.NoError(t, p.Poll(context.Background()))

		last := obs.last(t)
		assert.Equal(t, 3, last.Delivered)
		assert.True(t, last.HasLag)
		assert.Equal(t, uint64(2), last.IDLag, "ids 4 and 5 are in the table, the cursor is at 3")
	})

	// A poll that fails after a full batch leaves the cursor before the
	// maximum: that is the lag an operator needs to see.
	t.Run("a failed read leaves the lag and the delivered count", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		obs := &recordingObserver{}
		p := newTestPoller(store, sender, newClock(), func(c *Config) { c.Observer = obs; c.BatchSize = 2 })
		require.NoError(t, p.Poll(context.Background()))
		store.commit(1, 2, 3, 4, 5, 6)
		store.mu.Lock()
		store.failListOnCall = store.listCalls + 2 // the second read of the next poll
		store.mu.Unlock()

		require.Error(t, p.Poll(context.Background()))

		last := obs.last(t)
		assert.Equal(t, 2, last.Delivered, "the first batch was delivered")
		assert.True(t, last.HasLag)
		assert.Equal(t, uint64(4), last.IDLag, "the table maximum is 6, the cursor is at 2")
		assert.Equal(t, []uint64{1, 2}, sender.ids(t))
	})

	t.Run("a failed lag query keeps the last lag", func(t *testing.T) {
		t.Parallel()
		store, sender := newFakeStore(), &fakeSender{}
		obs := &recordingObserver{}
		p := newTestPoller(store, sender, newClock(), func(c *Config) { c.Observer = obs })
		require.NoError(t, p.Poll(context.Background()))
		store.commit(1)
		store.mu.Lock()
		store.maxIDErrAfterInit = errors.New("database down")
		store.mu.Unlock()

		require.NoError(t, p.Poll(context.Background()), "a failed statistics query never fails the poll")

		last := obs.last(t)
		assert.Equal(t, 1, last.Delivered)
		assert.False(t, last.HasLag, "unknown, so the gauge keeps its value")
	})
}

func TestPollerWithoutObserverMakesNoStatsQueries(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock())
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1)
	require.NoError(t, p.Poll(context.Background()))
	// Without an observer a poll makes its reads only: no Age, no lag query
	// (MaxID ran once, at the start).
	assert.Equal(t, 2, store.listCalls, "one read per poll")
	assert.Equal(t, 1, store.maxIDCalls, "no lag query without an observer")
}

// queueSender is a hub whose broadcast queue the test controls.
type queueSender struct {
	fakeSender
	pending  atomic.Int64
	capacity int
}

func (q *queueSender) BroadcastBacklog() (int, int) { return int(q.pending.Load()), q.capacity }

func TestDefaultBatchIsWellBelowTheHubQueue(t *testing.T) {
	t.Parallel()
	// The hub drops a broadcast when its 256 slot queue is full, so a
	// catch-up batch must fit in half of it.
	assert.LessOrEqual(t, DefaultBatchSize, 128)
	p := newTestPoller(newFakeStore(), &fakeSender{}, newClock(), func(c *Config) { c.BatchSize = 0 })
	assert.Equal(t, DefaultBatchSize, p.batchSize)
}

// The poller does not send while the hub queue is at least half full: the hub
// drops what does not fit, and the cursor has moved on by then.
func TestPollerWaitsForTheHubQueueToDrain(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &queueSender{capacity: 256}
	p := NewPoller(Config{Repo: store, Sender: sender, Interval: time.Hour, BatchSize: 100, Now: newClock().Now})
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1, 2, 3)
	sender.pending.Store(200) // over the high water mark of 128

	done := make(chan error, 1)
	go func() { done <- p.Poll(context.Background()) }()

	time.Sleep(60 * time.Millisecond)
	assert.Empty(t, sender.snapshot(), "nothing is sent while the queue is full")
	select {
	case err := <-done:
		t.Fatalf("the poll ended while the queue was full: %v", err)
	default:
	}

	sender.pending.Store(10) // the hub drained it
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the poll did not go on after the queue drained")
	}
	assert.Equal(t, []uint64{1, 2, 3}, sender.ids(t))
}

// A hub that never drains must not stall the poller for ever: after the
// backpressure timeout it sends anyway (the hub then drops, as it always did).
func TestPollerSendsAnywayAfterTheBackpressureTimeout(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &queueSender{capacity: 256}
	p := NewPoller(Config{Repo: store, Sender: sender, Interval: time.Hour, BackpressureTimeout: 30 * time.Millisecond, Now: newClock().Now})
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1)
	sender.pending.Store(256)

	start := time.Now()
	require.NoError(t, p.Poll(context.Background()))

	assert.Equal(t, []uint64{1}, sender.ids(t))
	assert.Less(t, time.Since(start), 2*time.Second)
}

// One stalled socket costs one timeout per poll, not one per event.
func TestPollerGivesUpWaitingOncePerPoll(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &queueSender{capacity: 256}
	const timeout = 100 * time.Millisecond
	p := NewPoller(Config{Repo: store, Sender: sender, Interval: time.Hour, BackpressureTimeout: timeout, Now: newClock().Now})
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1, 2, 3, 4, 5)
	sender.pending.Store(256) // never drains

	start := time.Now()
	require.NoError(t, p.Poll(context.Background()))
	elapsed := time.Since(start)

	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, sender.ids(t))
	assert.GreaterOrEqual(t, elapsed, timeout)
	assert.Less(t, elapsed, 4*timeout, "five events waited once, not five times")

	// The next poll waits again (the flag is per poll).
	store.commit(6)
	start = time.Now()
	require.NoError(t, p.Poll(context.Background()))
	assert.GreaterOrEqual(t, time.Since(start), timeout)
}

// A poll that is cancelled while it waits returns the error with the cursor
// before the event it did not send, so the next poll sends it.
func TestPollerCancelDuringBackpressureKeepsTheCursor(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &queueSender{capacity: 256}
	p := NewPoller(Config{Repo: store, Sender: sender, Interval: time.Hour, Now: newClock().Now})
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1, 2)
	sender.pending.Store(256)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := p.Poll(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, sender.snapshot())

	sender.pending.Store(0)
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, []uint64{1, 2}, sender.ids(t), "nothing was skipped")
}

// A sender without a queue (the BroadcastSender interface alone) is not
// waited for.
func TestPollerDoesNotWaitForASenderWithoutAQueue(t *testing.T) {
	t.Parallel()
	store, sender := newFakeStore(), &fakeSender{}
	p := newTestPoller(store, sender, newClock())
	require.NoError(t, p.Poll(context.Background()))
	store.commit(1, 2, 3)
	start := time.Now()
	require.NoError(t, p.Poll(context.Background()))
	assert.Less(t, time.Since(start), 500*time.Millisecond)
	assert.Equal(t, []uint64{1, 2, 3}, sender.ids(t))
}

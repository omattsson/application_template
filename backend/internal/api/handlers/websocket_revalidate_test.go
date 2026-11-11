package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCtx is the context of one revalidation run, ended by the test (end), not
// by a clock: end(context.DeadlineExceeded) is the run reaching its
// RevalidateTimeout, end(context.Canceled) the hub shutting down.
type runCtx struct {
	context.Context // Background: no deadline, no values
	done            chan struct{}
	once            sync.Once
	mu              sync.Mutex
	err             error
}

func newRunCtx() *runCtx {
	return &runCtx{Context: context.Background(), done: make(chan struct{})}
}

func (c *runCtx) Done() <-chan struct{} { return c.done }

func (c *runCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *runCtx) end(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

// countingStore counts the blocklist lookups. With a budget >= 0, the lookup
// after the first budget ones of a run ends the run (the run reached its
// deadline: the onOver of startRun) and returns the context error, like a
// query cut by the run deadline. failErr makes every lookup fail at once (a
// failing store); hangFirst makes the first lookup hang until its own
// context ends (a hung connection).
type countingStore struct {
	sessionstore.SessionStore
	mu        sync.Mutex
	calls     int
	runCalls  int
	budget    int // < 0: no budget
	failErr   error
	hangFirst bool
	onOver    func()
	tokens    []string // jti of every IsTokenBlocked call, in order
	userBlk   int      // IsUserBlocked calls
}

func newCountingStore(t *testing.T) *countingStore {
	t.Helper()
	inner := sessionstore.NewMemory()
	t.Cleanup(inner.Stop)
	return &countingStore{SessionStore: inner, budget: -1}
}

// startRun resets the per-run budget; onOver runs when a lookup exceeds it.
func (s *countingStore) startRun(onOver func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runCalls = 0
	s.onOver = onOver
}

func (s *countingStore) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *countingStore) userBlockCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userBlk
}

func (s *countingStore) seenTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens...)
}

// enter counts a lookup and returns the error it must give (nil: go on).
// A lookup that goes on answers at once, whatever its context: a healthy
// store does not time out, so a short lookup timeout in a test only cuts the
// hung lookups.
func (s *countingStore) enter(ctx context.Context, jti string) error {
	s.mu.Lock()
	s.calls++
	s.runCalls++
	if jti != "" {
		s.tokens = append(s.tokens, jti)
	}
	over := s.budget >= 0 && s.runCalls > s.budget
	hang := s.hangFirst && s.calls == 1
	failErr, onOver := s.failErr, s.onOver
	s.mu.Unlock()
	if failErr != nil {
		return failErr
	}
	if over && onOver != nil {
		onOver()
	}
	if over || hang {
		<-ctx.Done()
		return fmt.Errorf("store query: %w", ctx.Err())
	}
	return nil
}

func (s *countingStore) IsTokenBlocked(ctx context.Context, jti string) (bool, error) {
	if err := s.enter(ctx, jti); err != nil {
		return false, err
	}
	return s.SessionStore.IsTokenBlocked(context.WithoutCancel(ctx), jti)
}

func (s *countingStore) IsSessionBlocked(ctx context.Context, id string) (bool, error) {
	if err := s.enter(ctx, ""); err != nil {
		return false, err
	}
	return s.SessionStore.IsSessionBlocked(context.WithoutCancel(ctx), id)
}

func (s *countingStore) IsUserBlocked(ctx context.Context, id string, at time.Time) (bool, error) {
	s.mu.Lock()
	s.userBlk++
	s.mu.Unlock()
	if err := s.enter(ctx, ""); err != nil {
		return false, err
	}
	return s.SessionStore.IsUserBlocked(context.WithoutCancel(ctx), id, at)
}

// runOnce runs check as one revalidation run whose deadline is the budget of
// store.
func runOnce(check websocket.IdentityChecker, store *countingStore, ids []websocket.ClientIdentity) []bool {
	rc := newRunCtx()
	store.startRun(func() { rc.end(context.DeadlineExceeded) })
	return check(rc, ids)
}

// fakeUsers is an in-memory user repository that counts its lookups.
type fakeUsers struct {
	models.UserRepository
	mu       sync.Mutex
	users    map[string]*models.User
	byIDs    int
	byID     int
	asked    int // user IDs asked for, over all batches
	batchErr error
	hang     bool // the batch hangs until its context ends
}

func newFakeUsers(n int) *fakeUsers {
	f := &fakeUsers{users: map[string]*models.User{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("user-%d", i)
		f.users[id] = &models.User{ID: id, Role: models.RoleUser}
	}
	return f
}

func (f *fakeUsers) FindByIDs(ctx context.Context, ids []string) (map[string]*models.User, error) {
	f.mu.Lock()
	f.byIDs++
	f.asked += len(ids)
	hang, batchErr := f.hang, f.batchErr
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, fmt.Errorf("users query: %w", ctx.Err())
	}
	if batchErr != nil {
		return nil, batchErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*models.User{}
	for _, id := range ids {
		if u, ok := f.users[id]; ok {
			cp := *u
			out[id] = &cp
		}
	}
	return out, nil
}

func (f *fakeUsers) FindByID(context.Context, string) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID++
	return nil, errors.New("the revalidation must use FindByIDs")
}

func (f *fakeUsers) counts() (byIDs, byID int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byIDs, f.byID
}

func (f *fakeUsers) askedIDs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// fakeKeys is an in-memory API key repository that counts the key IDs its
// batches are asked for.
type fakeKeys struct {
	models.APIKeyRepository
	mu      sync.Mutex
	keys    map[string]*models.APIKey
	batches int
	asked   int
}

func (f *fakeKeys) FindByIDs(_ context.Context, ids []string) ([]*models.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	f.asked += len(ids)
	var out []*models.APIKey
	for _, id := range ids {
		if k, ok := f.keys[id]; ok {
			cp := *k
			out = append(out, &cp)
		}
	}
	return out, nil
}

// jwtIdentities returns n JWT identities, each with its own user, token and
// session, numbered from first.
func jwtIdentitiesFrom(first, n int) []websocket.ClientIdentity {
	ids := make([]websocket.ClientIdentity, n)
	past := time.Now().Add(-time.Minute)
	for i := range ids {
		k := first + i
		ids[i] = websocket.ClientIdentity{
			UserID: fmt.Sprintf("user-%d", k), TokenID: fmt.Sprintf("jti-%d", k),
			SessionID: fmt.Sprintf("sid-%d", k), IssuedAt: past,
		}
	}
	return ids
}

func jwtIdentities(n int) []websocket.ClientIdentity { return jwtIdentitiesFrom(0, n) }

// orderOnly is a fail-closed checker without the unverified-runs limit, to
// test the order of the runs alone.
func orderOnly(store sessionstore.SessionStore, users models.UserRepository) websocket.IdentityChecker {
	return newRevocationChecker(store, users, nil, checkerConfig{failClosed: true, lookupTimeout: defaultLookupTimeout}).check
}

func TestRevocationCheckerLookupTimeoutIsBelowTheRunBudget(t *testing.T) {
	t.Parallel()
	assert.Less(t, defaultLookupTimeout, websocket.RevalidateTimeout)
	assert.Positive(t, defaultMaxUnverifiedRuns)
}

// TestRevocationCheckerDeadlineMidRunRevokesNothing: a run that runs out of
// its own time is not a store failure: with fail closed on, nothing that was
// not found revoked is closed.
func TestRevocationCheckerDeadlineMidRunRevokesNothing(t *testing.T) {
	t.Parallel()
	store := newCountingStore(t)
	store.budget = 4 // the run reaches its deadline at the 5th lookup
	ids := jwtIdentities(10)
	require.NoError(t, store.BlockToken(context.Background(), "jti-0", time.Now().Add(time.Hour)))

	got := runOnce(NewWebSocketRevocationChecker(store, newFakeUsers(10), nil, true), store, ids)

	want := make([]bool, len(ids))
	want[0] = true // checked before the deadline: still revoked
	assert.Equal(t, want, got, "the identities the run did not reach stay open")
}

// TestRevocationCheckerResumesWhereTheLastRunStopped: identities that a slow
// run did not reach are checked first in the next run, so with a run that is
// always too slow every identity is still checked, in turn. The re-check of
// a new socket between runs does not change that order.
func TestRevocationCheckerResumesWhereTheLastRunStopped(t *testing.T) {
	t.Parallel()
	store := newCountingStore(t)
	store.budget = 5 // a little more than one identity per run
	const n = 12
	ids := jwtIdentities(n)
	require.NoError(t, store.BlockToken(context.Background(), "jti-11", time.Now().Add(time.Hour)))

	check := orderOnly(store, newFakeUsers(n))
	revokedSeen := false
	var firstOfRun []string
	for run := 0; run < 3*n && len(distinct(store.seenTokens())) < n; run++ {
		before := len(store.seenTokens())
		got := runOnce(check, store, ids) // the same order every run: the worst case
		if seen := store.seenTokens(); len(seen) > before {
			firstOfRun = append(firstOfRun, seen[before])
		}
		for i, r := range got {
			if r {
				require.Equal(t, n-1, i, "only the blocked token is revoked")
				revokedSeen = true
			}
		}

		rc := newRunCtx()
		store.startRun(func() { rc.end(context.DeadlineExceeded) })
		check(withRegistrationRecheck(rc), ids[:1])
	}

	assert.Len(t, distinct(store.seenTokens()), n, "every identity was checked")
	assert.True(t, revokedSeen, "the blocked token at the end of the list was found")
	require.GreaterOrEqual(t, len(firstOfRun), 2)
	assert.NotEqual(t, firstOfRun[0], firstOfRun[1], "the second run starts with an identity the first did not reach")
}

func distinct(s []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return m
}

// TestRevocationCheckerNewIdentitiesDoNotStarveOldOnes: under a store that
// lets a run check 2 identities, new identities arrive before the old ones
// in every run; the revoked old identity at the end is still found within
// ceil(6/2) = 3 runs (new identities count as checked when first seen).
func TestRevocationCheckerNewIdentitiesDoNotStarveOldOnes(t *testing.T) {
	t.Parallel()
	const old, perRun = 6, 2
	store := newCountingStore(t)
	store.budget = 3 * perRun // 3 lookups per identity: token, session, user block
	require.NoError(t, store.BlockToken(context.Background(), "jti-5", time.Now().Add(time.Hour)))

	check := orderOnly(store, newFakeUsers(1000))
	ids := jwtIdentities(old)
	found := 0
	for run := 1; run <= 10 && found == 0; run++ {
		if run > 1 { // two new sockets, first in the hub's order
			ids = append(jwtIdentitiesFrom(100*run, perRun), ids...)
		}
		got := runOnce(check, store, ids)
		for i, r := range got {
			if r {
				require.Equal(t, "jti-5", ids[i].TokenID, "only the blocked token is revoked")
				found = run
			}
		}
	}
	require.NotZero(t, found, "the revoked old identity was never reached")
	assert.LessOrEqual(t, found, (old+perRun-1)/perRun)
}

// TestRevocationCheckerClosesUnverifiedAfterMaxRuns: a store that hangs on
// every lookup until the run deadline, with a lookup timeout too long to
// help: no run verifies anything, and every identity closes in the M-th run
// since it was first seen, none before. A cancelled run (shutdown) does not
// count; fail open never closes.
func TestRevocationCheckerClosesUnverifiedAfterMaxRuns(t *testing.T) {
	t.Parallel()
	const m = defaultMaxUnverifiedRuns

	t.Run("fail closed", func(t *testing.T) {
		t.Parallel()
		store := newCountingStore(t)
		store.budget = 0
		check := newRevocationChecker(store, newFakeUsers(1000), nil,
			checkerConfig{failClosed: true, lookupTimeout: time.Hour, maxUnverifiedRuns: m}).check

		firstSeen := map[string]int{}
		ids := jwtIdentities(3)
		for run := 1; run <= 2*m; run++ {
			if run == 2 { // a shutdown between two runs
				rc := newRunCtx()
				rc.end(context.Canceled)
				assert.Equal(t, make([]bool, len(ids)), check(rc, ids), "a cancelled run closes nothing")
			}
			ids = append(ids, jwtIdentitiesFrom(100*run, 1)...) // a new socket every run
			for _, id := range ids {
				if _, ok := firstSeen[id.TokenID]; !ok {
					firstSeen[id.TokenID] = run
				}
			}
			ids = assertClosedInRunM(t, run, m, firstSeen, ids, runOnce(check, store, ids))
		}
	})

	t.Run("fail open", func(t *testing.T) {
		t.Parallel()
		store := newCountingStore(t)
		store.budget = 0
		check := newRevocationChecker(store, newFakeUsers(3), nil,
			checkerConfig{lookupTimeout: time.Hour, maxUnverifiedRuns: m}).check
		for run := 1; run <= 2*m; run++ {
			assert.Equal(t, make([]bool, 3), runOnce(check, store, jwtIdentities(3)))
		}
	})
}

// assertClosedInRunM checks that exactly the identities first seen M or more
// runs ago are closed, and returns the open ones (the hub closes the rest).
func assertClosedInRunM(t *testing.T, run, m int, firstSeen map[string]int, ids []websocket.ClientIdentity, got []bool) []websocket.ClientIdentity {
	t.Helper()
	var open []websocket.ClientIdentity
	for i, r := range got {
		first := firstSeen[ids[i].TokenID]
		assert.Equal(t, run-first+1 >= m, r, "run %d, %s first seen in run %d", run, ids[i].TokenID, first)
		if !r {
			open = append(open, ids[i])
		}
	}
	return open
}

// TestRevocationCheckerHungLookupFailsClosed: a lookup that hangs until its
// context ends fails on its own timeout while the run still has time: that is
// a store failure, so it fails closed in the same run.
func TestRevocationCheckerHungLookupFailsClosed(t *testing.T) {
	t.Parallel()
	cfg := checkerConfig{failClosed: true, lookupTimeout: time.Millisecond, maxUnverifiedRuns: defaultMaxUnverifiedRuns}

	t.Run("blocklist", func(t *testing.T) {
		t.Parallel()
		store := newCountingStore(t)
		store.hangFirst = true
		check := newRevocationChecker(store, newFakeUsers(3), nil, cfg).check
		assert.Equal(t, []bool{true, true, true}, check(context.Background(), jwtIdentities(3)))
		assert.Equal(t, 1, store.total(), "the store is not queried again in the run")
	})

	t.Run("user batch", func(t *testing.T) {
		t.Parallel()
		users := newFakeUsers(2)
		users.hang = true
		ids := append(jwtIdentities(2), websocket.ClientIdentity{TokenID: "no-user"})
		check := newRevocationChecker(newCountingStore(t), users, nil, cfg).check
		assert.Equal(t, []bool{true, true, false}, check(context.Background(), ids))
	})

	t.Run("fail open lets them pass", func(t *testing.T) {
		t.Parallel()
		store := newCountingStore(t)
		store.hangFirst = true
		open := cfg
		open.failClosed = false
		check := newRevocationChecker(store, newFakeUsers(3), nil, open).check
		assert.Equal(t, []bool{false, false, false}, check(context.Background(), jwtIdentities(3)))
	})
}

// TestRevocationCheckerFailedLookupIsNotVerified: an identity that needed a
// lookup that failed was not verified, in both modes, even when its other
// checks passed.
func TestRevocationCheckerFailedLookupIsNotVerified(t *testing.T) {
	t.Parallel()
	for _, failClosed := range []bool{true, false} {
		failClosed := failClosed
		t.Run(fmt.Sprintf("user batch, fail closed %v", failClosed), func(t *testing.T) {
			t.Parallel()
			users := newFakeUsers(2)
			users.batchErr = errors.New("dial tcp: database down")
			r := newRevocationChecker(newCountingStore(t), users, nil, checkerConfig{failClosed: failClosed})
			ids := append(jwtIdentities(1), websocket.ClientIdentity{TokenID: "no-user"})
			cr := newCheckRun(r, context.Background(), ids)
			cr.process([]int{0, 1})
			assert.Equal(t, []bool{false, true}, cr.verified, "fail closed %v", failClosed)
			assert.Equal(t, []bool{failClosed, false}, cr.revoked, "fail closed %v", failClosed)
		})
		t.Run(fmt.Sprintf("store, fail closed %v", failClosed), func(t *testing.T) {
			t.Parallel()
			store := newCountingStore(t)
			store.failErr = errors.New("dial tcp: store down")
			r := newRevocationChecker(store, newFakeUsers(2), nil, checkerConfig{failClosed: failClosed})
			cr := newCheckRun(r, context.Background(), jwtIdentities(2))
			cr.process([]int{0, 1})
			assert.Equal(t, []bool{false, false}, cr.verified, "fail closed %v", failClosed)
			assert.Equal(t, []bool{failClosed, failClosed}, cr.revoked, "fail closed %v", failClosed)
		})
	}
}

// TestRevocationCheckerGenuineErrors: a lookup that fails while the run still
// has time is a store failure: fail closed revokes the identities that need
// the failed lookup, fail open lets them pass.
func TestRevocationCheckerGenuineErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("store error", func(t *testing.T) {
		t.Parallel()
		for _, failClosed := range []bool{true, false} {
			store := newCountingStore(t)
			store.failErr = errors.New("dial tcp: store down")
			got := NewWebSocketRevocationChecker(store, newFakeUsers(3), nil, failClosed)(ctx, jwtIdentities(3))
			assert.Equal(t, []bool{failClosed, failClosed, failClosed}, got)
			assert.Equal(t, 1, store.total(), "the run stops at the first store failure")
		}
	})

	t.Run("a query timeout of its own is a failure, not the run deadline", func(t *testing.T) {
		t.Parallel()
		store := newCountingStore(t)
		store.failErr = fmt.Errorf("query: %w", context.DeadlineExceeded)
		got := NewWebSocketRevocationChecker(store, newFakeUsers(2), nil, true)(ctx, jwtIdentities(2))
		assert.Equal(t, []bool{true, true}, got)
	})

	t.Run("user batch error", func(t *testing.T) {
		t.Parallel()
		users := newFakeUsers(2)
		users.batchErr = errors.New("dial tcp: database down")
		ids := append(jwtIdentities(2), websocket.ClientIdentity{TokenID: "no-user"})
		got := NewWebSocketRevocationChecker(newCountingStore(t), users, nil, true)(ctx, ids)
		assert.Equal(t, []bool{true, true, false}, got, "only the identities with a user need the user lookup")
	})

	t.Run("api key batch error closes only the key sockets", func(t *testing.T) {
		t.Parallel()
		env := newCheckerEnv(t)
		keys := failingAPIKeyRepo{APIKeyRepository: env.keys.keys, err: errors.New("dial tcp"), ops: map[string]bool{"find": true}}
		store := newCountingStore(t)
		require.NoError(t, store.BlockToken(ctx, "jti-blocked", time.Now().Add(time.Hour)))
		past := time.Now().Add(-time.Minute)
		ids := []websocket.ClientIdentity{
			{UserID: "u-alice", TokenID: "jti-ok", IssuedAt: past},
			{UserID: "u-alice", APIKeyID: env.key.ID, IssuedAt: time.Now()},
			{UserID: "u-alice", TokenID: "jti-blocked", IssuedAt: past},
		}
		got := NewWebSocketRevocationChecker(store, env.keys.users, keys, true)(ctx, ids)
		assert.Equal(t, []bool{false, true, true}, got, "the JWT sockets are still checked normally")
	})
}

// TestRevocationCheckerUserBlockByIssueTime: the user block covers the
// sockets issued at or before it, and not the later ones.
func TestRevocationCheckerUserBlockByIssueTime(t *testing.T) {
	t.Parallel()
	store := newCountingStore(t)
	require.NoError(t, store.BlockUser(context.Background(), "user-0", time.Now().Add(time.Hour)))
	time.Sleep(2 * time.Millisecond)
	after := time.Now()
	before := after.Add(-time.Hour)

	ids := []websocket.ClientIdentity{
		{UserID: "user-0", IssuedAt: before},
		{UserID: "user-0", IssuedAt: after},
		{UserID: "user-0", IssuedAt: before.Add(time.Minute)},
		{UserID: "user-0"}, // no issue time: blocked like the HTTP auth
		{UserID: "user-1", IssuedAt: before},
	}
	got := NewWebSocketRevocationChecker(store, newFakeUsers(2), nil, true)(context.Background(), ids)
	assert.Equal(t, []bool{true, false, true, true, false}, got)
}

// TestRevocationCheckerLargeRunCost: the cost of a run follows the distinct
// credentials, not sockets x 4: one user batch per chunk, one blocklist
// lookup per distinct token and session, and one per user without a block.
// A healthy replica of that size closes nothing, run after run.
func TestRevocationCheckerLargeRunCost(t *testing.T) {
	t.Parallel()
	const (
		sockets  = 5000
		users    = 50
		sessions = 250
		tokens   = 500
	)
	store := newCountingStore(t)
	repo := newFakeUsers(users)
	past := time.Now().Add(-time.Minute)
	ids := make([]websocket.ClientIdentity, sockets)
	for i := range ids {
		tok := i % tokens
		sess := tok % sessions
		ids[i] = websocket.ClientIdentity{
			UserID: fmt.Sprintf("user-%d", sess%users), TokenID: fmt.Sprintf("jti-%d", tok),
			SessionID: fmt.Sprintf("sid-%d", sess), IssuedAt: past.Add(time.Duration(tok) * time.Millisecond),
		}
	}

	check := NewWebSocketRevocationChecker(store, repo, nil, true)
	got := check(context.Background(), ids)
	assert.Equal(t, make([]bool, sockets), got)

	byIDs, byID := repo.counts()
	assert.Zero(t, byID, "no lookup per user")
	assert.LessOrEqual(t, byIDs, sockets/revalidateChunkSize+1, "one user batch per chunk at most")
	assert.Equal(t, users, repo.askedIDs(), "each user is read once per run")
	assert.Equal(t, users, store.userBlockCalls(), "no user block: one user-block lookup per user, whatever its issue times")
	assert.Equal(t, tokens+sessions+users, store.total(), "one lookup per distinct credential")
	t.Logf("store lookups: %d for %d sockets (sockets x 3 = %d)", store.total(), sockets, 3*sockets)

	for run := 2; run <= 2*defaultMaxUnverifiedRuns; run++ {
		assert.Equal(t, make([]bool, sockets), check(context.Background(), ids), "run %d closes nothing", run)
	}
}

// TestRevocationCheckerUserBlockLookups: the user-block lookups of a user
// with many sockets issued at different times: one when no block covers the
// oldest, two when a block covers the newest, and one per issue time only
// when the block falls between the oldest and the newest.
func TestRevocationCheckerUserBlockLookups(t *testing.T) {
	t.Parallel()
	const n = 100

	tests := []struct {
		name      string
		block     bool
		offset    time.Duration // first issue time relative to the block
		wantCalls int
		wantN     int // revoked identities
	}{
		{name: "no block", offset: -time.Hour, wantCalls: 1, wantN: 0},
		{name: "block after every socket", block: true, offset: -time.Hour, wantCalls: 2, wantN: n},
		{name: "block before every socket", block: true, offset: time.Hour, wantCalls: 1, wantN: 0},
		// Issued 1s apart, half a second off the block: 50 before, 50 after;
		// min and max, and one exact lookup for each of the 98 in between.
		{name: "block between the sockets", block: true, offset: -50*time.Second + 500*time.Millisecond, wantCalls: n, wantN: 50},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newCountingStore(t)
			if tt.block {
				require.NoError(t, store.BlockUser(context.Background(), "user-0", time.Now().Add(time.Hour)))
			}
			base := time.Now().Add(tt.offset)
			ids := make([]websocket.ClientIdentity, n)
			for i := range ids {
				ids[i] = websocket.ClientIdentity{UserID: "user-0", IssuedAt: base.Add(time.Duration(i) * time.Second)}
			}
			ids = append(ids, ids...) // two sockets per issue time: one lookup per time
			got := NewWebSocketRevocationChecker(store, newFakeUsers(1), nil, true)(context.Background(), ids)
			revoked := 0
			for _, r := range got {
				if r {
					revoked++
				}
			}
			assert.Equal(t, 2*tt.wantN, revoked)
			assert.Equal(t, tt.wantCalls, store.userBlockCalls())
		})
	}
}

// TestRevocationCheckerKeyBatches: a key used by many sockets is read once
// per run, in one batch per chunk at most.
func TestRevocationCheckerKeyBatches(t *testing.T) {
	t.Parallel()
	keys := &fakeKeys{keys: map[string]*models.APIKey{"key-1": {ID: "key-1", UserID: "user-0"}}}
	ids := make([]websocket.ClientIdentity, 2*revalidateChunkSize+1)
	for i := range ids {
		ids[i] = websocket.ClientIdentity{UserID: "user-0", APIKeyID: "key-1", IssuedAt: time.Now()}
	}
	got := NewWebSocketRevocationChecker(nil, newFakeUsers(1), keys, true)(context.Background(), ids)
	assert.Equal(t, make([]bool, len(ids)), got)
	assert.Equal(t, 1, keys.asked, "the key is asked for once")
	assert.Equal(t, 1, keys.batches)
}

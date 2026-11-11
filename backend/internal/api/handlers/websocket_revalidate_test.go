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

// countingStore counts the blocklist lookups. With a budget >= 0, the lookups
// after the first budget ones of a run block until the run context ends and
// return its error (a run that is too slow, not a failing store). failErr
// makes every lookup fail while the context is alive (a failing store).
type countingStore struct {
	sessionstore.SessionStore
	mu       sync.Mutex
	calls    int
	runCalls int
	budget   int // < 0: no budget
	failErr  error
	tokens   []string // jti of every IsTokenBlocked call, in order
	userBlk  int      // IsUserBlocked calls
}

func newCountingStore(t *testing.T) *countingStore {
	t.Helper()
	inner := sessionstore.NewMemory()
	t.Cleanup(inner.Stop)
	return &countingStore{SessionStore: inner, budget: -1}
}

// startRun resets the per-run budget.
func (s *countingStore) startRun() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runCalls = 0
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
func (s *countingStore) enter(ctx context.Context, jti string) error {
	s.mu.Lock()
	s.calls++
	s.runCalls++
	if jti != "" {
		s.tokens = append(s.tokens, jti)
	}
	over := s.budget >= 0 && s.runCalls > s.budget
	failErr := s.failErr
	s.mu.Unlock()
	if failErr != nil {
		return failErr
	}
	if over {
		<-ctx.Done()
		return fmt.Errorf("store query: %w", ctx.Err())
	}
	return nil
}

func (s *countingStore) IsTokenBlocked(ctx context.Context, jti string) (bool, error) {
	if err := s.enter(ctx, jti); err != nil {
		return false, err
	}
	return s.SessionStore.IsTokenBlocked(ctx, jti)
}

func (s *countingStore) IsSessionBlocked(ctx context.Context, id string) (bool, error) {
	if err := s.enter(ctx, ""); err != nil {
		return false, err
	}
	return s.SessionStore.IsSessionBlocked(ctx, id)
}

func (s *countingStore) IsUserBlocked(ctx context.Context, id string, at time.Time) (bool, error) {
	s.mu.Lock()
	s.userBlk++
	s.mu.Unlock()
	if err := s.enter(ctx, ""); err != nil {
		return false, err
	}
	return s.SessionStore.IsUserBlocked(ctx, id, at)
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
}

func newFakeUsers(n int) *fakeUsers {
	f := &fakeUsers{users: map[string]*models.User{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("user-%d", i)
		f.users[id] = &models.User{ID: id, Role: models.RoleUser}
	}
	return f
}

func (f *fakeUsers) FindByIDs(_ context.Context, ids []string) (map[string]*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byIDs++
	f.asked += len(ids)
	if f.batchErr != nil {
		return nil, f.batchErr
	}
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
// session.
func jwtIdentities(n int) []websocket.ClientIdentity {
	ids := make([]websocket.ClientIdentity, n)
	past := time.Now().Add(-time.Minute)
	for i := range ids {
		ids[i] = websocket.ClientIdentity{
			UserID: fmt.Sprintf("user-%d", i), TokenID: fmt.Sprintf("jti-%d", i),
			SessionID: fmt.Sprintf("sid-%d", i), IssuedAt: past,
		}
	}
	return ids
}

// TestRevocationCheckerDeadlineMidRunRevokesNothing: a run that runs out of
// its own time is not a store failure: with fail closed on, nothing that was
// not found revoked is closed.
func TestRevocationCheckerDeadlineMidRunRevokesNothing(t *testing.T) {
	t.Parallel()
	store := newCountingStore(t)
	store.budget = 4 // the run gets slow after 4 lookups
	ids := jwtIdentities(10)
	require.NoError(t, store.BlockToken(context.Background(), "jti-0", time.Now().Add(time.Hour)))

	check := NewWebSocketRevocationChecker(store, newFakeUsers(10), nil, true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := check(ctx, ids)

	want := make([]bool, len(ids))
	want[0] = true // checked before the run got slow: still revoked
	assert.Equal(t, want, got, "the identities the run did not reach stay open")
}

// TestRevocationCheckerResumesWhereTheLastRunStopped: identities that a slow
// run did not reach are checked first in the next run, so with a run that is
// always too slow every identity is still checked, in turn.
func TestRevocationCheckerResumesWhereTheLastRunStopped(t *testing.T) {
	t.Parallel()
	store := newCountingStore(t)
	store.budget = 5 // a little more than one identity per run
	const n = 12
	ids := jwtIdentities(n)
	require.NoError(t, store.BlockToken(context.Background(), "jti-11", time.Now().Add(time.Hour)))

	check := NewWebSocketRevocationChecker(store, newFakeUsers(n), nil, true)
	revokedSeen := false
	var firstOfRun []string
	for run := 0; run < 3*n && len(distinct(store.seenTokens())) < n; run++ {
		before := len(store.seenTokens())
		store.startRun()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		got := check(ctx, ids) // the same order every run: the worst case
		cancel()
		if seen := store.seenTokens(); len(seen) > before {
			firstOfRun = append(firstOfRun, seen[before])
		}
		for i, r := range got {
			if r {
				require.Equal(t, n-1, i, "only the blocked token is revoked")
				revokedSeen = true
			}
		}

		// The re-check of a new socket between runs (SetRecheck) does not
		// change the order of the next run.
		store.startRun()
		rctx, rcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		check(rctx, ids[:1])
		rcancel()
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
// sockets issued at or before it, and not the later ones, with at most two
// lookups per user.
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
// lookup per distinct token and session, and one or two per user.
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

	got := NewWebSocketRevocationChecker(store, repo, nil, true)(context.Background(), ids)
	assert.Equal(t, make([]bool, sockets), got)

	byIDs, byID := repo.counts()
	assert.Zero(t, byID, "no lookup per user")
	assert.LessOrEqual(t, byIDs, sockets/revalidateChunkSize+1, "one user batch per chunk at most")
	assert.Equal(t, users, repo.askedIDs(), "each user is read once per run")
	assert.Equal(t, users, store.userBlockCalls(), "no user block: one user-block lookup per user, whatever its issue times")
	assert.Equal(t, tokens+sessions+users, store.total(), "one lookup per distinct credential")
	t.Logf("store lookups: %d for %d sockets (sockets x 3 = %d)", store.total(), sockets, 3*sockets)
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

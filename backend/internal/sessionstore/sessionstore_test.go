package sessionstore_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// fakeClock is the time of a store under test. It only moves when a test
// advances it, so expiry and the user block cut are exact.
type fakeClock struct {
	t  time.Time
	mu sync.Mutex
}

// newFakeClock starts half a second into a second, so a whole-second token
// time and a millisecond token time differ.
func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 8, 12, 0, 0, 500_000_000, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// backend builds one store implementation for the shared tests.
type backend struct {
	build func(t *testing.T, clk *fakeClock) sessionstore.SessionStore
	name  string
}

// openSQLite opens an in-memory SQLite database on one connection with the
// session_entries table. SQLite stands in for MySQL in unit tests; the MySQL
// integration tests are in mysql_integration_test.go.
func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&sessionstore.SessionEntry{}))
	return db
}

// quiet is a cleanup interval so long that the cleanup goroutine never runs
// during a test; tests call Cleanup themselves.
const quiet = time.Hour

// extraBackends is added by the integration build (the real MySQL).
var extraBackends []backend

func backends() []backend {
	return append(baseBackends(), extraBackends...)
}

func baseBackends() []backend {
	return []backend{
		{name: "memory", build: func(t *testing.T, clk *fakeClock) sessionstore.SessionStore {
			t.Helper()
			s := sessionstore.NewMemoryWithClock(clk.Now, quiet)
			t.Cleanup(s.Stop)
			return s
		}},
		{name: "database", build: func(t *testing.T, clk *fakeClock) sessionstore.SessionStore {
			t.Helper()
			s := sessionstore.NewMySQLWithClock(openSQLite(t), clk.Now, quiet)
			t.Cleanup(s.Stop)
			return s
		}},
	}
}

// eachBackend runs fn once per store implementation, each with a fresh store.
func eachBackend(t *testing.T, fn func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock)) {
	t.Helper()
	for _, b := range backends() {
		b := b
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			clk := newFakeClock()
			fn(t, b.build(t, clk), clk)
		})
	}
}

func TestBlockToken(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		blocked, err := s.IsTokenBlocked(ctx, "jti-1")
		require.NoError(t, err)
		assert.False(t, blocked, "an unknown token is not blocked")

		require.NoError(t, s.BlockToken(ctx, "jti-1", clk.Now().Add(time.Minute)))

		for _, step := range []struct {
			name    string
			advance time.Duration
			want    bool
		}{
			{name: "at once", advance: 0, want: true},
			{name: "just before the expiry", advance: time.Minute - time.Millisecond, want: true},
			{name: "at the expiry", advance: time.Millisecond, want: false},
			{name: "after the expiry", advance: time.Hour, want: false},
		} {
			clk.Advance(step.advance)
			blocked, err = s.IsTokenBlocked(ctx, "jti-1")
			require.NoError(t, err, step.name)
			assert.Equal(t, step.want, blocked, step.name)
		}
		assert.Equal(t, 1, sessionstore.EntryCount(s), "the expired row stays until Cleanup")
	})
}

func TestBlockTokenSubMillisecondExpiryIsNotShortened(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		// The block ends at 100.4 ms; the store keeps whole milliseconds and
		// rounds up, so the token is blocked for as long as it is valid.
		require.NoError(t, s.BlockToken(ctx, "jti", clk.Now().Add(100*time.Millisecond+400*time.Microsecond)))
		clk.Advance(100 * time.Millisecond)
		blocked, err := s.IsTokenBlocked(ctx, "jti")
		require.NoError(t, err)
		assert.True(t, blocked)
	})
}

func TestBlockTokenAgainKeepsTheLaterExpiry(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, s.BlockToken(ctx, "jti", clk.Now().Add(time.Hour)))
		require.NoError(t, s.BlockToken(ctx, "jti", clk.Now().Add(time.Minute)))
		clk.Advance(30 * time.Minute)
		blocked, err := s.IsTokenBlocked(ctx, "jti")
		require.NoError(t, err)
		assert.True(t, blocked, "a shorter block does not shorten a longer one")

		// A later expiry extends it.
		require.NoError(t, s.BlockToken(ctx, "jti", clk.Now().Add(2*time.Hour)))
		clk.Advance(90 * time.Minute)
		blocked, err = s.IsTokenBlocked(ctx, "jti")
		require.NoError(t, err)
		assert.True(t, blocked)
	})
}

func TestBlockKindsAreSeparate(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		until := clk.Now().Add(time.Hour)
		require.NoError(t, s.BlockToken(ctx, "same-id", until))

		sessionBlocked, err := s.IsSessionBlocked(ctx, "same-id")
		require.NoError(t, err)
		assert.False(t, sessionBlocked, "a token block is not a session block")
		userBlocked, err := s.IsUserBlocked(ctx, "same-id", clk.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.False(t, userBlocked, "a token block is not a user block")

		other, err := s.IsTokenBlocked(ctx, "SAME-ID")
		require.NoError(t, err)
		assert.False(t, other, "keys are case-sensitive")
	})
}

func TestBlockUserCut(t *testing.T) {
	t.Parallel()
	// The block is made at clock time T = 12:00:00.500. A token is blocked
	// when it was issued at or before T (to the millisecond); a token issued
	// after T stays valid. A token without iat is always blocked.
	second := func(t time.Time) time.Time { return t.Truncate(time.Second) }
	tests := []struct {
		issuedAt func(block time.Time) time.Time
		name     string
		want     bool
	}{
		{name: "issued an hour before", issuedAt: func(b time.Time) time.Time { return b.Add(-time.Hour) }, want: true},
		{name: "issued a second before", issuedAt: func(b time.Time) time.Time { return b.Add(-time.Second) }, want: true},
		{name: "issued one millisecond before", issuedAt: func(b time.Time) time.Time { return b.Add(-time.Millisecond) }, want: true},
		{name: "issued in the block millisecond (boundary, blocked)", issuedAt: func(b time.Time) time.Time { return b }, want: true},
		{name: "issued one millisecond after", issuedAt: func(b time.Time) time.Time { return b.Add(time.Millisecond) }, want: false},
		{name: "issued a second after", issuedAt: func(b time.Time) time.Time { return b.Add(time.Second) }, want: false},
		{name: "issued an hour after", issuedAt: func(b time.Time) time.Time { return b.Add(time.Hour) }, want: false},
		// A token without the millisecond claim has a whole-second time. It
		// may be older than the block, so the block second blocks it.
		{name: "whole-second iat in the block second", issuedAt: func(b time.Time) time.Time { return second(b) }, want: true},
		{name: "whole-second iat of the next second", issuedAt: func(b time.Time) time.Time { return second(b).Add(time.Second) }, want: false},
		{name: "no iat", issuedAt: func(time.Time) time.Time { return time.Time{} }, want: true},
		{name: "iat at the Unix epoch", issuedAt: func(time.Time) time.Time { return time.Unix(0, 0) }, want: true},
		// Clock skew: the block time comes from the replica that blocked, the
		// iat from the replica that issued. The store cannot tell them apart,
		// so a skew moves the cut by the skew. A token from a replica that is
		// behind is blocked (safe); one from a replica that is ahead stays
		// valid (the limit of the rule; it needs NTP).
		{name: "issued after the block by a replica 2s behind", issuedAt: func(b time.Time) time.Time { return b.Add(time.Millisecond - 2*time.Second) }, want: true},
		{name: "issued before the block by a replica 2s ahead", issuedAt: func(b time.Time) time.Time { return b.Add(-time.Millisecond + 2*time.Second) }, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
				t.Helper()
				ctx := context.Background()
				blockAt := clk.Now()
				require.NoError(t, s.BlockUser(ctx, "user-1", blockAt.Add(time.Hour)))
				clk.Advance(time.Minute) // the check happens later than the block

				got, err := s.IsUserBlocked(ctx, "user-1", tt.issuedAt(blockAt))
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)

				other, err := s.IsUserBlocked(ctx, "user-2", time.Time{})
				require.NoError(t, err)
				assert.False(t, other, "another user is not blocked")
			})
		})
	}
}

func TestBlockUserLifecycle(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		first := clk.Now()
		old := first.Add(-time.Minute)

		blocked, err := s.IsUserBlocked(ctx, "u", old)
		require.NoError(t, err)
		assert.False(t, blocked, "never blocked")

		require.NoError(t, s.BlockUser(ctx, "u", first.Add(time.Hour)))
		clk.Advance(10 * time.Minute)
		fresh := clk.Now() // a token issued after the first block
		blocked, err = s.IsUserBlocked(ctx, "u", fresh)
		require.NoError(t, err)
		assert.False(t, blocked, "a token issued after the block is valid")

		// A second block moves the cut forward: the token is now older.
		clk.Advance(time.Second)
		require.NoError(t, s.BlockUser(ctx, "u", first.Add(time.Minute)))
		blocked, err = s.IsUserBlocked(ctx, "u", fresh)
		require.NoError(t, err)
		assert.True(t, blocked, "the new block covers the token")

		// The shorter second until did not shorten the first block.
		clk.Advance(30 * time.Minute)
		blocked, err = s.IsUserBlocked(ctx, "u", fresh)
		require.NoError(t, err)
		assert.True(t, blocked, "a shorter block does not shorten a longer one")

		// The block ends at its expiry, before any Cleanup.
		clk.Advance(time.Hour)
		blocked, err = s.IsUserBlocked(ctx, "u", old)
		require.NoError(t, err)
		assert.False(t, blocked, "an expired block is not reported")
		assert.Positive(t, sessionstore.EntryCount(s), "the expired row is still stored")

		// UnblockUser removes the block; unblocking a never-blocked user is fine.
		require.NoError(t, s.BlockUser(ctx, "u", clk.Now().Add(time.Hour)))
		blocked, err = s.IsUserBlocked(ctx, "u", old)
		require.NoError(t, err)
		require.True(t, blocked)
		require.NoError(t, s.UnblockUser(ctx, "u"))
		blocked, err = s.IsUserBlocked(ctx, "u", old)
		require.NoError(t, err)
		assert.False(t, blocked)
		require.NoError(t, s.UnblockUser(ctx, "never-blocked"))
	})
}

// The cut of a user block never moves back: a BlockUser whose clock reads an
// earlier time (a replica that is behind, or a write that commits late) keeps
// the later cut, so the tokens that the first block revoked stay revoked.
func TestBlockUserCutNeverMovesBack(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		later := clk.Now()
		require.NoError(t, s.BlockUser(ctx, "u", later.Add(time.Hour)))
		issued := later.Add(-time.Second) // revoked by the first block

		clk.Advance(-time.Minute) // the second writer's clock is a minute behind
		require.NoError(t, s.BlockUser(ctx, "u", clk.Now().Add(time.Hour)))

		blocked, err := s.IsUserBlocked(ctx, "u", issued)
		require.NoError(t, err)
		assert.True(t, blocked, "an earlier block time must not lower the cut")
		blocked, err = s.IsUserBlocked(ctx, "u", later)
		require.NoError(t, err)
		assert.True(t, blocked, "the cut stays at the later block, inclusive")
		blocked, err = s.IsUserBlocked(ctx, "u", later.Add(time.Millisecond))
		require.NoError(t, err)
		assert.False(t, blocked, "a token issued after the later cut stays valid")
	})
}

func TestBlockSession(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		blocked, err := s.IsSessionBlocked(ctx, "sid-1")
		require.NoError(t, err)
		assert.False(t, blocked)

		require.NoError(t, s.BlockSession(ctx, "sid-1", clk.Now().Add(time.Minute)))
		blocked, err = s.IsSessionBlocked(ctx, "sid-1")
		require.NoError(t, err)
		assert.True(t, blocked)

		// A shorter second block keeps the later expiry.
		require.NoError(t, s.BlockSession(ctx, "sid-1", clk.Now().Add(time.Second)))
		clk.Advance(30 * time.Second)
		blocked, err = s.IsSessionBlocked(ctx, "sid-1")
		require.NoError(t, err)
		assert.True(t, blocked)

		clk.Advance(31 * time.Second)
		blocked, err = s.IsSessionBlocked(ctx, "sid-1")
		require.NoError(t, err)
		assert.False(t, blocked, "expired before Cleanup")
		assert.Equal(t, 1, sessionstore.EntryCount(s))
	})
}

func TestBlockSessions(t *testing.T) {
	t.Parallel()
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("sid-%04d", i)
		}
		return out
	}
	tests := []struct {
		name      string
		ids       []string
		wantCount int
	}{
		{name: "nil slice is a no-op", ids: nil, wantCount: 0},
		{name: "empty slice is a no-op", ids: []string{}, wantCount: 0},
		{name: "one", ids: ids(1), wantCount: 1},
		{name: "a duplicate is written once", ids: []string{"a", "b", "a", "b", "a"}, wantCount: 2},
		{name: "exactly one batch", ids: ids(500), wantCount: 500},
		{name: "one more than a batch", ids: ids(501), wantCount: 501},
		{name: "several batches", ids: ids(1203), wantCount: 1203},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
				t.Helper()
				ctx := context.Background()
				require.NoError(t, s.BlockSessions(ctx, tt.ids, clk.Now().Add(time.Hour)))
				assert.Equal(t, tt.wantCount, sessionstore.EntryCount(s))
				for _, id := range tt.ids {
					blocked, err := s.IsSessionBlocked(ctx, id)
					require.NoError(t, err)
					require.True(t, blocked, id)
				}
				blocked, err := s.IsSessionBlocked(ctx, "not-in-the-list")
				require.NoError(t, err)
				assert.False(t, blocked)
			})
		})
	}
}

func TestBlockSessionsKeepsTheLaterExpiryPerSession(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, s.BlockSession(ctx, "long", clk.Now().Add(time.Hour)))
		require.NoError(t, s.BlockSessions(ctx, []string{"long", "short"}, clk.Now().Add(time.Minute)))
		clk.Advance(30 * time.Minute)

		long, err := s.IsSessionBlocked(ctx, "long")
		require.NoError(t, err)
		short, err := s.IsSessionBlocked(ctx, "short")
		require.NoError(t, err)
		assert.True(t, long)
		assert.False(t, short)
	})
}

func TestOIDCState(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		data := sessionstore.OIDCStateData{CodeVerifier: `v"1\` + "\u00e5\u03a9", RedirectURL: "/dash?x=1&y=<2>"}
		require.NoError(t, s.SaveOIDCState(ctx, "state-1", data, 5*time.Minute))

		got, err := s.ConsumeOIDCState(ctx, "state-1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, data, *got, "the data comes back as saved")
		assert.Zero(t, sessionstore.EntryCount(s), "consuming removes the row")

		got, err = s.ConsumeOIDCState(ctx, "state-1")
		require.NoError(t, err)
		assert.Nil(t, got, "a state works once")

		got, err = s.ConsumeOIDCState(ctx, "never-saved")
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestOIDCStateExpiry(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, s.SaveOIDCState(ctx, "live", sessionstore.OIDCStateData{CodeVerifier: "a"}, time.Minute))
		require.NoError(t, s.SaveOIDCState(ctx, "dead", sessionstore.OIDCStateData{CodeVerifier: "b"}, time.Minute))

		clk.Advance(time.Minute - time.Millisecond)
		got, err := s.ConsumeOIDCState(ctx, "live")
		require.NoError(t, err)
		require.NotNil(t, got, "valid just before the end of the ttl")

		clk.Advance(time.Millisecond)
		got, err = s.ConsumeOIDCState(ctx, "dead")
		require.NoError(t, err)
		assert.Nil(t, got, "an expired state is not returned, before any Cleanup")
	})
}

func TestOIDCStateSaveAgainReplaces(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, _ *fakeClock) {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, s.SaveOIDCState(ctx, "s", sessionstore.OIDCStateData{CodeVerifier: "old"}, time.Minute))
		require.NoError(t, s.SaveOIDCState(ctx, "s", sessionstore.OIDCStateData{CodeVerifier: "new"}, time.Minute))

		got, err := s.ConsumeOIDCState(ctx, "s")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "new", got.CodeVerifier)
		assert.Zero(t, sessionstore.EntryCount(s))
	})
}

func TestOIDCStateStatesAreCaseSensitive(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, _ *fakeClock) {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, s.SaveOIDCState(ctx, "AbC", sessionstore.OIDCStateData{CodeVerifier: "x"}, time.Minute))
		got, err := s.ConsumeOIDCState(ctx, "abc")
		require.NoError(t, err)
		assert.Nil(t, got)
		got, err = s.ConsumeOIDCState(ctx, "AbC")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})
}

// A state is single-use even when many requests consume it at once: exactly
// one gets the data. Run with -race.
func TestOIDCStateConcurrentConsumeIsSingleUse(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, _ *fakeClock) {
		t.Helper()
		ctx := context.Background()
		const rounds, consumers = 20, 16
		for round := 0; round < rounds; round++ {
			state := fmt.Sprintf("state-%d", round)
			require.NoError(t, s.SaveOIDCState(ctx, state, sessionstore.OIDCStateData{CodeVerifier: state}, time.Minute))

			var wg sync.WaitGroup
			var mu sync.Mutex
			winners, errs := 0, 0
			start := make(chan struct{})
			for i := 0; i < consumers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					got, err := s.ConsumeOIDCState(ctx, state)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs++
					}
					if got != nil {
						winners++
						assert.Equal(t, state, got.CodeVerifier)
					}
				}()
			}
			close(start)
			wg.Wait()
			assert.Equal(t, 1, winners, "round %d: exactly one consumer gets the state", round)
			assert.Zero(t, errs, "round %d: a lost race is not an error", round)
		}
	})
}

func TestCleanupRemovesOnlyExpiredRows(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		soon, later := clk.Now().Add(time.Minute), clk.Now().Add(time.Hour)
		require.NoError(t, s.BlockToken(ctx, "tok-soon", soon))
		require.NoError(t, s.BlockToken(ctx, "tok-later", later))
		require.NoError(t, s.BlockUser(ctx, "user-soon", soon))
		require.NoError(t, s.BlockUser(ctx, "user-later", later))
		require.NoError(t, s.BlockSession(ctx, "sid-soon", soon))
		require.NoError(t, s.BlockSession(ctx, "sid-later", later))
		require.NoError(t, s.SaveOIDCState(ctx, "state-soon", sessionstore.OIDCStateData{}, time.Minute))
		require.NoError(t, s.SaveOIDCState(ctx, "state-later", sessionstore.OIDCStateData{}, time.Hour))
		require.Equal(t, 8, sessionstore.EntryCount(s))

		require.NoError(t, s.Cleanup(ctx), "nothing is expired yet")
		assert.Equal(t, 8, sessionstore.EntryCount(s))

		clk.Advance(time.Minute) // the "soon" rows end exactly now
		require.NoError(t, s.Cleanup(ctx))
		assert.Equal(t, 4, sessionstore.EntryCount(s), "the four expired rows are gone")

		old := clk.Now().Add(-time.Hour)
		tok, err := s.IsTokenBlocked(ctx, "tok-later")
		require.NoError(t, err)
		user, err := s.IsUserBlocked(ctx, "user-later", old)
		require.NoError(t, err)
		sess, err := s.IsSessionBlocked(ctx, "sid-later")
		require.NoError(t, err)
		state, err := s.ConsumeOIDCState(ctx, "state-later")
		require.NoError(t, err)
		assert.True(t, tok)
		assert.True(t, user)
		assert.True(t, sess)
		assert.NotNil(t, state, "the unexpired rows still work")

		require.NoError(t, s.Cleanup(ctx), "a second run is a no-op")
	})
}

func TestInvalidKeys(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 256)
	tests := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "256 characters", key: long},
		{name: "invalid UTF-8", key: "a\xffb"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
				t.Helper()
				ctx := context.Background()
				until := clk.Now().Add(time.Hour)
				calls := map[string]func() error{
					"BlockToken":     func() error { return s.BlockToken(ctx, tt.key, until) },
					"BlockUser":      func() error { return s.BlockUser(ctx, tt.key, until) },
					"BlockSession":   func() error { return s.BlockSession(ctx, tt.key, until) },
					"BlockSessions":  func() error { return s.BlockSessions(ctx, []string{"ok", tt.key}, until) },
					"UnblockUser":    func() error { return s.UnblockUser(ctx, tt.key) },
					"SaveOIDCState":  func() error { return s.SaveOIDCState(ctx, tt.key, sessionstore.OIDCStateData{}, time.Minute) },
					"IsTokenBlocked": func() error { _, err := s.IsTokenBlocked(ctx, tt.key); return err },
					"IsUserBlocked":  func() error { _, err := s.IsUserBlocked(ctx, tt.key, time.Time{}); return err },
					"IsSession":      func() error { _, err := s.IsSessionBlocked(ctx, tt.key); return err },
					"ConsumeState":   func() error { _, err := s.ConsumeOIDCState(ctx, tt.key); return err },
				}
				for name, call := range calls {
					err := call()
					require.ErrorIs(t, err, sessionstore.ErrInvalidKey, name)
					assert.NotContains(t, err.Error(), "aaaa", "%s: the key is not in the error", name)
				}
				assert.Zero(t, sessionstore.EntryCount(s), "BlockSessions wrote nothing for the valid id either")
			})
		})
	}
}

func TestKeyAtTheColumnLimitIsStored(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx := context.Background()
		for _, key := range []string{strings.Repeat("a", 255), strings.Repeat("å", 255), strings.Repeat("\U0001F600", 255)} {
			require.NoError(t, s.BlockToken(ctx, key, clk.Now().Add(time.Hour)))
			blocked, err := s.IsTokenBlocked(ctx, key)
			require.NoError(t, err)
			assert.True(t, blocked)
		}
	})
}

func TestOIDCStateLimits(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, _ *fakeClock) {
		t.Helper()
		ctx := context.Background()
		for _, ttl := range []time.Duration{0, -time.Second} {
			err := s.SaveOIDCState(ctx, "s", sessionstore.OIDCStateData{}, ttl)
			require.ErrorIs(t, err, sessionstore.ErrInvalidTTL, ttl.String())
		}
		err := s.SaveOIDCState(ctx, "s", sessionstore.OIDCStateData{RedirectURL: strings.Repeat("x", 70000)}, time.Minute)
		require.ErrorIs(t, err, sessionstore.ErrStateTooLarge)
		assert.Zero(t, sessionstore.EntryCount(s))

		// Just below the column size is stored as given.
		big := sessionstore.OIDCStateData{RedirectURL: strings.Repeat("x", 60000)}
		require.NoError(t, s.SaveOIDCState(ctx, "big", big, time.Minute))
		got, err := s.ConsumeOIDCState(ctx, "big")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, big, *got)
	})
}

// A cancelled request context reaches the caller as context.Canceled, so the
// handler layer can answer 499.
func TestCancelledContext(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		until := clk.Now().Add(time.Hour)
		calls := map[string]func() error{
			"BlockToken":     func() error { return s.BlockToken(ctx, "k", until) },
			"BlockUser":      func() error { return s.BlockUser(ctx, "k", until) },
			"BlockSession":   func() error { return s.BlockSession(ctx, "k", until) },
			"BlockSessions":  func() error { return s.BlockSessions(ctx, []string{"k"}, until) },
			"UnblockUser":    func() error { return s.UnblockUser(ctx, "k") },
			"SaveOIDCState":  func() error { return s.SaveOIDCState(ctx, "k", sessionstore.OIDCStateData{}, time.Minute) },
			"IsTokenBlocked": func() error { _, err := s.IsTokenBlocked(ctx, "k"); return err },
			"IsUserBlocked":  func() error { _, err := s.IsUserBlocked(ctx, "k", time.Time{}); return err },
			"IsSession":      func() error { _, err := s.IsSessionBlocked(ctx, "k"); return err },
			"ConsumeState":   func() error { _, err := s.ConsumeOIDCState(ctx, "k"); return err },
			"Cleanup":        func() error { return s.Cleanup(ctx) },
		}
		for name, call := range calls {
			err := call()
			require.Error(t, err, name)
			require.ErrorIs(t, err, context.Canceled, name)
		}
		assert.Zero(t, sessionstore.EntryCount(s))
	})
}

func TestStopIsIdempotentAndEndsTheCleanupGoroutine(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s sessionstore.SessionStore, clk *fakeClock) {
		t.Helper()
		select {
		case <-sessionstore.CleanupExited(s):
			t.Fatal("the cleanup goroutine ended before Stop")
		default:
		}

		s.Stop()
		select {
		case <-sessionstore.CleanupExited(s):
		default:
			t.Fatal("Stop returned while the cleanup goroutine still runs")
		}
		s.Stop() // a second Stop is a no-op
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); s.Stop() }()
		}
		wg.Wait()

		// The data stays usable after Stop.
		ctx := context.Background()
		require.NoError(t, s.BlockToken(ctx, "after-stop", clk.Now().Add(time.Hour)))
		blocked, err := s.IsTokenBlocked(ctx, "after-stop")
		require.NoError(t, err)
		assert.True(t, blocked)
	})
}

// The cleanup goroutine removes expired rows by itself and stops on Stop.
func TestCleanupGoroutineRemovesExpiredRows(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"memory", "database"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clk := newFakeClock()
			var s sessionstore.SessionStore
			if name == "memory" {
				s = sessionstore.NewMemoryWithClock(clk.Now, 5*time.Millisecond)
			} else {
				s = sessionstore.NewMySQLWithClock(openSQLite(t), clk.Now, 5*time.Millisecond)
			}
			defer s.Stop()
			ctx := context.Background()
			require.NoError(t, s.BlockToken(ctx, "t", clk.Now().Add(time.Minute)))
			require.NoError(t, s.SaveOIDCState(ctx, "s", sessionstore.OIDCStateData{}, time.Minute))
			time.Sleep(30 * time.Millisecond)
			assert.Equal(t, 2, sessionstore.EntryCount(s), "nothing is expired yet")

			clk.Advance(time.Hour)
			assert.Eventually(t, func() bool { return sessionstore.EntryCount(s) == 0 }, 5*time.Second, 5*time.Millisecond)

			s.Stop()
			<-sessionstore.CleanupExited(s)
		})
	}
}

func TestDatabaseStoreReportsErrorsOfTheDatabase(t *testing.T) {
	t.Parallel()
	db := openSQLite(t)
	clk := newFakeClock()
	s := sessionstore.NewMySQLWithClock(db, clk.Now, 5*time.Millisecond)
	defer s.Stop()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	ctx := context.Background()
	until := clk.Now().Add(time.Hour)
	require.Error(t, s.BlockToken(ctx, "k", until))
	require.Error(t, s.BlockUser(ctx, "k", until))
	require.Error(t, s.BlockSessions(ctx, []string{"k"}, until))
	require.Error(t, s.UnblockUser(ctx, "k"))
	require.Error(t, s.SaveOIDCState(ctx, "k", sessionstore.OIDCStateData{}, time.Minute))
	require.Error(t, s.Cleanup(ctx))
	_, err = s.IsTokenBlocked(ctx, "k")
	require.Error(t, err)
	_, err = s.IsUserBlocked(ctx, "k", time.Time{})
	require.Error(t, err)
	_, err = s.IsSessionBlocked(ctx, "k")
	require.Error(t, err)
	_, err = s.ConsumeOIDCState(ctx, "k")
	require.Error(t, err)

	time.Sleep(20 * time.Millisecond) // the cleanup goroutine logs the failure and keeps running
	select {
	case <-sessionstore.CleanupExited(s):
		t.Fatal("a failed cleanup must not end the goroutine")
	default:
	}
}

func TestConstructors(t *testing.T) {
	t.Parallel()
	m := sessionstore.NewMemory()
	m.Stop()
	d := sessionstore.NewMySQL(openSQLite(t))
	d.Stop()
	for _, s := range []sessionstore.SessionStore{m, d} {
		select {
		case <-sessionstore.CleanupExited(s):
		default:
			t.Fatal("Stop did not end the cleanup goroutine")
		}
	}
}

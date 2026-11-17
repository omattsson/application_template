package database

import (
	"context"
	"sync"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	idemTTL  = time.Hour
	idemLock = time.Minute
)

func newIdemRepo(t *testing.T) *GORMIdempotencyRepository {
	t.Helper()
	return NewGORMIdempotencyRepository(openMigratedDB(t))
}

func idemClaim(t *testing.T, r *GORMIdempotencyRepository, scope, key, hash string) *models.IdempotencyClaim {
	t.Helper()
	c, err := r.Claim(context.Background(), scope, key, hash, idemTTL, idemLock)
	require.NoError(t, err)
	require.NotNil(t, c)
	return c
}

// idemAge moves the lock (and optionally the expiry) of a row into the past.
func idemAge(t *testing.T, db *gorm.DB, scope, key string, set string) {
	t.Helper()
	require.NoError(t, db.Exec("UPDATE idempotency_keys SET "+set+" WHERE scope = ? AND idempotency_key = ?", scope, key).Error)
}

func idemRows(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Table("idempotency_keys").Count(&n).Error)
	return n
}

func TestIdempotencyClaimLifecycle(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	ctx := context.Background()
	claim := func(hash string) *models.IdempotencyClaim {
		c, err := r.Claim(ctx, "u1", "k1", hash, idemTTL, idemLock)
		require.NoError(t, err)
		return c
	}

	first := claim("hash-a")
	require.Equal(t, models.IdempotencyClaimed, first.Outcome)
	require.NotEmpty(t, first.LockID)

	t.Run("same request while running is in progress", func(t *testing.T) {
		c := claim("hash-a")
		assert.Equal(t, models.IdempotencyInProgress, c.Outcome)
		assert.GreaterOrEqual(t, c.RetryAfter, time.Second)
		assert.LessOrEqual(t, c.RetryAfter, idemLock)
		assert.Empty(t, c.LockID)
	})

	t.Run("another request is a mismatch even while running", func(t *testing.T) {
		assert.Equal(t, models.IdempotencyMismatch, claim("hash-b").Outcome)
	})

	t.Run("another owner cannot complete", func(t *testing.T) {
		ok, err := r.Complete(ctx, "u1", "k1", "not-the-owner", models.IdempotencyResponse{StatusCode: 201, Body: []byte("x")}, idemTTL)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, models.IdempotencyInProgress, claim("hash-a").Outcome)
	})

	resp := models.IdempotencyResponse{StatusCode: 201, ContentType: "application/json; charset=utf-8", Body: []byte(`{"id":1}`)}
	ok, err := r.Complete(ctx, "u1", "k1", first.LockID, resp, idemTTL)
	require.NoError(t, err)
	require.True(t, ok)

	t.Run("the finished request is replayed", func(t *testing.T) {
		c := claim("hash-a")
		require.Equal(t, models.IdempotencyReplay, c.Outcome)
		require.NotNil(t, c.Response)
		assert.Equal(t, resp, *c.Response)
	})

	t.Run("another request is a mismatch after completion", func(t *testing.T) {
		assert.Equal(t, models.IdempotencyMismatch, claim("hash-b").Outcome)
	})

	t.Run("a second complete changes nothing", func(t *testing.T) {
		ok, err := r.Complete(ctx, "u1", "k1", first.LockID, models.IdempotencyResponse{StatusCode: 500}, idemTTL)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, 201, claim("hash-a").Response.StatusCode)
	})

	t.Run("release leaves a finished row", func(t *testing.T) {
		require.NoError(t, r.Release(ctx, "u1", "k1", first.LockID))
		assert.Equal(t, models.IdempotencyReplay, claim("hash-a").Outcome)
	})
}

func TestIdempotencyEmptyBodyAndContentTypeRoundTrip(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	c := idemClaim(t, r, "u", "k", "h")
	ok, err := r.Complete(context.Background(), "u", "k", c.LockID, models.IdempotencyResponse{StatusCode: 204}, idemTTL)
	require.NoError(t, err)
	require.True(t, ok)
	got := idemClaim(t, r, "u", "k", "h")
	require.Equal(t, models.IdempotencyReplay, got.Outcome)
	assert.Equal(t, 204, got.Response.StatusCode)
	assert.Empty(t, got.Response.Body)
	assert.Empty(t, got.Response.ContentType)
}

func TestIdempotencyRelease(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	ctx := context.Background()
	c := idemClaim(t, r, "u", "k", "h")

	require.NoError(t, r.Release(ctx, "u", "k", "someone-else"))
	assert.Equal(t, int64(1), idemRows(t, r.DB), "a foreign owner cannot release")

	require.NoError(t, r.Release(ctx, "u", "k", c.LockID))
	assert.Zero(t, idemRows(t, r.DB))
	require.NoError(t, r.Release(ctx, "u", "k", c.LockID), "a missing row is not an error")

	again := idemClaim(t, r, "u", "k", "other-hash")
	assert.Equal(t, models.IdempotencyClaimed, again.Outcome, "after a release the key is free for any request")
}

func TestIdempotencyScopes(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	a := idemClaim(t, r, "user-a", "same-key", "h")
	b := idemClaim(t, r, "user-b", "same-key", "h")
	s := idemClaim(t, r, models.IdempotencyScopeShared, "same-key", "h")
	assert.Equal(t, models.IdempotencyClaimed, a.Outcome)
	assert.Equal(t, models.IdempotencyClaimed, b.Outcome)
	assert.Equal(t, models.IdempotencyClaimed, s.Outcome)
	assert.Equal(t, int64(3), idemRows(t, r.DB))
}

func TestIdempotencyKeysAreCaseSensitive(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	assert.Equal(t, models.IdempotencyClaimed, idemClaim(t, r, "u", "Key", "h").Outcome)
	assert.Equal(t, models.IdempotencyClaimed, idemClaim(t, r, "u", "key", "h").Outcome)
}

func TestIdempotencyStaleLockTakeover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("same request takes over, the old owner is shut out", func(t *testing.T) {
		t.Parallel()
		r := newIdemRepo(t)
		old := idemClaim(t, r, "u", "k", "h")
		idemAge(t, r.DB, "u", "k", "locked_at = '2000-01-01 00:00:00.000'")

		taken := idemClaim(t, r, "u", "k", "h")
		require.Equal(t, models.IdempotencyClaimed, taken.Outcome)
		assert.NotEqual(t, old.LockID, taken.LockID)

		ok, err := r.Complete(ctx, "u", "k", old.LockID, models.IdempotencyResponse{StatusCode: 201}, idemTTL)
		require.NoError(t, err)
		assert.False(t, ok, "the first owner lost the key")
		require.NoError(t, r.Release(ctx, "u", "k", old.LockID))
		assert.Equal(t, int64(1), idemRows(t, r.DB), "and cannot release it either")

		ok, err = r.Complete(ctx, "u", "k", taken.LockID, models.IdempotencyResponse{StatusCode: 201}, idemTTL)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("a fresh lock is not taken over", func(t *testing.T) {
		t.Parallel()
		r := newIdemRepo(t)
		idemClaim(t, r, "u", "k", "h")
		assert.Equal(t, models.IdempotencyInProgress, idemClaim(t, r, "u", "k", "h").Outcome)
	})

	t.Run("another request does not take over a stale lock", func(t *testing.T) {
		t.Parallel()
		r := newIdemRepo(t)
		idemClaim(t, r, "u", "k", "h")
		idemAge(t, r.DB, "u", "k", "locked_at = '2000-01-01 00:00:00.000'")
		assert.Equal(t, models.IdempotencyMismatch, idemClaim(t, r, "u", "k", "different").Outcome)
	})

	t.Run("a finished row is never taken over by its lock age", func(t *testing.T) {
		t.Parallel()
		r := newIdemRepo(t)
		c := idemClaim(t, r, "u", "k", "h")
		_, err := r.Complete(ctx, "u", "k", c.LockID, models.IdempotencyResponse{StatusCode: 200}, idemTTL)
		require.NoError(t, err)
		idemAge(t, r.DB, "u", "k", "locked_at = '2000-01-01 00:00:00.000'")
		assert.Equal(t, models.IdempotencyReplay, idemClaim(t, r, "u", "k", "h").Outcome)
	})
}

func TestIdempotencyExpiredRowIsTakenOver(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	ctx := context.Background()
	c := idemClaim(t, r, "u", "k", "h")
	_, err := r.Complete(ctx, "u", "k", c.LockID, models.IdempotencyResponse{StatusCode: 201, ContentType: "text/plain", Body: []byte("old")}, idemTTL)
	require.NoError(t, err)
	idemAge(t, r.DB, "u", "k", "expires_at = '2000-01-01 00:00:00.000'")

	taken := idemClaim(t, r, "u", "k", "a-new-request")
	require.Equal(t, models.IdempotencyClaimed, taken.Outcome, "an expired key is free, even for another request")

	in := idemClaim(t, r, "u", "k", "a-new-request")
	assert.Equal(t, models.IdempotencyInProgress, in.Outcome, "the old response is gone")
	assert.Equal(t, int64(1), idemRows(t, r.DB))
}

func TestIdempotencyConcurrentClaimsHaveOneWinner(t *testing.T) {
	t.Parallel()
	const n = 12

	run := func(t *testing.T, r *GORMIdempotencyRepository) []*models.IdempotencyClaim {
		t.Helper()
		var wg sync.WaitGroup
		start := make(chan struct{})
		out := make([]*models.IdempotencyClaim, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out[i], errs[i] = r.Claim(context.Background(), "u", "k", "h", idemTTL, idemLock)
			}()
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		return out
	}
	count := func(cs []*models.IdempotencyClaim, o models.IdempotencyOutcome) int {
		k := 0
		for _, c := range cs {
			if c.Outcome == o {
				k++
			}
		}
		return k
	}

	t.Run("a new key", func(t *testing.T) {
		t.Parallel()
		r := newIdemRepo(t)
		cs := run(t, r)
		assert.Equal(t, 1, count(cs, models.IdempotencyClaimed))
		assert.Equal(t, n-1, count(cs, models.IdempotencyInProgress))
		assert.Equal(t, int64(1), idemRows(t, r.DB))
	})

	t.Run("a stale lock", func(t *testing.T) {
		t.Parallel()
		r := newIdemRepo(t)
		idemClaim(t, r, "u", "k", "h")
		idemAge(t, r.DB, "u", "k", "locked_at = '2000-01-01 00:00:00.000'")
		cs := run(t, r)
		assert.Equal(t, 1, count(cs, models.IdempotencyClaimed))
		assert.Equal(t, n-1, count(cs, models.IdempotencyInProgress))
		assert.Equal(t, int64(1), idemRows(t, r.DB))
	})
}

func TestIdempotencyDeleteExpired(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	ctx := context.Background()
	for _, k := range []string{"e1", "e2", "e3", "e4", "e5"} {
		idemClaim(t, r, "u", k, "h")
		idemAge(t, r.DB, "u", k, "expires_at = '2000-01-01 00:00:00.000'")
	}
	idemClaim(t, r, "u", "fresh", "h")

	for _, want := range []int64{2, 2, 1, 0} {
		got, err := r.DeleteExpired(ctx, 2)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.Equal(t, int64(1), idemRows(t, r.DB), "only the fresh row is left")

	_, err := r.DeleteExpired(ctx, 0)
	require.Error(t, err, "a batch size of zero is refused")
}

func TestIdempotencyRepositoryErrors(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	r := NewGORMIdempotencyRepository(db)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	ctx := context.Background()

	_, err = r.Claim(ctx, "u", "k", "h", idemTTL, idemLock)
	require.Error(t, err)
	_, err = r.Complete(ctx, "u", "k", "l", models.IdempotencyResponse{StatusCode: 200}, idemTTL)
	require.Error(t, err)
	require.Error(t, r.Release(ctx, "u", "k", "l"))
	_, err = r.DeleteExpired(ctx, 10)
	require.Error(t, err)
}

func TestIdempotencyCancelledContext(t *testing.T) {
	t.Parallel()
	r := newIdemRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Claim(ctx, "u", "k", "h", idemTTL, idemLock)
	require.ErrorIs(t, err, context.Canceled)
}

func TestIdempotencyTimeSQL(t *testing.T) {
	t.Parallel()
	assert.Contains(t, idemOffsetSQL(dialectMySQL), "UTC_TIMESTAMP(3)")
	assert.Contains(t, idemOffsetSQL("sqlite"), "strftime")
	assert.Equal(t, int64(-1500000), idemOffsetArg(dialectMySQL, -1500*time.Millisecond))
	assert.Equal(t, "+3600.000 seconds", idemOffsetArg("sqlite", time.Hour))
	assert.Equal(t, "-1.500 seconds", idemOffsetArg("sqlite", -1500*time.Millisecond))
}

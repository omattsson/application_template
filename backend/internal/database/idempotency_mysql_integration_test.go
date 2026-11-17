//go:build integration

package database

import (
	"context"
	"sync"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDatabaseIdempotencyRepository runs the idempotency repository on MySQL:
// the migrated column types and collation, the claim of one winner among
// concurrent callers (new key and stale lock), the take-over of an expired
// row, the lock id that shuts out a former owner and the batched cleanup.
func TestDatabaseIdempotencyRepository(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	repo := NewGORMIdempotencyRepository(db.DB)
	ctx := context.Background()
	scope := "it-" + uuid.NewString()[:8]
	t.Cleanup(func() { db.Exec("DELETE FROM idempotency_keys WHERE scope = ?", scope) })

	for col, want := range map[string]string{
		"scope": "varchar(64)", "idempotency_key": "varchar(255)", "body": "mediumblob",
		"locked_at": "datetime(3)", "expires_at": "datetime(3)", "status_code": "smallint",
	} {
		var got string
		require.NoError(t, db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'idempotency_keys' AND column_name = ?", col).Scan(&got).Error)
		assert.Equal(t, want, got, col)
	}
	for _, col := range []string{"scope", "idempotency_key"} {
		var got string
		require.NoError(t, db.Raw("SELECT COLLATION_NAME FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'idempotency_keys' AND column_name = ?", col).Scan(&got).Error)
		assert.Equal(t, "utf8mb4_0900_bin", got, col)
	}
	var idx int64
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'idempotency_keys' AND index_name = 'idx_idempotency_keys_expires_at'").Scan(&idx).Error)
	assert.Equal(t, int64(1), idx, "the janitor's index")

	claim := func(key, hash string) *models.IdempotencyClaim {
		c, err := repo.Claim(ctx, scope, key, hash, time.Hour, time.Minute)
		require.NoError(t, err)
		return c
	}
	age := func(key, set string) {
		require.NoError(t, db.Exec("UPDATE idempotency_keys SET "+set+" WHERE scope = ? AND idempotency_key = ?", scope, key).Error)
	}

	t.Run("lifecycle and case sensitivity", func(t *testing.T) {
		c := claim("Life", "h1")
		require.Equal(t, models.IdempotencyClaimed, c.Outcome)
		assert.Equal(t, models.IdempotencyClaimed, claim("life", "h1").Outcome, "a key that differs in case is another key")
		in := claim("Life", "h1")
		assert.Equal(t, models.IdempotencyInProgress, in.Outcome)
		assert.GreaterOrEqual(t, in.RetryAfter, time.Second)
		assert.LessOrEqual(t, in.RetryAfter, time.Minute)
		assert.Equal(t, models.IdempotencyMismatch, claim("Life", "h2").Outcome)

		resp := models.IdempotencyResponse{StatusCode: 201, ContentType: "application/json; charset=utf-8", Body: []byte(`{"id":1}`)}
		ok, err := repo.Complete(ctx, scope, "Life", c.LockID, resp, time.Hour)
		require.NoError(t, err)
		require.True(t, ok)
		got := claim("Life", "h1")
		require.Equal(t, models.IdempotencyReplay, got.Outcome)
		assert.Equal(t, resp, *got.Response)
		assert.Equal(t, models.IdempotencyMismatch, claim("Life", "h2").Outcome)
	})

	t.Run("a release frees an in-progress key only", func(t *testing.T) {
		c := claim("Rel", "h")
		require.NoError(t, repo.Release(ctx, scope, "Rel", "not-the-owner"))
		assert.Equal(t, models.IdempotencyInProgress, claim("Rel", "h").Outcome)
		require.NoError(t, repo.Release(ctx, scope, "Rel", c.LockID))
		assert.Equal(t, models.IdempotencyClaimed, claim("Rel", "h").Outcome)
	})

	concurrently := func(n int, key, hash string) []*models.IdempotencyClaim {
		out := make([]*models.IdempotencyClaim, n)
		errs := make([]error, n)
		var ready, done sync.WaitGroup
		barrier := make(chan struct{})
		for i := 0; i < n; i++ {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				ready.Done()
				<-barrier
				out[i], errs[i] = repo.Claim(ctx, scope, key, hash, time.Hour, time.Minute)
			}()
		}
		ready.Wait()
		close(barrier)
		done.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		return out
	}
	winners := func(cs []*models.IdempotencyClaim) (claimed, inProgress int) {
		for _, c := range cs {
			switch c.Outcome {
			case models.IdempotencyClaimed:
				claimed++
			case models.IdempotencyInProgress:
				inProgress++
			}
		}
		return
	}

	t.Run("concurrent claims of a new key have one winner", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			key := "race-new-" + uuid.NewString()[:8]
			claimed, inProgress := winners(concurrently(8, key, "h"))
			assert.Equal(t, 1, claimed)
			assert.Equal(t, 7, inProgress)
			var n int64
			require.NoError(t, db.Raw("SELECT COUNT(1) FROM idempotency_keys WHERE scope = ? AND idempotency_key = ?", scope, key).Scan(&n).Error)
			assert.Equal(t, int64(1), n)
		}
	})

	t.Run("concurrent take-overs of a stale lock have one winner", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			key := "race-stale-" + uuid.NewString()[:8]
			old := claim(key, "h")
			age(key, "locked_at = UTC_TIMESTAMP(3) - INTERVAL 1 HOUR")
			claimed, inProgress := winners(concurrently(8, key, "h"))
			assert.Equal(t, 1, claimed)
			assert.Equal(t, 7, inProgress)

			ok, err := repo.Complete(ctx, scope, key, old.LockID, models.IdempotencyResponse{StatusCode: 201}, time.Hour)
			require.NoError(t, err)
			assert.False(t, ok, "the former owner cannot store its response")
		}
	})

	t.Run("a stale lock of another request is a mismatch, an expired row is free", func(t *testing.T) {
		claim("stale-other", "h")
		age("stale-other", "locked_at = UTC_TIMESTAMP(3) - INTERVAL 1 HOUR")
		assert.Equal(t, models.IdempotencyMismatch, claim("stale-other", "other").Outcome)

		c := claim("expired", "h")
		_, err := repo.Complete(ctx, scope, "expired", c.LockID, models.IdempotencyResponse{StatusCode: 200, Body: []byte("old")}, time.Hour)
		require.NoError(t, err)
		age("expired", "expires_at = UTC_TIMESTAMP(3) - INTERVAL 1 SECOND")
		assert.Equal(t, models.IdempotencyClaimed, claim("expired", "another").Outcome)
	})

	t.Run("the cleanup deletes expired rows in batches", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			key := "gc-" + uuid.NewString()[:8]
			claim(key, "h")
			age(key, "expires_at = UTC_TIMESTAMP(3) - INTERVAL 1 DAY")
		}
		total := int64(0)
		for {
			n, err := repo.DeleteExpired(ctx, 2)
			require.NoError(t, err)
			assert.LessOrEqual(t, n, int64(2), "a batch is bounded")
			total += n
			if n < 2 {
				break
			}
		}
		assert.GreaterOrEqual(t, total, int64(5))
		var left int64
		require.NoError(t, db.Raw("SELECT COUNT(1) FROM idempotency_keys WHERE expires_at < UTC_TIMESTAMP(3)").Scan(&left).Error)
		assert.Zero(t, left)
	})

	t.Run("the migration rolls back and applies again", func(t *testing.T) {
		mig := createIdempotencyKeysMigration()
		require.NoError(t, mig.Up(db.DB), "a second Up is a no-op")
		require.NoError(t, mig.Down(db.DB))
		assert.False(t, db.Migrator().HasTable("idempotency_keys"))
		require.NoError(t, mig.Down(db.DB), "a second Down is a no-op")
		require.NoError(t, mig.Up(db.DB))
		assert.True(t, db.Migrator().HasTable("idempotency_keys"))
	})
}

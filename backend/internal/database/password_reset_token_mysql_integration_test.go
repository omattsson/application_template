//go:build integration

package database

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashOf returns a 64-character hex digest that is unique to the test run.
func resetHashOf(seed string) string {
	return sha256Hex(seed + uuid.NewString())
}

// TestDatabasePasswordResetTokenRepository runs the reset token repository on
// MySQL: the migrated column types, the unique hash index, the single winner
// of concurrent consumes and of concurrent issues, the replacement of an
// unused token, the expiry and the batched cleanup, all on the database clock.
func TestDatabasePasswordResetTokenRepository(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())

	repo := NewGORMPasswordResetTokenRepository(db.DB)
	ctx := context.Background()
	prefix := "it-" + uuid.NewString()[:8]
	userID := func(name string) string { return prefix + "-" + name }

	t.Cleanup(func() { db.Exec("DELETE FROM password_reset_tokens WHERE user_id LIKE ?", prefix+"-%") })

	columnType := func(col string) string {
		var got string
		require.NoError(t, db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'password_reset_tokens' AND column_name = ?", col).Scan(&got).Error)

		return got
	}

	for col, want := range map[string]string{
		"id": "varchar(36)", "user_id": "varchar(36)", "token_hash": "varchar(64)",
		"expires_at": "datetime(3)", "created_at": "datetime(3)", "used_at": "datetime(3)",
	} {
		assert.Equal(t, want, columnType(col), col)
	}

	var unique int64
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'password_reset_tokens' AND index_name = 'idx_password_reset_tokens_token_hash' AND non_unique = 0").Scan(&unique).Error)
	assert.Equal(t, int64(1), unique, "the hash is a unique index")

	for _, idx := range []string{"idx_password_reset_tokens_user_id", "idx_password_reset_tokens_expires_at"} {
		var n int64
		require.NoError(t, db.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'password_reset_tokens' AND index_name = ?", idx).Scan(&n).Error)
		assert.Equal(t, int64(1), n, idx)
	}

	// concurrently runs n calls of fn that start at one moment.
	concurrently := func(n int, fn func(i int)) {
		var ready, done sync.WaitGroup

		barrier := make(chan struct{})

		for i := 0; i < n; i++ {
			ready.Add(1)
			done.Add(1)

			go func() {
				defer done.Done()
				ready.Done()
				<-barrier
				fn(i)
			}()
		}

		ready.Wait()
		close(barrier)
		done.Wait()
	}

	count := func(where string, args ...any) int64 {
		var n int64
		require.NoError(t, db.Model(&models.PasswordResetToken{}).Where(where, args...).Count(&n).Error)

		return n
	}

	t.Run("issue then consume once", func(t *testing.T) {
		uid, h := userID("once"), resetHashOf("once")
		issued, err := repo.Issue(ctx, uid, h, time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, issued)

		got, ok, err := repo.Consume(ctx, h)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, uid, got)

		_, ok, err = repo.Consume(ctx, h)
		require.NoError(t, err)
		assert.False(t, ok, "used")
	})

	t.Run("concurrent consumes of one token have one winner", func(t *testing.T) {
		uid, h := userID("race"), resetHashOf("race")
		issued, err := repo.Issue(ctx, uid, h, time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, issued)

		const n = 16

		var mu sync.Mutex

		wins := 0

		concurrently(n, func(int) {
			_, ok, err := repo.Consume(ctx, h)
			assert.NoError(t, err)

			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})

		assert.Equal(t, 1, wins)
	})

	t.Run("concurrent issues for one user store one token", func(t *testing.T) {
		uid := userID("issue-race")

		const n = 16

		var mu sync.Mutex

		issuedCount := 0

		concurrently(n, func(i int) {
			ok, err := repo.Issue(ctx, uid, resetHashOf("issue-race"+string(rune('a'+i))), time.Hour, time.Minute)
			assert.NoError(t, err)

			if ok {
				mu.Lock()
				issuedCount++
				mu.Unlock()
			}
		})

		assert.Equal(t, 1, issuedCount, "one mail per user and interval")
		assert.EqualValues(t, 1, count("user_id = ?", uid))
	})

	t.Run("a new token replaces the unused one after the interval", func(t *testing.T) {
		uid, first, second := userID("replace"), resetHashOf("first"), resetHashOf("second")
		ok, err := repo.Issue(ctx, uid, first, time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)

		ok, err = repo.Issue(ctx, uid, second, time.Hour, time.Minute)
		require.NoError(t, err)
		assert.False(t, ok, "inside the interval")

		require.NoError(t, db.Exec("UPDATE password_reset_tokens SET created_at = UTC_TIMESTAMP(3) - INTERVAL 2 MINUTE WHERE user_id = ?", uid).Error)

		ok, err = repo.Issue(ctx, uid, second, time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)

		_, ok, err = repo.Consume(ctx, first)
		require.NoError(t, err)
		assert.False(t, ok, "the earlier token is gone")

		_, ok, err = repo.Consume(ctx, second)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("an expired token cannot be consumed", func(t *testing.T) {
		uid, h := userID("expired"), resetHashOf("expired")
		ok, err := repo.Issue(ctx, uid, h, time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, db.Exec("UPDATE password_reset_tokens SET expires_at = UTC_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE user_id = ?", uid).Error)

		_, ok, err = repo.Consume(ctx, h)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("a duplicate hash is a conflict, not a second row", func(t *testing.T) {
		h := resetHashOf("dup")
		ok, err := repo.Issue(ctx, userID("dup-a"), h, time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)

		_, err = repo.Issue(ctx, userID("dup-b"), h, time.Hour, time.Minute)
		require.Error(t, err)
		assert.EqualValues(t, 1, count("token_hash = ?", h))
	})

	t.Run("an upper case or short hash is refused", func(t *testing.T) {
		_, err := repo.Issue(ctx, userID("bad"), strings.ToUpper(resetHashOf("x")), time.Hour, time.Minute)
		assert.Error(t, err, "the stored form is lower case hex")

		_, err = repo.Issue(ctx, userID("bad"), "abc", time.Hour, time.Minute)
		require.Error(t, err)
	})

	t.Run("cleanup deletes the expired and the long used, in batches", func(t *testing.T) {
		uid := userID("stale")
		for i, set := range []string{
			"expires_at = UTC_TIMESTAMP(3) - INTERVAL 1 HOUR",
			"used_at = UTC_TIMESTAMP(3) - INTERVAL 2 DAY",
			"used_at = UTC_TIMESTAMP(3)", // used just now: kept
			"",                           // unused and valid: kept
		} {
			h := resetHashOf("stale" + string(rune('a'+i)))
			require.NoError(t, db.Create(&models.PasswordResetToken{
				ID: uuid.NewString(), UserID: uid, TokenHash: h,
				ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
			}).Error)

			if set != "" {
				require.NoError(t, db.Exec("UPDATE password_reset_tokens SET "+set+" WHERE token_hash = ?", h).Error)
			}
		}

		n, err := repo.DeleteStale(ctx, 24*time.Hour, 1)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n, "the limit bounds a batch")

		n, err = repo.DeleteStale(ctx, 24*time.Hour, 100)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, int64(1))
		assert.EqualValues(t, 2, count("user_id = ?", uid), "the fresh used and the valid unused tokens stay")

		n, err = repo.DeleteUnusedForUser(ctx, uid)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		assert.EqualValues(t, 1, count("user_id = ?", uid), "a used token is not an unused one")
	})
}

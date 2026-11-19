package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

func newTokenRepo(t *testing.T) (*GORMPasswordResetTokenRepository, *gorm.DB) {
	t.Helper()
	db := openMigratedDB(t)

	return NewGORMPasswordResetTokenRepository(db), db
}

// tokenRow is a stored token without its times (the SQLite driver does not
// parse the strftime text of a datetime(3) column into a time.Time).
type tokenRow struct {
	ID        string
	UserID    string
	TokenHash string
	Used      bool
}

func tokenRows(t *testing.T, db *gorm.DB, userID string) []tokenRow {
	t.Helper()

	var rows []tokenRow
	require.NoError(t, db.Model(&models.PasswordResetToken{}).
		Select("id, user_id, token_hash, used_at IS NOT NULL AS used").
		Where("user_id = ?", userID).Order("token_hash").Scan(&rows).Error)

	return rows
}

// setTimes rewrites the times of a token (the tests cannot wait an hour).
func setTimes(t *testing.T, db *gorm.DB, hash, created, expires string, used any) {
	t.Helper()
	require.NoError(t, db.Exec("UPDATE password_reset_tokens SET created_at = ?, expires_at = ?, used_at = ? WHERE token_hash = ?",
		created, expires, used, hash).Error)
}

func TestPasswordResetTokenIssueAndConsume(t *testing.T) {
	t.Parallel()
	repo, db := newTokenRepo(t)
	ctx := context.Background()
	hash := sha256Hex("raw-token-1")

	issued, err := repo.Issue(ctx, "user-1", hash, time.Hour, time.Minute)
	require.NoError(t, err)
	require.True(t, issued)

	rows := tokenRows(t, db, "user-1")
	require.Len(t, rows, 1)
	assert.Equal(t, hash, rows[0].TokenHash, "only the hash is stored")
	assert.False(t, rows[0].Used)
	assert.NotEmpty(t, rows[0].ID)

	userID, ok, err := repo.Consume(ctx, hash)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "user-1", userID)

	rows = tokenRows(t, db, "user-1")
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Used, "the token is marked used")

	userID, ok, err = repo.Consume(ctx, hash)
	require.NoError(t, err)
	assert.False(t, ok, "a used token cannot be used again")
	assert.Empty(t, userID)
}

func TestPasswordResetTokenConsumeRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(t *testing.T, repo *GORMPasswordResetTokenRepository, db *gorm.DB, hash string)
		hash    string
	}{
		{name: "unknown token", hash: sha256Hex("never-issued"), prepare: func(*testing.T, *GORMPasswordResetTokenRepository, *gorm.DB, string) {}},
		{
			name: "expired token", hash: sha256Hex("expired"),
			prepare: func(t *testing.T, repo *GORMPasswordResetTokenRepository, db *gorm.DB, hash string) {
				issued, err := repo.Issue(context.Background(), "u", hash, time.Hour, time.Minute)
				require.NoError(t, err)
				require.True(t, issued)
				setTimes(t, db, hash, "2000-01-01 00:00:00.000", "2000-01-01 01:00:00.000", nil)
			},
		},
		{
			name: "token that expires now", hash: sha256Hex("edge"),
			prepare: func(t *testing.T, repo *GORMPasswordResetTokenRepository, db *gorm.DB, hash string) {
				issued, err := repo.Issue(context.Background(), "u", hash, time.Hour, time.Minute)
				require.NoError(t, err)
				require.True(t, issued)
				require.NoError(t, db.Exec("UPDATE password_reset_tokens SET expires_at = strftime('%Y-%m-%d %H:%M:%f', 'now') WHERE token_hash = ?", hash).Error)
			},
		},
		{
			name: "used token", hash: sha256Hex("used"),
			prepare: func(t *testing.T, repo *GORMPasswordResetTokenRepository, db *gorm.DB, hash string) {
				issued, err := repo.Issue(context.Background(), "u", hash, time.Hour, time.Minute)
				require.NoError(t, err)
				require.True(t, issued)
				_, ok, err := repo.Consume(context.Background(), hash)
				require.NoError(t, err)
				require.True(t, ok)
			},
		},
		{name: "a hash that is not a digest", hash: "not-a-hash", prepare: func(*testing.T, *GORMPasswordResetTokenRepository, *gorm.DB, string) {}},
		{name: "an empty hash", hash: "", prepare: func(*testing.T, *GORMPasswordResetTokenRepository, *gorm.DB, string) {}},
		{name: "the raw token instead of its hash", hash: "raw-token", prepare: func(t *testing.T, repo *GORMPasswordResetTokenRepository, db *gorm.DB, _ string) {
			issued, err := repo.Issue(context.Background(), "u", sha256Hex("raw-token"), time.Hour, time.Minute)
			require.NoError(t, err)
			require.True(t, issued)
		}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo, db := newTokenRepo(t)
			tt.prepare(t, repo, db, tt.hash)

			userID, ok, err := repo.Consume(context.Background(), tt.hash)
			require.NoError(t, err, "a refusal is not an error")
			assert.False(t, ok)
			assert.Empty(t, userID)
		})
	}
}

func TestPasswordResetTokenIssueValidation(t *testing.T) {
	t.Parallel()
	repo, db := newTokenRepo(t)

	tests := []struct {
		name        string
		user, hash  string
		ttl, minInt time.Duration
	}{
		{name: "no user", hash: sha256Hex("a"), ttl: time.Hour, minInt: time.Minute},
		{name: "short hash", user: "u", hash: "abc", ttl: time.Hour, minInt: time.Minute},
		{name: "raw token instead of a hash", user: "u", hash: "A-raw-token-that-is-long-enough-to-pass-a-length-check-but-is-not-hex-xx", ttl: time.Hour, minInt: time.Minute},
		{name: "no ttl", user: "u", hash: sha256Hex("a"), minInt: time.Minute},
		{name: "negative interval", user: "u", hash: sha256Hex("a"), ttl: time.Hour, minInt: -time.Second},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issued, err := repo.Issue(context.Background(), tt.user, tt.hash, tt.ttl, tt.minInt)
			require.Error(t, err)
			assert.ErrorIs(t, err, dberrors.ErrValidation)
			assert.False(t, issued)
		})
	}

	var n int64
	require.NoError(t, db.Model(&models.PasswordResetToken{}).Count(&n).Error)
	assert.Zero(t, n)
}

func TestPasswordResetTokenMinIntervalAndReplacement(t *testing.T) {
	t.Parallel()
	repo, db := newTokenRepo(t)
	ctx := context.Background()
	first, second, third := sha256Hex("one"), sha256Hex("two"), sha256Hex("three")

	issued, err := repo.Issue(ctx, "user-1", first, time.Hour, time.Minute)
	require.NoError(t, err)
	require.True(t, issued)

	issued, err = repo.Issue(ctx, "user-1", second, time.Hour, time.Minute)
	require.NoError(t, err)
	assert.False(t, issued, "a second request within the interval stores nothing")
	require.Len(t, tokenRows(t, db, "user-1"), 1)

	// Another user is not limited by user-1.
	issued, err = repo.Issue(ctx, "user-2", sha256Hex("other"), time.Hour, time.Minute)
	require.NoError(t, err)
	assert.True(t, issued)

	// The first token is older than the interval now.
	setTimes(t, db, first, "2000-01-01 00:00:00.000", "2099-01-01 00:00:00.000", nil)
	issued, err = repo.Issue(ctx, "user-1", second, time.Hour, time.Minute)
	require.NoError(t, err)
	require.True(t, issued)

	rows := tokenRows(t, db, "user-1")
	require.Len(t, rows, 1, "the earlier unused token is deleted")
	assert.Equal(t, second, rows[0].TokenHash)

	_, ok, err := repo.Consume(ctx, first)
	require.NoError(t, err)
	assert.False(t, ok, "the replaced token is dead")

	// A used token is kept (history) and does not block a new request once
	// the interval has passed.
	_, ok, err = repo.Consume(ctx, second)
	require.NoError(t, err)
	require.True(t, ok)
	setTimes(t, db, second, "2000-01-01 00:00:00.000", "2099-01-01 00:00:00.000", "2000-01-01 00:10:00.000")
	issued, err = repo.Issue(ctx, "user-1", third, time.Hour, time.Minute)
	require.NoError(t, err)
	require.True(t, issued)
	assert.Len(t, tokenRows(t, db, "user-1"), 2, "the used token stays, the new one is added")
}

func TestPasswordResetTokenDuplicateHashIsAConflict(t *testing.T) {
	t.Parallel()
	repo, db := newTokenRepo(t)
	ctx := context.Background()
	hash := sha256Hex("same")

	issued, err := repo.Issue(ctx, "user-1", hash, time.Hour, time.Minute)
	require.NoError(t, err)
	require.True(t, issued)

	_, err = repo.Issue(ctx, "user-2", hash, time.Hour, time.Minute)
	require.Error(t, err)
	assert.ErrorIs(t, err, dberrors.ErrDuplicateKey)
	assert.Empty(t, tokenRows(t, db, "user-2"))
}

func TestPasswordResetTokenDeleteUnusedForUser(t *testing.T) {
	t.Parallel()
	repo, db := newTokenRepo(t)
	ctx := context.Background()

	for _, u := range []string{"user-1", "user-2"} {
		issued, err := repo.Issue(ctx, u, sha256Hex(u), time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, issued)
	}

	n, err := repo.DeleteUnusedForUser(ctx, "user-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Empty(t, tokenRows(t, db, "user-1"))
	assert.Len(t, tokenRows(t, db, "user-2"), 1, "another user's token stays")

	// A used token is not touched.
	_, ok, err := repo.Consume(ctx, sha256Hex("user-2"))
	require.NoError(t, err)
	require.True(t, ok)
	n, err = repo.DeleteUnusedForUser(ctx, "user-2")
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestPasswordResetTokenDeleteStale(t *testing.T) {
	t.Parallel()
	repo, db := newTokenRepo(t)
	ctx := context.Background()

	hashes := map[string]string{}
	for _, name := range []string{"live", "expired", "used-old", "used-recent", "expired-1", "expired-2"} {
		hashes[name] = sha256Hex(name)
		issued, err := repo.Issue(ctx, "user-"+name, hashes[name], time.Hour, time.Minute)
		require.NoError(t, err)
		require.True(t, issued)
	}

	const future, past = "2099-01-01 00:00:00.000", "2000-01-01 00:00:00.000"
	setTimes(t, db, hashes["expired"], past, "2000-01-01 01:00:00.000", nil)
	setTimes(t, db, hashes["expired-1"], past, "2000-01-01 01:00:00.000", nil)
	setTimes(t, db, hashes["expired-2"], past, "2000-01-01 01:00:00.000", nil)
	setTimes(t, db, hashes["used-old"], past, future, "2000-01-01 00:10:00.000")
	require.NoError(t, db.Exec("UPDATE password_reset_tokens SET created_at = ?, expires_at = ?, used_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '-1 hours') WHERE token_hash = ?",
		past, future, hashes["used-recent"]).Error)

	n, err := repo.DeleteStale(ctx, 24*time.Hour, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "the limit bounds one run")

	n, err = repo.DeleteStale(ctx, 24*time.Hour, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "the rest of the stale rows go")

	var left []string
	require.NoError(t, db.Model(&models.PasswordResetToken{}).Order("token_hash").Pluck("token_hash", &left).Error)
	assert.ElementsMatch(t, []string{hashes["live"], hashes["used-recent"]}, left, "a live token and a recently used one stay")

	n, err = repo.DeleteStale(ctx, 24*time.Hour, 100)
	require.NoError(t, err)
	assert.Zero(t, n, "a second run finds nothing")
}

func TestPasswordResetTokenCancelledContext(t *testing.T) {
	t.Parallel()
	repo, _ := newTokenRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.Issue(ctx, "u", sha256Hex("a"), time.Hour, time.Minute)
	assert.ErrorIs(t, err, context.Canceled)
	_, _, err = repo.Consume(ctx, sha256Hex("a"))
	assert.ErrorIs(t, err, context.Canceled)
	_, err = repo.DeleteUnusedForUser(ctx, "u")
	assert.ErrorIs(t, err, context.Canceled)
	_, err = repo.DeleteStale(ctx, time.Hour, 10)
	assert.ErrorIs(t, err, context.Canceled)
}

// Several confirms with one token: exactly one wins (SQLite serializes the
// statements; the MySQL test runs them in parallel with a barrier).
func TestPasswordResetTokenConsumeIsSingleUse(t *testing.T) {
	t.Parallel()
	repo, _ := newTokenRepo(t)
	ctx := context.Background()
	hash := sha256Hex("race")

	issued, err := repo.Issue(ctx, "user-1", hash, time.Hour, time.Minute)
	require.NoError(t, err)
	require.True(t, issued)

	var wins atomic.Int32

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, ok, err := repo.Consume(ctx, hash); err == nil && ok {
				wins.Add(1)
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())
}

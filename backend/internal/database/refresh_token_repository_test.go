package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashOf returns the SHA-256 hex digest of s, the stored form of a token.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func setupRefreshTokenRepo(t *testing.T) *GORMRefreshTokenRepository {
	t.Helper()
	return NewGORMRefreshTokenRepository(openMigratedDB(t))
}

// seedRefreshToken stores a token with sensible defaults; mutate adjusts it.
func seedRefreshToken(t *testing.T, repo *GORMRefreshTokenRepository, id, familyID string, mutate func(*models.RefreshToken)) {
	t.Helper()
	now := time.Now().UTC()
	rt := &models.RefreshToken{
		ID:               id,
		UserID:           "u1",
		FamilyID:         familyID,
		TokenHash:        hashOf(id),
		ExpiresAt:        now.Add(time.Hour),
		LastActivity:     now.Add(-10 * time.Minute),
		CreatedAt:        now.Add(-10 * time.Minute),
		SessionStartedAt: now.Add(-10 * time.Minute),
	}
	if mutate != nil {
		mutate(rt)
	}
	require.NoError(t, repo.Create(context.Background(), rt))
}

func loadRefreshToken(t *testing.T, repo *GORMRefreshTokenRepository, id string) models.RefreshToken {
	t.Helper()
	var rt models.RefreshToken
	require.NoError(t, repo.DB.Where("id = ?", id).First(&rt).Error)
	return rt
}

func TestRefreshTokenRepositoryCreateAndFind(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()

	rt := &models.RefreshToken{UserID: "u1", TokenHash: hashOf("raw"), ExpiresAt: now.Add(time.Hour), LastActivity: now}
	require.NoError(t, repo.Create(ctx, rt))
	assert.NotEmpty(t, rt.ID, "the repository sets the ID")
	assert.False(t, rt.SessionStartedAt.IsZero(), "MySQL refuses a zero datetime")
	assert.Equal(t, rt.CreatedAt, rt.SessionStartedAt, "a new session starts at creation")

	got, err := repo.FindByTokenHash(ctx, hashOf("raw"))
	require.NoError(t, err)
	assert.Equal(t, rt.ID, got.ID)

	_, err = repo.FindByTokenHash(ctx, hashOf("other"))
	require.ErrorIs(t, err, dberrors.ErrNotFound)

	dup := &models.RefreshToken{UserID: "u2", TokenHash: hashOf("raw"), ExpiresAt: now.Add(time.Hour), LastActivity: now}
	require.ErrorIs(t, repo.Create(ctx, dup), dberrors.ErrDuplicateKey)

	plain := &models.RefreshToken{UserID: "u1", TokenHash: "raw-token", ExpiresAt: now.Add(time.Hour), LastActivity: now}
	require.ErrorIs(t, repo.Create(ctx, plain), dberrors.ErrValidation, "a plaintext token is refused")
}

func TestRefreshTokenRepositoryMarkRotatedIfActive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		revoked      bool
		wantAffected int64
		wantRotated  bool
	}{
		{name: "active token is revoked and stamped", wantAffected: 1, wantRotated: true},
		{name: "revoked token is left alone", revoked: true, wantAffected: 0, wantRotated: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupRefreshTokenRepo(t)
			seedRefreshToken(t, repo, "rt-1", "rt-1", func(rt *models.RefreshToken) { rt.Revoked = tt.revoked })

			at := time.Now().UTC().Truncate(time.Second)
			affected, err := repo.MarkRotatedIfActive(context.Background(), "rt-1", at)
			require.NoError(t, err)
			assert.Equal(t, tt.wantAffected, affected)

			got := loadRefreshToken(t, repo, "rt-1")
			assert.True(t, got.Revoked)
			if tt.wantRotated {
				require.NotNil(t, got.RotatedAt)
				assert.WithinDuration(t, at, *got.RotatedAt, time.Second)
			} else {
				assert.Nil(t, got.RotatedAt, "a revoked token must not get rotated_at")
			}

			// A second consumption never succeeds.
			affected, err = repo.MarkRotatedIfActive(context.Background(), "rt-1", at)
			require.NoError(t, err)
			assert.Equal(t, int64(0), affected)
		})
	}
}

func TestRefreshTokenRepositoryRevoke(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		run         func(r *GORMRefreshTokenRepository) error
		wantRevoked []string
	}{
		{
			name:        "by ID",
			run:         func(r *GORMRefreshTokenRepository) error { return r.RevokeByID(context.Background(), "first") },
			wantRevoked: []string{"first"},
		},
		{
			name:        "family, including a first token without a family",
			run:         func(r *GORMRefreshTokenRepository) error { return r.RevokeFamily(context.Background(), "first") },
			wantRevoked: []string{"first", "succ"},
		},
		{
			name: "family of another user is left alone",
			run: func(r *GORMRefreshTokenRepository) error {
				return r.RevokeFamilyForUser(context.Background(), "foreign", "u1")
			},
			wantRevoked: []string{},
		},
		{
			name: "family of the user",
			run: func(r *GORMRefreshTokenRepository) error {
				return r.RevokeFamilyForUser(context.Background(), "first", "u1")
			},
			wantRevoked: []string{"first", "succ"},
		},
		{
			name:        "all of the user",
			run:         func(r *GORMRefreshTokenRepository) error { return r.RevokeAllForUser(context.Background(), "u1") },
			wantRevoked: []string{"first", "succ", "other"},
		},
		{
			name: "all of the user except one",
			run: func(r *GORMRefreshTokenRepository) error {
				return r.RevokeAllForUserExcept(context.Background(), "u1", "other")
			},
			wantRevoked: []string{"first", "succ"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupRefreshTokenRepo(t)
			seedRefreshToken(t, repo, "first", "", nil)
			seedRefreshToken(t, repo, "succ", "first", nil)
			seedRefreshToken(t, repo, "other", "other", nil)
			seedRefreshToken(t, repo, "foreign", "foreign", func(rt *models.RefreshToken) { rt.UserID = "u2" })

			require.NoError(t, tt.run(repo))

			revoked := map[string]bool{}
			for _, id := range tt.wantRevoked {
				revoked[id] = true
			}
			for _, id := range []string{"first", "succ", "other", "foreign"} {
				got := loadRefreshToken(t, repo, id)
				assert.Equal(t, revoked[id], got.Revoked, "%s revoked", id)
				assert.Nil(t, got.RotatedAt, "%s: only a rotation sets rotated_at", id)
			}
		})
	}
}

func TestRefreshTokenRepositoryListRecentFamiliesForUser(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)
	now := time.Now().UTC()
	since := now.Add(-10 * time.Minute)

	// fam-a: three rows, one family; the revoked row is included.
	seedRefreshToken(t, repo, "fam-a", "fam-a", func(rt *models.RefreshToken) {
		rt.Revoked = true
		rt.CreatedAt = now.Add(-50 * time.Minute)
	})
	seedRefreshToken(t, repo, "fam-a-2", "fam-a", func(rt *models.RefreshToken) { rt.CreatedAt = now.Add(-40 * time.Minute) })
	seedRefreshToken(t, repo, "fam-a-3", "fam-a", func(rt *models.RefreshToken) { rt.CreatedAt = now.Add(-30 * time.Minute) })
	// fam-b: revoked, expiry still ahead: included.
	seedRefreshToken(t, repo, "fam-b", "fam-b", func(rt *models.RefreshToken) {
		rt.Revoked = true
		rt.CreatedAt = now.Add(-20 * time.Minute)
	})
	// A token without a family is its own family; the newest family comes first.
	seedRefreshToken(t, repo, "solo", "", func(rt *models.RefreshToken) { rt.CreatedAt = now.Add(-5 * time.Minute) })
	// Expired before since: excluded. Expired after since: included.
	seedRefreshToken(t, repo, "old-1", "old-1", func(rt *models.RefreshToken) { rt.ExpiresAt = since.Add(-time.Minute) })
	seedRefreshToken(t, repo, "just-expired", "just-expired", func(rt *models.RefreshToken) {
		rt.ExpiresAt = since.Add(time.Minute)
		rt.CreatedAt = now.Add(-60 * time.Minute)
	})
	seedRefreshToken(t, repo, "other-user", "other-user", func(rt *models.RefreshToken) { rt.UserID = "u2" })

	got, err := repo.ListRecentFamiliesForUser(context.Background(), "u1", since)
	require.NoError(t, err)
	assert.Equal(t, []string{"solo", "fam-b", "fam-a", "just-expired"}, got, "newest family first")

	got, err = repo.ListRecentFamiliesForUser(context.Background(), "nobody", since)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestRefreshTokenRepositoryListRecentFamiliesForUserLimit(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)
	now := time.Now().UTC()
	rows := make([]models.RefreshToken, 0, models.MaxRecentFamilies+5)
	for i := 0; i < models.MaxRecentFamilies+5; i++ {
		id := fmt.Sprintf("f-%04d", i)
		rows = append(rows, models.RefreshToken{
			ID: id, UserID: "u1", FamilyID: id, TokenHash: hashOf(id),
			ExpiresAt: now.Add(time.Hour), LastActivity: now, SessionStartedAt: now,
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
	}
	require.NoError(t, repo.DB.CreateInBatches(&rows, 200).Error)

	got, err := repo.ListRecentFamiliesForUser(context.Background(), "u1", now)
	require.NoError(t, err)
	require.Len(t, got, models.MaxRecentFamilies)
	assert.Equal(t, fmt.Sprintf("f-%04d", models.MaxRecentFamilies+4), got[0], "the newest families are kept")
}

func TestRefreshTokenRepositoryCounts(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)

	seedRefreshToken(t, repo, "fam-a", "fam-a", func(rt *models.RefreshToken) { rt.Revoked = true })
	seedRefreshToken(t, repo, "fam-a-2", "fam-a", nil)
	seedRefreshToken(t, repo, "fam-a-3", "fam-a", func(rt *models.RefreshToken) { rt.ExpiresAt = time.Now().UTC().Add(-time.Minute) })
	seedRefreshToken(t, repo, "fam-b", "fam-b", nil)
	seedRefreshToken(t, repo, "u2-a", "u2-a", func(rt *models.RefreshToken) { rt.UserID = "u2" })

	tests := []struct {
		name string
		run  func() (int64, error)
		want int64
	}{
		{name: "family a", run: func() (int64, error) { return repo.CountActiveInFamily(context.Background(), "fam-a") }, want: 1},
		{name: "family b", run: func() (int64, error) { return repo.CountActiveInFamily(context.Background(), "fam-b") }, want: 1},
		{name: "missing family", run: func() (int64, error) { return repo.CountActiveInFamily(context.Background(), "missing") }, want: 0},
		{name: "user u1", run: func() (int64, error) { return repo.CountActiveForUser(context.Background(), "u1") }, want: 2},
		{name: "user u2", run: func() (int64, error) { return repo.CountActiveForUser(context.Background(), "u2") }, want: 1},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.run()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRefreshTokenRepositoryTouchFamily(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedRefreshToken(t, repo, "active", "fam", nil)
	seedRefreshToken(t, repo, "revoked", "fam", func(rt *models.RefreshToken) { rt.Revoked = true })
	seedRefreshToken(t, repo, "other", "fam-other", nil)
	seedRefreshToken(t, repo, "newer", "fam-newer", func(rt *models.RefreshToken) { rt.LastActivity = now.Add(time.Minute) })

	require.NoError(t, repo.TouchFamily(context.Background(), "fam", now))
	require.NoError(t, repo.TouchFamily(context.Background(), "fam-newer", now))

	assert.WithinDuration(t, now, loadRefreshToken(t, repo, "active").LastActivity, time.Second)
	assert.True(t, loadRefreshToken(t, repo, "revoked").LastActivity.Before(now.Add(-time.Minute)), "revoked token must not be touched")
	assert.True(t, loadRefreshToken(t, repo, "other").LastActivity.Before(now.Add(-time.Minute)), "other family must not be touched")
	assert.WithinDuration(t, now.Add(time.Minute), loadRefreshToken(t, repo, "newer").LastActivity, time.Second,
		"TouchFamily must never move last_activity backwards")
}

// A token created without a family is its own family: Create stores its ID
// as family_id, so every family method (TouchFamily included) finds it.
func TestRefreshTokenRepositoryCreateDefaultsFamilyToID(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedRefreshToken(t, repo, "solo", "", nil)
	stored := loadRefreshToken(t, repo, "solo")
	assert.Equal(t, "solo", stored.FamilyID, "Create sets family_id to the ID")

	require.NoError(t, repo.TouchFamily(ctx, "solo", now))
	assert.WithinDuration(t, now, loadRefreshToken(t, repo, "solo").LastActivity, time.Second, "TouchFamily finds the token")

	n, err := repo.CountActiveInFamily(ctx, "solo")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	// A row written before this rule (empty family_id) is still its own
	// family for TouchFamily, like for the other family methods.
	seedRefreshToken(t, repo, "legacy", "x", nil)
	require.NoError(t, repo.DB.Model(&models.RefreshToken{}).Where("id = ?", "legacy").Update("family_id", "").Error)
	require.NoError(t, repo.TouchFamily(ctx, "legacy", now))
	assert.WithinDuration(t, now, loadRefreshToken(t, repo, "legacy").LastActivity, time.Second, "TouchFamily matches a legacy row by ID")
}

// ClaimReuseGrace succeeds once, only for a token that a rotation consumed
// inside the window, also when many requests claim at the same time.
func TestRefreshTokenRepositoryClaimReuseGrace(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-30 * time.Second)
	rotated := func(at time.Time) func(*models.RefreshToken) {
		return func(rt *models.RefreshToken) { rt.Revoked = true; rt.RotatedAt = &at }
	}
	seedRefreshToken(t, repo, "fresh", "f1", rotated(now.Add(-5*time.Second)))
	seedRefreshToken(t, repo, "stale", "f2", rotated(now.Add(-time.Minute)))
	seedRefreshToken(t, repo, "logged-out", "f3", func(rt *models.RefreshToken) { rt.Revoked = true })
	seedRefreshToken(t, repo, "active", "f4", nil)
	seedRefreshToken(t, repo, "raced", "f5", rotated(now.Add(-time.Second)))

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "rotated inside the window", id: "fresh", want: true},
		{name: "second claim of the same token", id: "fresh", want: false},
		{name: "rotated before the window", id: "stale", want: false},
		{name: "revoked by logout (no rotation)", id: "logged-out", want: false},
		{name: "not consumed", id: "active", want: false},
		{name: "unknown token", id: "missing", want: false},
	}
	for _, tt := range tests { // sequential: the second row depends on the first
		got, err := repo.ClaimReuseGrace(ctx, tt.id, since)
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.want, got, tt.name)
	}
	assert.Nil(t, loadRefreshToken(t, repo, "fresh").RotatedAt, "the grace is spent")
	assert.True(t, loadRefreshToken(t, repo, "fresh").Revoked, "the token stays consumed")

	const claimers = 8
	var wins atomicCounter
	var wg sync.WaitGroup
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.ClaimReuseGrace(ctx, "raced", since)
			assert.NoError(t, err)
			if ok {
				wins.add()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins.get(), "exactly one concurrent claim wins")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := repo.ClaimReuseGrace(cancelled, "raced", since)
	require.ErrorIs(t, err, context.Canceled)
}

// atomicCounter counts concurrent wins.
type atomicCounter struct {
	n  int
	mu sync.Mutex
}

func (c *atomicCounter) add() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestRefreshTokenRepositoryRevokeOldestForUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		keep        int
		exclude     string
		wantRevoked []string
		wantCount   int64
	}{
		{name: "keep two newest", keep: 2, wantRevoked: []string{"t1", "t2"}, wantCount: 2},
		{name: "keep two, excluded one stays", keep: 2, exclude: "t1", wantRevoked: []string{"t2"}, wantCount: 1},
		{name: "keep more than exist", keep: 10, wantRevoked: []string{}, wantCount: 0},
		{name: "negative keep revokes all", keep: -1, wantRevoked: []string{"t1", "t2", "t3", "t4"}, wantCount: 4},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupRefreshTokenRepo(t)
			now := time.Now().UTC()
			for i, id := range []string{"t1", "t2", "t3", "t4"} {
				seedRefreshToken(t, repo, id, id, func(rt *models.RefreshToken) { rt.CreatedAt = now.Add(time.Duration(i-10) * time.Minute) })
			}
			seedRefreshToken(t, repo, "expired", "expired", func(rt *models.RefreshToken) {
				rt.CreatedAt = now.Add(-time.Hour)
				rt.ExpiresAt = now.Add(-time.Minute)
			})
			seedRefreshToken(t, repo, "foreign", "foreign", func(rt *models.RefreshToken) { rt.UserID = "u2" })

			n, err := repo.RevokeOldestForUser(context.Background(), "u1", tt.keep, tt.exclude)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCount, n)

			want := map[string]bool{}
			for _, id := range tt.wantRevoked {
				want[id] = true
			}
			for _, id := range []string{"t1", "t2", "t3", "t4", "expired", "foreign"} {
				assert.Equal(t, want[id], loadRefreshToken(t, repo, id).Revoked, "%s revoked", id)
			}
		})
	}
}

func TestRefreshTokenRepositoryDeleteExpired(t *testing.T) {
	t.Parallel()
	repo := setupRefreshTokenRepo(t)
	now := time.Now().UTC()
	seedRefreshToken(t, repo, "old", "old", func(rt *models.RefreshToken) { rt.ExpiresAt = now.Add(-2 * time.Hour) })
	seedRefreshToken(t, repo, "recent", "recent", func(rt *models.RefreshToken) { rt.ExpiresAt = now.Add(-time.Minute) })
	seedRefreshToken(t, repo, "live", "live", nil)

	n, err := repo.DeleteExpired(context.Background(), now.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	var ids []string
	require.NoError(t, repo.DB.Model(&models.RefreshToken{}).Order("id").Pluck("id", &ids).Error)
	assert.Equal(t, []string{"live", "recent"}, ids)
}

func TestRefreshTokenRepositoryWithTx(t *testing.T) {
	t.Parallel()

	t.Run("commits", func(t *testing.T) {
		t.Parallel()
		repo := setupRefreshTokenRepo(t)
		seedRefreshToken(t, repo, "old", "fam", nil)

		err := repo.WithTx(context.Background(), func(tx models.RefreshTokenRepository) error {
			if _, err := tx.MarkRotatedIfActive(context.Background(), "old", time.Now().UTC()); err != nil {
				return err
			}
			now := time.Now().UTC()
			return tx.Create(context.Background(), &models.RefreshToken{ID: "new", UserID: "u1", FamilyID: "fam", TokenHash: hashOf("new"), ExpiresAt: now.Add(time.Hour), LastActivity: now})
		})

		require.NoError(t, err)
		assert.True(t, loadRefreshToken(t, repo, "old").Revoked)
		assert.False(t, loadRefreshToken(t, repo, "new").Revoked)
	})

	t.Run("rolls back on error", func(t *testing.T) {
		t.Parallel()
		repo := setupRefreshTokenRepo(t)
		seedRefreshToken(t, repo, "old", "fam", nil)
		errBoom := errors.New("boom")

		err := repo.WithTx(context.Background(), func(tx models.RefreshTokenRepository) error {
			if err := tx.RevokeByID(context.Background(), "old"); err != nil {
				return err
			}
			return errBoom
		})

		require.ErrorIs(t, err, errBoom)
		assert.False(t, loadRefreshToken(t, repo, "old").Revoked, "the revoke is rolled back")
	})

	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		repo := setupRefreshTokenRepo(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := repo.WithTx(ctx, func(models.RefreshTokenRepository) error { return nil })

		require.ErrorIs(t, err, context.Canceled)
	})
}

package database

import (
	"context"
	"fmt"
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

func setupAPIKeyRepo(t *testing.T) *GORMAPIKeyRepository {
	t.Helper()
	return NewGORMAPIKeyRepository(openMigratedDB(t))
}

// newAPIKey returns a valid key of userID with a hash and prefix derived from id.
func newAPIKey(id, userID string) *models.APIKey {
	h := hashOf(id)
	return &models.APIKey{ID: id, UserID: userID, Name: "key " + id, KeyHash: h, Prefix: h[:models.APIKeyPrefixLen]}
}

func TestAPIKeyRepositoryCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     models.APIKey
		wantErr error
	}{
		{name: "success", key: *newAPIKey("", "u1")},
		{name: "no user", key: models.APIKey{Name: "n", KeyHash: hashOf("x"), Prefix: "p"}, wantErr: dberrors.ErrValidation},
		{name: "no name", key: models.APIKey{UserID: "u1", KeyHash: hashOf("x"), Prefix: "p"}, wantErr: dberrors.ErrValidation},
		{name: "plaintext key", key: models.APIKey{UserID: "u1", Name: "n", KeyHash: "raw", Prefix: "p"}, wantErr: dberrors.ErrValidation},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupAPIKeyRepo(t)
			k := tt.key
			err := repo.Create(context.Background(), &k)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, k.ID)
			assert.False(t, k.CreatedAt.IsZero())
		})
	}
}

func TestAPIKeyRepositoryFind(t *testing.T) {
	t.Parallel()
	repo := setupAPIKeyRepo(t)
	ctx := context.Background()
	k1 := newAPIKey("k1", "u1")
	k2 := newAPIKey("k2", "u2")
	require.NoError(t, repo.Create(ctx, k1))
	require.NoError(t, repo.Create(ctx, k2))

	t.Run("by ID of the owner", func(t *testing.T) {
		t.Parallel()
		got, err := repo.FindByID(ctx, "u1", "k1")
		require.NoError(t, err)
		assert.Equal(t, k1.KeyHash, got.KeyHash)

		_, err = repo.FindByID(ctx, "u2", "k1")
		require.ErrorIs(t, err, dberrors.ErrNotFound, "a key of another user is not found")
	})

	t.Run("by prefix", func(t *testing.T) {
		t.Parallel()
		got, err := repo.FindByPrefix(ctx, k2.Prefix)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "k2", got[0].ID)

		_, err = repo.FindByPrefix(ctx, "0000000000000000")
		require.ErrorIs(t, err, dberrors.ErrNotFound)
	})

	t.Run("by IDs", func(t *testing.T) {
		t.Parallel()
		got, err := repo.FindByIDs(ctx, []string{"k1", "k2", "missing"})
		require.NoError(t, err)
		assert.Len(t, got, 2)

		none, err := repo.FindByIDs(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, none)
	})
}

func TestAPIKeyRepositoryFindByIDsInChunks(t *testing.T) {
	t.Parallel()
	repo := setupAPIKeyRepo(t)
	n := apiKeyIDsPerQuery + 3
	keys := make([]models.APIKey, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k := newAPIKey(fmt.Sprintf("k-%04d", i), "u1")
		k.CreatedAt = time.Now().UTC()
		keys = append(keys, *k)
		ids = append(ids, k.ID)
	}
	require.NoError(t, repo.DB.CreateInBatches(&keys, 200).Error)

	got, err := repo.FindByIDs(context.Background(), ids)

	require.NoError(t, err)
	assert.Len(t, got, n)
}

func TestAPIKeyRepositoryListByUser(t *testing.T) {
	t.Parallel()
	repo := setupAPIKeyRepo(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		require.NoError(t, repo.Create(ctx, newAPIKey(id, "u1")))
		time.Sleep(2 * time.Millisecond)
	}
	require.NoError(t, repo.Create(ctx, newAPIKey("c", "u2")))

	got, err := repo.ListByUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].ID, "newest first")

	none, err := repo.ListByUser(ctx, "nobody")
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestAPIKeyRepositoryUpdateLastUsed(t *testing.T) {
	t.Parallel()
	repo := setupAPIKeyRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, newAPIKey("k1", "u1")))
	at := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, repo.UpdateLastUsed(ctx, "u1", "k1", at))
	got, err := repo.FindByID(ctx, "u1", "k1")
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt)
	assert.WithinDuration(t, at, *got.LastUsedAt, time.Second)

	require.ErrorIs(t, repo.UpdateLastUsed(ctx, "u2", "k1", at), dberrors.ErrNotFound)
	require.ErrorIs(t, repo.UpdateLastUsed(ctx, "u1", "missing", at), dberrors.ErrNotFound)
}

// TestAPIKeyRepositoryUpdateLastUsedSameValue: an update that changes no row
// (MySQL reports 0 affected rows for the same millisecond) is a success when
// the key exists. It sits on the authentication path.
func TestAPIKeyRepositoryUpdateLastUsedSameValue(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	repo := NewGORMAPIKeyRepository(db)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, newAPIKey("k1", "u1")))
	at := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, repo.UpdateLastUsed(ctx, "u1", "k1", at))

	// Force the zero-rows case regardless of the driver: a callback that
	// reports no affected rows for the update.
	require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:zero_rows", func(tx *gorm.DB) {
		if tx.Statement.Table == "api_keys" {
			tx.RowsAffected = 0
		}
	}))
	require.NoError(t, repo.UpdateLastUsed(ctx, "u1", "k1", at), "an existing key whose row did not change is not an error")
	require.ErrorIs(t, repo.UpdateLastUsed(ctx, "u1", "missing", at), dberrors.ErrNotFound)
	require.ErrorIs(t, repo.UpdateLastUsed(ctx, "u2", "k1", at), dberrors.ErrNotFound, "a key of another user is still not found")
}

func TestAPIKeyRepositoryListPaged(t *testing.T) {
	t.Parallel()
	repo := setupAPIKeyRepo(t)
	ctx := context.Background()
	for _, k := range []*models.APIKey{newAPIKey("a", "u1"), newAPIKey("b", "u1"), newAPIKey("c", "u1"), newAPIKey("d", "u2")} {
		require.NoError(t, repo.Create(ctx, k))
		time.Sleep(2 * time.Millisecond)
	}

	all, total, err := repo.ListPaged(ctx, models.APIKeyFilter{}, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(4), total)
	require.Len(t, all, 2)
	assert.Equal(t, "d", all[0].ID, "newest first")
	for _, k := range all {
		assert.Empty(t, k.KeyHash, "the page query does not read the hash")
	}

	page2, _, err := repo.ListPaged(ctx, models.APIKeyFilter{}, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.Equal(t, "b", page2[0].ID)

	own, total, err := repo.ListPaged(ctx, models.APIKeyFilter{UserID: "u1"}, 25, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Len(t, own, 3)
	for _, k := range own {
		assert.Equal(t, "u1", k.UserID)
	}

	none, total, err := repo.ListPaged(ctx, models.APIKeyFilter{UserID: "nobody"}, 25, 0)
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.NotNil(t, none)
	assert.Empty(t, none)

	_, _, err = repo.ListPaged(ctx, models.APIKeyFilter{}, 25, 0)
	require.NoError(t, err)

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = repo.ListPaged(cctx, models.APIKeyFilter{}, 25, 0)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAPIKeyRepositoryCreateLimited(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	repo := NewGORMAPIKeyRepository(db)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, id := range []string{"u1", "u2"} {
		require.NoError(t, db.Create(&models.User{ID: id, Username: id, Role: models.RoleUser, AuthProvider: models.AuthProviderLocal}).Error)
	}
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	withExpiry := func(id, user string, exp *time.Time) *models.APIKey {
		k := newAPIKey(id, user)
		k.ExpiresAt = exp
		return k
	}

	require.NoError(t, repo.CreateLimited(ctx, withExpiry("e1", "u1", &past), 2, now), "an expired key does not count")
	require.NoError(t, repo.CreateLimited(ctx, withExpiry("a1", "u1", &future), 2, now))
	require.NoError(t, repo.CreateLimited(ctx, withExpiry("a2", "u1", &future), 2, now))
	err := repo.CreateLimited(ctx, withExpiry("a3", "u1", &future), 2, now)
	require.ErrorIs(t, err, models.ErrTooManyAPIKeys)
	require.NoError(t, repo.CreateLimited(ctx, withExpiry("b1", "u2", &future), 2, now), "the limit is per user")

	require.NoError(t, repo.Delete(ctx, "u1", "a1"))
	require.NoError(t, repo.CreateLimited(ctx, withExpiry("a4", "u1", &future), 2, now), "deleting a key frees a slot")

	t.Run("unknown owner", func(t *testing.T) {
		t.Parallel()
		err := repo.CreateLimited(ctx, withExpiry("z1", "ghost", &future), 2, now)
		require.ErrorIs(t, err, dberrors.ErrNotFound, "a key is never created for a user that does not exist")
	})
	t.Run("invalid key", func(t *testing.T) {
		t.Parallel()
		k := newAPIKey("v1", "u2")
		k.KeyHash = "raw"
		require.ErrorIs(t, repo.CreateLimited(ctx, k, 2, now), dberrors.ErrValidation)
	})
	t.Run("no limit given", func(t *testing.T) {
		t.Parallel()
		require.ErrorIs(t, repo.CreateLimited(ctx, withExpiry("z2", "u2", &future), 0, now), dberrors.ErrValidation)
	})
	t.Run("concurrent creates keep the limit", func(t *testing.T) {
		t.Parallel()
		var wg sync.WaitGroup
		var okCount atomic.Int32
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				k := withExpiry(fmt.Sprintf("c%d", i), "u2", &future)
				if err := repo.CreateLimited(ctx, k, 3, now); err == nil {
					okCount.Add(1)
				}
			}(i)
		}
		wg.Wait()
		n, err := repo.ListByUser(ctx, "u2")
		require.NoError(t, err)
		assert.Len(t, n, 3, "b1 plus two of the six")
		assert.Equal(t, int32(2), okCount.Load())
	})
}

func TestAPIKeyRepositoryDelete(t *testing.T) {
	t.Parallel()
	repo := setupAPIKeyRepo(t)
	ctx := context.Background()
	for _, k := range []*models.APIKey{newAPIKey("k1", "u1"), newAPIKey("k2", "u1"), newAPIKey("k3", "u2")} {
		require.NoError(t, repo.Create(ctx, k))
	}

	require.ErrorIs(t, repo.Delete(ctx, "u2", "k1"), dberrors.ErrNotFound, "only the owner's key is deleted")
	require.NoError(t, repo.Delete(ctx, "u1", "k1"))
	require.ErrorIs(t, repo.Delete(ctx, "u1", "k1"), dberrors.ErrNotFound)

	n, err := repo.DeleteAllForUser(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	n, err = repo.DeleteAllForUser(ctx, "u1")
	require.NoError(t, err)
	assert.Zero(t, n, "no keys is not an error")
	_, err = repo.DeleteAllForUser(ctx, "")
	require.ErrorIs(t, err, dberrors.ErrValidation)

	left, err := repo.ListByUser(ctx, "u2")
	require.NoError(t, err)
	assert.Len(t, left, 1)
}

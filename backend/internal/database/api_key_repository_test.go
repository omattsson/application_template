package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

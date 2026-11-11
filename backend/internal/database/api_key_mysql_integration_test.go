//go:build integration

package database

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPIKeyRepositoryMySQL runs the API key operations that depend on the
// server: the zero-affected-rows UpdateLastUsed, the paged list, and the
// create limit under concurrent creates (it locks the owner row).
func TestAPIKeyRepositoryMySQL(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	ctx := context.Background()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	users := NewGORMUserRepository(db.DB)
	owner := &models.User{Username: "it-key-owner-" + suffix, Role: models.RoleUser, PasswordHash: "hash"}
	require.NoError(t, users.Create(ctx, owner))
	t.Cleanup(func() {
		db.Where("user_id = ?", owner.ID).Delete(&models.APIKey{})
		db.Where("id = ?", owner.ID).Delete(&models.User{})
	})
	keys := NewGORMAPIKeyRepository(db.DB)
	newKey := func(id string, exp *time.Time) *models.APIKey {
		h := hashOf(id + suffix)
		return &models.APIKey{ID: id + suffix, UserID: owner.ID, Name: id, KeyHash: h, Prefix: h[:models.APIKeyPrefixLen], ExpiresAt: exp}
	}

	t.Run("UpdateLastUsed with an unchanged value is a success", func(t *testing.T) {
		require.NoError(t, keys.Create(ctx, newKey("lu", nil)))
		at := time.Now().UTC().Truncate(time.Second)
		require.NoError(t, keys.UpdateLastUsed(ctx, owner.ID, "lu"+suffix, at))
		// MySQL reports 0 affected rows for the same value.
		require.NoError(t, keys.UpdateLastUsed(ctx, owner.ID, "lu"+suffix, at))
		require.ErrorIs(t, keys.UpdateLastUsed(ctx, owner.ID, "missing"+suffix, at), dberrors.ErrNotFound)
		require.ErrorIs(t, keys.UpdateLastUsed(ctx, "other-user", "lu"+suffix, at), dberrors.ErrNotFound)
	})

	t.Run("ListPaged", func(t *testing.T) {
		page, total, err := keys.ListPaged(ctx, models.APIKeyFilter{UserID: owner.ID}, 1, 0)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, total, int64(1))
		require.Len(t, page, 1)
		assert.Empty(t, page[0].KeyHash, "the hash is not read")
	})

	t.Run("CreateLimited under concurrent creates", func(t *testing.T) {
		require.NoError(t, db.Where("user_id = ?", owner.ID).Delete(&models.APIKey{}).Error)
		future := time.Now().Add(time.Hour)
		var wg sync.WaitGroup
		var created, limited atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := keys.CreateLimited(ctx, newKey(fmt.Sprintf("c%d", i), &future), 3, time.Now())
				switch {
				case err == nil:
					created.Add(1)
				case err == models.ErrTooManyAPIKeys:
					limited.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(i)
		}
		wg.Wait()
		assert.Equal(t, int32(3), created.Load())
		assert.Equal(t, int32(5), limited.Load())
		list, err := keys.ListByUser(ctx, owner.ID)
		require.NoError(t, err)
		assert.Len(t, list, 3)

		err = keys.CreateLimited(ctx, &models.APIKey{UserID: "no-such-user-" + suffix, Name: "x", KeyHash: hashOf("x"), Prefix: "p"}, 3, time.Now())
		require.ErrorIs(t, err, dberrors.ErrNotFound)
	})
}

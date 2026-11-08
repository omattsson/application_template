package database

import (
	"context"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Compile-time interface check.
var _ models.APIKeyRepository = (*GORMAPIKeyRepository)(nil)

// apiKeyIDsPerQuery bounds the IN list of one FindByIDs query.
const apiKeyIDsPerQuery = 500

// GORMAPIKeyRepository implements models.APIKeyRepository using GORM.
type GORMAPIKeyRepository struct {
	*BaseRepository
}

// NewGORMAPIKeyRepository creates a new GORM-backed API key repository.
func NewGORMAPIKeyRepository(db *gorm.DB) *GORMAPIKeyRepository {
	return &GORMAPIKeyRepository{BaseRepository: NewBaseRepository(db)}
}

// Create validates and inserts an API key. See models.APIKeyRepository.
func (r *GORMAPIKeyRepository) Create(ctx context.Context, key *models.APIKey) error {
	if err := key.Validate(); err != nil {
		return validationError("create", err)
	}
	if key.ID == "" {
		key.ID = uuid.New().String()
	}
	key.CreatedAt = time.Now().UTC()
	err := r.Write(ctx, func(db *gorm.DB) error { return db.Create(key).Error })
	return repoError(ctx, "create", err)
}

// FindByID returns an API key by user ID and key ID.
func (r *GORMAPIKeyRepository) FindByID(ctx context.Context, userID, keyID string) (*models.APIKey, error) {
	var key models.APIKey
	if err := r.DB.WithContext(ctx).Where("user_id = ? AND id = ?", userID, keyID).Take(&key).Error; err != nil {
		return nil, repoError(ctx, "find_by_id", err)
	}
	return &key, nil
}

// FindByIDs returns the API keys with these IDs that exist, in chunks of
// apiKeyIDsPerQuery IDs, so a revalidation of many sockets needs few queries.
func (r *GORMAPIKeyRepository) FindByIDs(ctx context.Context, keyIDs []string) ([]*models.APIKey, error) {
	var out []*models.APIKey
	for start := 0; start < len(keyIDs); start += apiKeyIDsPerQuery {
		end := min(start+apiKeyIDsPerQuery, len(keyIDs))
		var keys []*models.APIKey
		if err := r.DB.WithContext(ctx).Where("id IN ?", keyIDs[start:end]).Find(&keys).Error; err != nil {
			return nil, repoError(ctx, "find_by_ids", err)
		}
		out = append(out, keys...)
	}
	return out, nil
}

// FindByPrefix returns all API keys with the given prefix.
func (r *GORMAPIKeyRepository) FindByPrefix(ctx context.Context, prefix string) ([]*models.APIKey, error) {
	var keys []*models.APIKey
	if err := r.DB.WithContext(ctx).Where("prefix = ?", prefix).Find(&keys).Error; err != nil {
		return nil, repoError(ctx, "find_by_prefix", err)
	}
	if len(keys) == 0 {
		return nil, dberrors.NewDatabaseError("find_by_prefix", dberrors.ErrNotFound)
	}
	return keys, nil
}

// ListByUser returns all API keys of a user, newest first.
func (r *GORMAPIKeyRepository) ListByUser(ctx context.Context, userID string) ([]*models.APIKey, error) {
	var keys []*models.APIKey
	if err := r.DB.WithContext(ctx).Where("user_id = ?", userID).Order("created_at DESC").Order("id DESC").Find(&keys).Error; err != nil {
		return nil, repoError(ctx, "list_by_user", err)
	}
	return keys, nil
}

// write runs one statement with the lock retry and returns the changed rows.
func (r *GORMAPIKeyRepository) write(ctx context.Context, op string, fn func(db *gorm.DB) *gorm.DB) (int64, error) {
	var affected int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		res := fn(db)
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return 0, repoError(ctx, op, err)
	}
	return affected, nil
}

// UpdateLastUsed sets the last_used_at timestamp of an API key.
func (r *GORMAPIKeyRepository) UpdateLastUsed(ctx context.Context, userID, keyID string, t time.Time) error {
	n, err := r.write(ctx, "update_last_used", func(db *gorm.DB) *gorm.DB {
		return db.Model(&models.APIKey{}).Where("user_id = ? AND id = ?", userID, keyID).Update("last_used_at", t.UTC())
	})
	if err == nil && n == 0 {
		err = dberrors.NewDatabaseError("update_last_used", dberrors.ErrNotFound)
	}
	return err
}

// Delete removes an API key by user ID and key ID.
func (r *GORMAPIKeyRepository) Delete(ctx context.Context, userID, keyID string) error {
	n, err := r.write(ctx, "delete", func(db *gorm.DB) *gorm.DB {
		return db.Where("user_id = ? AND id = ?", userID, keyID).Delete(&models.APIKey{})
	})
	if err == nil && n == 0 {
		err = dberrors.NewDatabaseError("delete", dberrors.ErrNotFound)
	}
	return err
}

// DeleteAllForUser removes every API key of the user with a single
// DELETE ... WHERE user_id = ?. Zero rows is not an error.
func (r *GORMAPIKeyRepository) DeleteAllForUser(ctx context.Context, userID string) (int64, error) {
	if userID == "" {
		return 0, dberrors.NewDatabaseError("delete_all_for_user", dberrors.ErrValidation)
	}
	return r.write(ctx, "delete_all_for_user", func(db *gorm.DB) *gorm.DB {
		return db.Where("user_id = ?", userID).Delete(&models.APIKey{})
	})
}

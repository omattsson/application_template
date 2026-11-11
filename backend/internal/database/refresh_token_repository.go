package database

import (
	"context"
	"time"

	"backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Compile-time interface check.
var _ models.RefreshTokenRepository = (*GORMRefreshTokenRepository)(nil)

// GORMRefreshTokenRepository implements models.RefreshTokenRepository using GORM.
type GORMRefreshTokenRepository struct {
	*BaseRepository
}

// NewGORMRefreshTokenRepository creates a new GORM-backed refresh token repository.
func NewGORMRefreshTokenRepository(db *gorm.DB) *GORMRefreshTokenRepository {
	return &GORMRefreshTokenRepository{BaseRepository: NewBaseRepository(db)}
}

// maxRevokeBatch bounds the IDs of one RevokeOldestForUser call. A user has
// at most the configured token cap of active tokens plus a few concurrent ones.
const maxRevokeBatch = 1000

// familyExpr is the session family of a row: a row without a family_id is
// its own family. It is a constant, never built from input.
const familyExpr = "COALESCE(NULLIF(family_id, ''), id)"

// Create validates and inserts a token. See models.RefreshTokenRepository.
func (r *GORMRefreshTokenRepository) Create(ctx context.Context, token *models.RefreshToken) error {
	if token.ID == "" {
		token.ID = uuid.New().String()
	}
	// A token without a family is its own family: store it, so every family
	// query matches it by family_id.
	if token.FamilyID == "" {
		token.FamilyID = token.ID
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now().UTC()
	}
	// A token without a session start starts a new session. MySQL (strict
	// mode) refuses the zero datetime.
	if token.SessionStartedAt.IsZero() {
		token.SessionStartedAt = token.CreatedAt
	}
	if err := token.Validate(); err != nil {
		return validationError("create", err)
	}
	err := r.Write(ctx, func(db *gorm.DB) error { return db.Create(token).Error })
	return repoError(ctx, "create", err)
}

// FindByTokenHash returns the token with this hash (ErrNotFound for none).
func (r *GORMRefreshTokenRepository) FindByTokenHash(ctx context.Context, hash string) (*models.RefreshToken, error) {
	var token models.RefreshToken
	if err := r.DB.WithContext(ctx).Where("token_hash = ?", hash).Take(&token).Error; err != nil {
		return nil, repoError(ctx, "find_by_token_hash", err)
	}
	return &token, nil
}

// update runs one UPDATE on refresh_tokens with the lock retry and returns
// the number of changed rows.
func (r *GORMRefreshTokenRepository) update(ctx context.Context, op string, scope func(db *gorm.DB) *gorm.DB, values map[string]any) (int64, error) {
	var affected int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		res := scope(db.Model(&models.RefreshToken{})).Updates(values)
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return 0, repoError(ctx, op, err)
	}
	return affected, nil
}

// revoke returns the update that revokes a token, a new map per call.
func revoke() map[string]any { return map[string]any{"revoked": true} }

// RevokeByID revokes one token.
func (r *GORMRefreshTokenRepository) RevokeByID(ctx context.Context, id string) error {
	_, err := r.update(ctx, "revoke_by_id", func(db *gorm.DB) *gorm.DB {
		return db.Where("id = ?", id)
	}, revoke())
	return err
}

// MarkRotatedIfActive revokes an active token and sets rotated_at. See
// models.RefreshTokenRepository.
func (r *GORMRefreshTokenRepository) MarkRotatedIfActive(ctx context.Context, id string, rotatedAt time.Time) (int64, error) {
	return r.update(ctx, "mark_rotated", func(db *gorm.DB) *gorm.DB {
		return db.Where("id = ? AND revoked = ?", id, false)
	}, map[string]any{"revoked": true, "rotated_at": rotatedAt.UTC()})
}

// ClaimReuseGrace clears rotated_at of a token consumed by a rotation at or
// after rotatedSince. The conditional UPDATE decides between concurrent
// claims: one changes the row, the others find rotated_at NULL. See
// models.RefreshTokenRepository.
func (r *GORMRefreshTokenRepository) ClaimReuseGrace(ctx context.Context, id string, rotatedSince time.Time) (bool, error) {
	n, err := r.update(ctx, "claim_reuse_grace", func(db *gorm.DB) *gorm.DB {
		return db.Where("id = ? AND revoked = ? AND rotated_at IS NOT NULL AND rotated_at >= ?", id, true, rotatedSince.UTC())
	}, map[string]any{"rotated_at": nil})
	return n == 1, err
}

// RevokeFamily revokes the active tokens of a family. "id = ?" also matches
// a token without a family_id that started the family.
func (r *GORMRefreshTokenRepository) RevokeFamily(ctx context.Context, familyID string) error {
	_, err := r.update(ctx, "revoke_family", func(db *gorm.DB) *gorm.DB {
		return db.Where("(family_id = ? OR id = ?) AND revoked = ?", familyID, familyID, false)
	}, revoke())
	return err
}

// RevokeFamilyForUser revokes the active tokens of a family only when they
// belong to userID (same match as RevokeFamily, plus the owner).
func (r *GORMRefreshTokenRepository) RevokeFamilyForUser(ctx context.Context, familyID, userID string) error {
	_, err := r.update(ctx, "revoke_family_for_user", func(db *gorm.DB) *gorm.DB {
		return db.Where("(family_id = ? OR id = ?) AND user_id = ? AND revoked = ?", familyID, familyID, userID, false)
	}, revoke())
	return err
}

// ListRecentFamiliesForUser returns the distinct session families of the
// refresh tokens of the user that expire after since, whether revoked or not
// (a family revoked by idle timeout or the token cap can still hold a live
// access token). Newest family first, at most models.MaxRecentFamilies. The
// user_id index finds the rows of the user; the user has few rows (the token
// cap plus recent revoked ones).
func (r *GORMRefreshTokenRepository) ListRecentFamiliesForUser(ctx context.Context, userID string, since time.Time) ([]string, error) {
	var families []string
	err := r.DB.WithContext(ctx).Model(&models.RefreshToken{}).
		Select(familyExpr+" AS family").
		Where("user_id = ? AND expires_at > ?", userID, since.UTC()).
		Group(familyExpr).
		Order("MAX(created_at) DESC").
		Limit(models.MaxRecentFamilies).
		Scan(&families).Error
	if err != nil {
		return nil, repoError(ctx, "list_recent_families", err)
	}
	return families, nil
}

// CountActiveInFamily counts the active tokens of a family.
func (r *GORMRefreshTokenRepository) CountActiveInFamily(ctx context.Context, familyID string) (int64, error) {
	var count int64
	err := r.DB.WithContext(ctx).Model(&models.RefreshToken{}).
		Where("(family_id = ? OR id = ?) AND revoked = ? AND expires_at > ?", familyID, familyID, false, time.Now().UTC()).
		Count(&count).Error
	if err != nil {
		return 0, repoError(ctx, "count_active_in_family", err)
	}
	return count, nil
}

// TouchFamily updates last_activity of the active tokens of a family (same
// family match as RevokeFamily). The "last_activity < ?" guard keeps a late
// write from moving the time back.
func (r *GORMRefreshTokenRepository) TouchFamily(ctx context.Context, familyID string, at time.Time) error {
	at = at.UTC()
	_, err := r.update(ctx, "touch_family", func(db *gorm.DB) *gorm.DB {
		return db.Where("(family_id = ? OR id = ?) AND revoked = ? AND expires_at > ? AND last_activity < ?", familyID, familyID, false, at, at)
	}, map[string]any{"last_activity": at})
	return err
}

// RevokeAllForUser revokes every active token of the user.
func (r *GORMRefreshTokenRepository) RevokeAllForUser(ctx context.Context, userID string) error {
	_, err := r.update(ctx, "revoke_all_for_user", func(db *gorm.DB) *gorm.DB {
		return db.Where("user_id = ? AND revoked = ?", userID, false)
	}, revoke())
	return err
}

// RevokeAllForUserExcept revokes every active token of the user but excludeID.
func (r *GORMRefreshTokenRepository) RevokeAllForUserExcept(ctx context.Context, userID string, excludeID string) error {
	_, err := r.update(ctx, "revoke_all_for_user_except", func(db *gorm.DB) *gorm.DB {
		return db.Where("user_id = ? AND revoked = ? AND id <> ?", userID, false, excludeID)
	}, revoke())
	return err
}

// RevokeOldestForUser revokes the active tokens of the user except the keep
// newest ones and excludeID. It reads the IDs first (MySQL refuses LIMIT in an
// IN subquery); a token created after the read is newer than every revoked one
// and stays active.
func (r *GORMRefreshTokenRepository) RevokeOldestForUser(ctx context.Context, userID string, keep int, excludeID string) (int64, error) {
	keep = max(keep, 0)
	q := r.DB.WithContext(ctx).Model(&models.RefreshToken{}).
		Where("user_id = ? AND revoked = ? AND expires_at > ?", userID, false, time.Now().UTC())
	if excludeID != "" {
		q = q.Where("id <> ?", excludeID)
	}
	var ids []string
	if err := q.Order("created_at DESC").Order("id DESC").Offset(keep).Limit(maxRevokeBatch).Pluck("id", &ids).Error; err != nil {
		return 0, repoError(ctx, "revoke_oldest_for_user", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return r.update(ctx, "revoke_oldest_for_user", func(db *gorm.DB) *gorm.DB {
		return db.Where("id IN ? AND revoked = ?", ids, false)
	}, revoke())
}

// DeleteExpired deletes the tokens that expired before `before`.
func (r *GORMRefreshTokenRepository) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	var affected int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		res := db.Where("expires_at < ?", before.UTC()).Delete(&models.RefreshToken{})
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return 0, repoError(ctx, "delete_expired", err)
	}
	return affected, nil
}

// CountActiveForUser counts the active tokens of the user.
func (r *GORMRefreshTokenRepository) CountActiveForUser(ctx context.Context, userID string) (int64, error) {
	var count int64
	err := r.DB.WithContext(ctx).Model(&models.RefreshToken{}).
		Where("user_id = ? AND revoked = ? AND expires_at > ?", userID, false, time.Now().UTC()).
		Count(&count).Error
	if err != nil {
		return 0, repoError(ctx, "count_active_for_user", err)
	}
	return count, nil
}

// WithTx runs fn in one transaction with ctx (BaseRepository.Tx: the whole
// transaction runs again after a deadlock). The repository passed to fn is
// built on the transaction, so its writes do not retry on their own.
func (r *GORMRefreshTokenRepository) WithTx(ctx context.Context, fn func(txRepo models.RefreshTokenRepository) error) error {
	err := r.Tx(ctx, func(tx *gorm.DB) error {
		return fn(NewGORMRefreshTokenRepository(tx))
	})
	return repoError(ctx, "refresh_token_tx", err)
}

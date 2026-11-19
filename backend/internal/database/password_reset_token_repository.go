package database

import (
	"context"
	"errors"
	"time"

	"backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// Compile-time interface check.
var _ models.PasswordResetTokenRepository = (*GORMPasswordResetTokenRepository)(nil)

// GORMPasswordResetTokenRepository implements models.PasswordResetTokenRepository
// with GORM. MySQL arbitrates between replicas: a consume is one conditional
// UPDATE, an issue is one short transaction that locks the rows of the user,
// and every time is written and compared with the database clock
// (UTC_TIMESTAMP(3)), so a clock difference between the replicas does not
// matter and no Go time value crosses the driver.
type GORMPasswordResetTokenRepository struct {
	*BaseRepository
}

// NewGORMPasswordResetTokenRepository creates a GORM-backed repository. The
// table password_reset_tokens must exist (migration 20261119000001). Its
// statements run on a session with a silent GORM logger: a failing statement
// would otherwise log its values, which include the hash of a token, and the
// handler logs a failure itself, through a logutil.Limiter.
func NewGORMPasswordResetTokenRepository(db *gorm.DB) *GORMPasswordResetTokenRepository {
	if db != nil && db.Logger != nil {
		db = db.Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	}

	return &GORMPasswordResetTokenRepository{BaseRepository: NewBaseRepository(db)}
}

// validTokenHash reports whether s is a lowercase hex SHA-256 digest.
func validTokenHash(s string) bool {
	if len(s) != models.PasswordResetTokenHashLen {
		return false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// Issue implements models.PasswordResetTokenRepository.
//
// The transaction starts with a locking read of the user's rows that are
// younger than minInterval. A concurrent Issue for the same user either waits
// for the lock or, when the user has no row yet, both take a gap lock and one
// of the inserts is chosen as the deadlock victim; RetryOnLock runs it again,
// and it then finds the other's row. Either way one request issues the token.
func (r *GORMPasswordResetTokenRepository) Issue(ctx context.Context, userID, tokenHash string, ttl, minInterval time.Duration) (bool, error) {
	if userID == "" || !validTokenHash(tokenHash) || ttl <= 0 || minInterval < 0 {
		return false, validationError("password reset issue", errors.New("a user, a token hash and a positive ttl are required"))
	}

	dialect := r.DB.Dialector.Name()
	issued := false

	err := r.Tx(ctx, func(tx *gorm.DB) error {
		issued = false

		var recent []string

		q := tx.Model(&models.PasswordResetToken{}).
			Where("user_id = ? AND created_at > "+heartbeatAgoSQL(dialect), userID, heartbeatAgoArg(dialect, minInterval)).
			Limit(1)
		if dialect == dialectMySQL {
			q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
		}

		if err := q.Pluck("id", &recent).Error; err != nil {
			return err
		}

		if len(recent) > 0 {
			return nil
		}

		if err := tx.Where("user_id = ? AND used_at IS NULL", userID).Delete(&models.PasswordResetToken{}).Error; err != nil {
			return err
		}

		err := tx.Model(&models.PasswordResetToken{}).Create(map[string]any{
			"id":         uuid.New().String(),
			"user_id":    userID,
			"token_hash": tokenHash,
			"created_at": gorm.Expr(heartbeatNowSQL(dialect)),
			"expires_at": gorm.Expr(idemOffsetSQL(dialect), idemOffsetArg(dialect, ttl)),
		}).Error
		if err != nil {
			return err
		}

		issued = true

		return nil
	})
	if err != nil {
		return false, repoError(ctx, "password reset issue", err)
	}

	return issued, nil
}

// Consume implements models.PasswordResetTokenRepository. The conditional
// UPDATE is the only place that decides: the row lock makes a second request
// with the same token wait for the first, and it then finds used_at set and
// changes nothing.
func (r *GORMPasswordResetTokenRepository) Consume(ctx context.Context, tokenHash string) (string, bool, error) {
	if !validTokenHash(tokenHash) {
		return "", false, nil
	}

	dialect := r.DB.Dialector.Name()
	userID := ""
	ok := false

	err := r.Tx(ctx, func(tx *gorm.DB) error {
		userID, ok = "", false

		res := tx.Model(&models.PasswordResetToken{}).
			Where("token_hash = ? AND used_at IS NULL AND expires_at > "+heartbeatNowSQL(dialect), tokenHash).
			Updates(map[string]any{"used_at": gorm.Expr(heartbeatNowSQL(dialect))})
		if res.Error != nil {
			return res.Error
		}

		if res.RowsAffected != 1 {
			return nil
		}

		var ids []string
		if err := tx.Model(&models.PasswordResetToken{}).Where("token_hash = ?", tokenHash).Limit(1).Pluck("user_id", &ids).Error; err != nil {
			return err
		}

		if len(ids) != 1 {
			return gorm.ErrRecordNotFound
		}

		userID, ok = ids[0], true

		return nil
	})
	if err != nil {
		return "", false, repoError(ctx, "password reset consume", err)
	}

	return userID, ok, nil
}

// DeleteUnusedForUser implements models.PasswordResetTokenRepository.
func (r *GORMPasswordResetTokenRepository) DeleteUnusedForUser(ctx context.Context, userID string) (int64, error) {
	var n int64

	err := r.Write(ctx, func(db *gorm.DB) error {
		n = 0
		res := db.Where("user_id = ? AND used_at IS NULL", userID).Delete(&models.PasswordResetToken{})
		n = res.RowsAffected

		return res.Error
	})

	return n, repoError(ctx, "password reset delete unused", err)
}

// DeleteStale implements models.PasswordResetTokenRepository. It selects the
// IDs first and deletes them by primary key, so the statement is bounded and
// portable (no DELETE ... LIMIT).
func (r *GORMPasswordResetTokenRepository) DeleteStale(ctx context.Context, usedRetention time.Duration, limit int) (int64, error) {
	dialect := r.DB.Dialector.Name()

	var deleted int64

	err := r.Write(ctx, func(db *gorm.DB) error {
		deleted = 0

		var ids []string
		if err := db.Model(&models.PasswordResetToken{}).
			Where("expires_at < "+heartbeatNowSQL(dialect)+" OR (used_at IS NOT NULL AND used_at < "+heartbeatAgoSQL(dialect)+")",
				heartbeatAgoArg(dialect, usedRetention)).
			Limit(limit).
			Pluck("id", &ids).Error; err != nil {
			return err
		}

		if len(ids) == 0 {
			return nil
		}

		res := db.Where("id IN ?", ids).Delete(&models.PasswordResetToken{})
		deleted = res.RowsAffected

		return res.Error
	})

	return deleted, repoError(ctx, "password reset cleanup", err)
}

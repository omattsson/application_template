package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Compile-time interface check.
var _ models.IdempotencyRepository = (*GORMIdempotencyRepository)(nil)

// claimAttempts bounds the loop of Claim. A row that appears and vanishes
// between the insert and the read (a release or the janitor) is the only
// reason for a second round.
const claimAttempts = 3

// GORMIdempotencyRepository implements models.IdempotencyRepository with
// GORM. MySQL arbitrates between replicas: a claim is one insert on the
// primary key (scope, idempotency_key), a take-over is one conditional
// UPDATE of that row, and the owner of a row is told apart by lock_id. Every
// time is written and compared with the database clock (UTC_TIMESTAMP(3)), so
// a clock difference between the replicas does not matter, and no Go time
// value crosses the driver.
type GORMIdempotencyRepository struct {
	*BaseRepository
}

// NewGORMIdempotencyRepository creates a GORM-backed idempotency repository.
// The table idempotency_keys must exist (migration 20261117000001). Its
// statements run on a session with a silent GORM logger: while the database
// fails every request with a key fails here, and the middleware logs that
// once per interval itself.
func NewGORMIdempotencyRepository(db *gorm.DB) *GORMIdempotencyRepository {
	if db != nil && db.Logger != nil {
		db = db.Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	}
	return &GORMIdempotencyRepository{BaseRepository: NewBaseRepository(db)}
}

// idemOffsetSQL is the database time plus the offset given as the one
// argument (idemOffsetArg), with millisecond precision, UTC. The text is a
// constant per dialect, never built from input.
func idemOffsetSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "(UTC_TIMESTAMP(3) + INTERVAL ? MICROSECOND)"
	}
	return "strftime('%Y-%m-%d %H:%M:%f', 'now', ?)"
}

// idemOffsetArg is the argument of idemOffsetSQL for d (negative: the past):
// signed microseconds on MySQL, a SQLite date modifier ("+3600.000 seconds")
// otherwise.
func idemOffsetArg(dialect string, d time.Duration) any {
	if dialect == dialectMySQL {
		return d.Microseconds()
	}
	return fmt.Sprintf("%+.3f seconds", d.Seconds())
}

// idemAgeMicrosSQL is the age of locked_at in microseconds, computed by the
// database.
func idemAgeMicrosSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "TIMESTAMPDIFF(MICROSECOND, locked_at, UTC_TIMESTAMP(3))"
	}
	return "CAST((julianday('now') - julianday(locked_at)) * 86400000000 AS INTEGER)"
}

// idemInsertMap is the row of a new claim.
func idemInsertMap(dialect, scope, key, hash, lockID string, ttl time.Duration) map[string]any {
	return map[string]any{
		"scope":           scope,
		"idempotency_key": key,
		"request_hash":    hash,
		"lock_id":         lockID,
		"content_type":    "",
		"status_code":     0,
		"locked_at":       gorm.Expr(idemOffsetSQL(dialect), idemOffsetArg(dialect, 0)),
		"expires_at":      gorm.Expr(idemOffsetSQL(dialect), idemOffsetArg(dialect, ttl)),
	}
}

// Claim implements models.IdempotencyRepository.
func (r *GORMIdempotencyRepository) Claim(ctx context.Context, scope, key, hash string, ttl, lockTimeout time.Duration) (*models.IdempotencyClaim, error) {
	if scope == "" || key == "" || hash == "" || ttl <= 0 || lockTimeout <= 0 {
		return nil, validationError("idempotency claim", errors.New("scope, key, hash, ttl and lock timeout are required"))
	}
	dialect := r.DB.Dialector.Name()

	for attempt := 0; attempt < claimAttempts; attempt++ {
		lockID := uuid.New().String()

		// 1. A new key: one insert on the primary key. The loser of a race gets
		// a duplicate key error.
		err := r.Write(ctx, func(db *gorm.DB) error {
			return db.Model(&models.IdempotencyKey{}).Create(idemInsertMap(dialect, scope, key, hash, lockID, ttl)).Error
		})
		if err == nil {
			return &models.IdempotencyClaim{Outcome: models.IdempotencyClaimed, LockID: lockID}, nil
		}
		if !IsDuplicateKey(err) {
			return nil, repoError(ctx, "idempotency claim", err)
		}

		// 2. The row exists. Take it over when it expired, or when it is in
		// progress past the lock timeout with the same fingerprint. One UPDATE
		// decides: of several callers only one changes the row, the others find
		// the new lock and match no row.
		var taken int64
		err = r.Write(ctx, func(db *gorm.DB) error {
			now := idemOffsetSQL(dialect)
			res := db.Model(&models.IdempotencyKey{}).
				Where("scope = ? AND idempotency_key = ?", scope, key).
				Where("(expires_at <= "+now+" OR (status_code = 0 AND request_hash = ? AND locked_at <= "+now+"))",
					idemOffsetArg(dialect, 0), hash, idemOffsetArg(dialect, -lockTimeout)).
				Updates(map[string]any{
					"request_hash": hash,
					"lock_id":      lockID,
					"content_type": "",
					"status_code":  0,
					"body":         nil,
					"locked_at":    gorm.Expr(now, idemOffsetArg(dialect, 0)),
					"expires_at":   gorm.Expr(now, idemOffsetArg(dialect, ttl)),
				})
			taken = res.RowsAffected
			return res.Error
		})
		if err != nil {
			return nil, repoError(ctx, "idempotency claim", err)
		}
		if taken > 0 {
			return &models.IdempotencyClaim{Outcome: models.IdempotencyClaimed, LockID: lockID}, nil
		}

		// 3. Somebody else owns the key: say what the caller must do.
		var row struct {
			RequestHash string
			ContentType string
			Body        []byte
			AgeMicros   int64
			StatusCode  int
		}
		err = r.DB.WithContext(ctx).Model(&models.IdempotencyKey{}).
			Select("request_hash, status_code, content_type, CASE WHEN request_hash = ? THEN body END AS body, "+idemAgeMicrosSQL(dialect)+" AS age_micros", hash).
			Where("scope = ? AND idempotency_key = ?", scope, key).
			Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue // released or deleted since the insert: claim again
		}
		if err != nil {
			return nil, repoError(ctx, "idempotency claim", err)
		}

		switch {
		case row.RequestHash != hash:
			return &models.IdempotencyClaim{Outcome: models.IdempotencyMismatch}, nil
		case row.StatusCode == 0:
			return &models.IdempotencyClaim{Outcome: models.IdempotencyInProgress, RetryAfter: retryAfter(lockTimeout, row.AgeMicros)}, nil
		default:
			return &models.IdempotencyClaim{
				Outcome:  models.IdempotencyReplay,
				Response: &models.IdempotencyResponse{StatusCode: row.StatusCode, ContentType: row.ContentType, Body: row.Body},
			}, nil
		}
	}

	// The row kept changing under us. It is being worked on by somebody.
	return &models.IdempotencyClaim{Outcome: models.IdempotencyInProgress, RetryAfter: time.Second}, nil
}

// retryAfter is the time until a lock of the age ageMicros can be taken over,
// rounded up to whole seconds, at least one second.
func retryAfter(lockTimeout time.Duration, ageMicros int64) time.Duration {
	left := lockTimeout - time.Duration(ageMicros)*time.Microsecond
	secs := max(int64((left+time.Second-1)/time.Second), 1)
	return time.Duration(secs) * time.Second
}

// Complete implements models.IdempotencyRepository.
func (r *GORMIdempotencyRepository) Complete(ctx context.Context, scope, key, lockID string, resp models.IdempotencyResponse, ttl time.Duration) (bool, error) {
	if resp.StatusCode < 100 || resp.StatusCode > 599 {
		return false, validationError("idempotency complete", errors.New("the status code must be 100 to 599"))
	}
	dialect := r.DB.Dialector.Name()
	var updated int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		res := db.Model(&models.IdempotencyKey{}).
			Where("scope = ? AND idempotency_key = ? AND lock_id = ? AND status_code = 0", scope, key, lockID).
			Updates(map[string]any{
				"status_code":  resp.StatusCode,
				"content_type": resp.ContentType,
				"body":         resp.Body,
				"expires_at":   gorm.Expr(idemOffsetSQL(dialect), idemOffsetArg(dialect, ttl)),
			})
		updated = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return false, repoError(ctx, "idempotency complete", err)
	}
	return updated > 0, nil
}

// Release implements models.IdempotencyRepository.
func (r *GORMIdempotencyRepository) Release(ctx context.Context, scope, key, lockID string) error {
	err := r.Write(ctx, func(db *gorm.DB) error {
		return db.Where("scope = ? AND idempotency_key = ? AND lock_id = ? AND status_code = 0", scope, key, lockID).
			Delete(&models.IdempotencyKey{}).Error
	})
	return repoError(ctx, "idempotency release", err)
}

// DeleteExpired implements models.IdempotencyRepository. It reads the primary
// keys of one batch (the index on expires_at serves it), then deletes exactly
// those rows, again under the condition that they are expired: a row that a
// request took over in between is not deleted. Two statements instead of
// DELETE ... LIMIT, so the same code runs on MySQL and SQLite.
func (r *GORMIdempotencyRepository) DeleteExpired(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, validationError("idempotency cleanup", errors.New("the batch size must be positive"))
	}
	dialect := r.DB.Dialector.Name()
	now := idemOffsetSQL(dialect)

	var batch []struct {
		Scope string
		Key   string `gorm:"column:idempotency_key"`
	}
	err := r.DB.WithContext(ctx).Model(&models.IdempotencyKey{}).
		Select("scope, idempotency_key").
		Where("expires_at < "+now, idemOffsetArg(dialect, 0)).
		Order("expires_at").
		Limit(limit).
		Find(&batch).Error
	if err != nil {
		return 0, repoError(ctx, "idempotency cleanup", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	pairs := make([][]any, len(batch))
	for i, b := range batch {
		pairs[i] = []any{b.Scope, b.Key}
	}
	var deleted int64
	err = r.Write(ctx, func(db *gorm.DB) error {
		res := db.Where("(scope, idempotency_key) IN ?", pairs).
			Where("expires_at < "+now, idemOffsetArg(dialect, 0)).
			Delete(&models.IdempotencyKey{})
		deleted = res.RowsAffected
		return res.Error
	})
	return deleted, repoError(ctx, "idempotency cleanup", err)
}

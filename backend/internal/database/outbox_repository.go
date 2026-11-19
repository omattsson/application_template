package database

import (
	"context"
	"time"

	"backend/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Compile-time interface check.
var _ models.OutboxRepository = (*GORMOutboxRepository)(nil)

// GORMOutboxRepository implements models.OutboxRepository using GORM. The row
// time is written and compared with the database server clock, so a clock
// difference between the replicas does not matter and no Go time value
// crosses the driver.
type GORMOutboxRepository struct {
	*BaseRepository
}

// NewGORMOutboxRepository creates a GORM-backed outbox repository. The table
// outbox_events must exist (migration 20261117000002). Its statements run on
// a session with a silent GORM logger: while the database fails, every poll
// would write an Error line, and the poller logs a failure itself, through a
// logutil.Limiter.
func NewGORMOutboxRepository(db *gorm.DB) *GORMOutboxRepository {
	if db != nil && db.Logger != nil {
		db = db.Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	}
	return &GORMOutboxRepository{BaseRepository: NewBaseRepository(db)}
}

// InsertOutboxEvent writes one event row on tx, the transaction of the change
// that the event describes. It is the write side of the outbox: pass it to
// outbox.ItemHook. created_at is the database time. It has no state and no
// side effect outside the database, so the repository can run the whole
// transaction again after a deadlock.
func InsertOutboxEvent(tx *gorm.DB, eventType string, message []byte) error {
	return tx.Model(&models.OutboxEvent{}).Create(map[string]any{
		"event_type": eventType,
		"message":    string(message),
		"created_at": gorm.Expr(heartbeatNowSQL(tx.Dialector.Name())),
	}).Error
}

// MaxID returns the highest event ID, or 0 for an empty table.
func (r *GORMOutboxRepository) MaxID(ctx context.Context) (uint64, error) {
	var maxID uint64
	err := r.DB.WithContext(ctx).Model(&models.OutboxEvent{}).
		Select("COALESCE(MAX(id), 0)").Scan(&maxID).Error
	return maxID, repoError(ctx, "outbox max id", err)
}

// ListAfter returns at most limit events with ID > after, in ID order. It
// selects the columns that delivery needs.
func (r *GORMOutboxRepository) ListAfter(ctx context.Context, after uint64, limit int) ([]models.OutboxEvent, error) {
	var rows []models.OutboxEvent
	err := r.DB.WithContext(ctx).
		Select("id", "event_type", "message").
		Where("id > ?", after).
		Order("id ASC").
		Limit(limit).
		Find(&rows).Error
	return rows, repoError(ctx, "outbox list after", err)
}

// ListByIDs returns the events with the given IDs that exist, in ID order.
func (r *GORMOutboxRepository) ListByIDs(ctx context.Context, ids []uint64) ([]models.OutboxEvent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.OutboxEvent
	err := r.DB.WithContext(ctx).
		Select("id", "event_type", "message").
		Where("id IN ?", ids).
		Order("id ASC").
		Find(&rows).Error
	return rows, repoError(ctx, "outbox list by ids", err)
}

// outboxAgeSQL is the age of created_at by the database clock in
// microseconds. It is a constant per dialect, never built from input.
func outboxAgeSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "TIMESTAMPDIFF(MICROSECOND, created_at, UTC_TIMESTAMP(3))"
	}
	return "CAST((julianday('now') - julianday(created_at)) * 86400000000 AS INTEGER)"
}

// Age returns how old the event id is by the database clock, or false when
// the row does not exist.
func (r *GORMOutboxRepository) Age(ctx context.Context, id uint64) (time.Duration, bool, error) {
	var micros []int64
	err := r.DB.WithContext(ctx).Model(&models.OutboxEvent{}).
		Where("id = ?", id).
		Limit(1).
		Pluck(outboxAgeSQL(r.DB.Dialector.Name()), &micros).Error
	if err != nil {
		return 0, false, repoError(ctx, "outbox age", err)
	}
	if len(micros) == 0 {
		return 0, false, nil
	}
	return time.Duration(micros[0]) * time.Microsecond, true, nil
}

// DeleteOlderThan deletes at most limit events whose created_at is more than
// maxAge before the database time, oldest first. It selects the IDs first and
// deletes them by primary key, so the statement is bounded and portable (no
// DELETE ... LIMIT).
func (r *GORMOutboxRepository) DeleteOlderThan(ctx context.Context, maxAge time.Duration, limit int) (int64, error) {
	dialect := r.DB.Dialector.Name()
	var deleted int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		deleted = 0
		var ids []uint64
		if err := db.Model(&models.OutboxEvent{}).
			Where("created_at < "+heartbeatAgoSQL(dialect), heartbeatAgoArg(dialect, maxAge)).
			Order("id ASC").
			Limit(limit).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		res := db.Where("id IN ?", ids).Delete(&models.OutboxEvent{})
		deleted = res.RowsAffected
		return res.Error
	})
	return deleted, repoError(ctx, "outbox cleanup", err)
}

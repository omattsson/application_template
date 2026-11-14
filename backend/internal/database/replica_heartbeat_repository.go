package database

import (
	"context"
	"fmt"
	"time"

	"backend/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// Compile-time interface check.
var _ models.ReplicaHeartbeatRepository = (*GORMReplicaHeartbeatRepository)(nil)

// GORMReplicaHeartbeatRepository implements models.ReplicaHeartbeatRepository
// using GORM. last_seen is written and compared with the database server
// clock, so a clock difference between the replicas does not matter, and no
// Go time value crosses the driver (no time zone or precision conversion).
type GORMReplicaHeartbeatRepository struct {
	*BaseRepository
}

// NewGORMReplicaHeartbeatRepository creates a GORM-backed heartbeat
// repository. The table replica_heartbeats must exist (migration
// 20260101000003). Its statements run on a session with a silent GORM logger:
// while the database fails, every heartbeat write (and cleanup run) would
// write an Error line, and the callers log a failure once themselves
// (replica.Heartbeat, the leader worker).
func NewGORMReplicaHeartbeatRepository(db *gorm.DB) *GORMReplicaHeartbeatRepository {
	if db != nil && db.Logger != nil {
		db = db.Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	}
	return &GORMReplicaHeartbeatRepository{BaseRepository: NewBaseRepository(db)}
}

// heartbeatNowSQL is the current database time with millisecond precision:
// UTC_TIMESTAMP(3) on MySQL (independent of the session time zone), an ISO
// text with milliseconds in UTC on SQLite (unit tests). It is a constant per
// dialect, never built from input.
func heartbeatNowSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "UTC_TIMESTAMP(3)"
	}
	return "strftime('%Y-%m-%d %H:%M:%f', 'now')"
}

// heartbeatAgoSQL is the database time minus the age given as the one
// argument (heartbeatAgoArg).
func heartbeatAgoSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "(UTC_TIMESTAMP(3) - INTERVAL ? MICROSECOND)"
	}
	return "strftime('%Y-%m-%d %H:%M:%f', 'now', ?)"
}

// heartbeatAgoArg is the argument of heartbeatAgoSQL for age: microseconds
// on MySQL, a SQLite date modifier ("-90.000 seconds") otherwise.
func heartbeatAgoArg(dialect string, age time.Duration) any {
	if dialect == dialectMySQL {
		return age.Microseconds()
	}
	return fmt.Sprintf("-%.3f seconds", age.Seconds())
}

// Beat inserts the row of id or updates its last_seen to the database time
// (ON DUPLICATE KEY UPDATE on MySQL, ON CONFLICT on SQLite).
func (r *GORMReplicaHeartbeatRepository) Beat(ctx context.Context, id string) error {
	now := gorm.Expr(heartbeatNowSQL(r.DB.Dialector.Name()))
	err := r.Write(ctx, func(db *gorm.DB) error {
		return db.Model(&models.ReplicaHeartbeat{}).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.Assignments(map[string]any{"last_seen": now}),
		}).Create(map[string]any{"id": id, "last_seen": now}).Error
	})
	return repoError(ctx, "replica heartbeat beat", err)
}

// Remove deletes the row of id. A missing row is not an error.
func (r *GORMReplicaHeartbeatRepository) Remove(ctx context.Context, id string) error {
	err := r.Write(ctx, func(db *gorm.DB) error {
		return db.Where("id = ?", id).Delete(&models.ReplicaHeartbeat{}).Error
	})
	return repoError(ctx, "replica heartbeat remove", err)
}

// DeleteOlderThan deletes the rows whose last_seen is more than maxAge
// before the database time.
func (r *GORMReplicaHeartbeatRepository) DeleteOlderThan(ctx context.Context, maxAge time.Duration) (int64, error) {
	dialect := r.DB.Dialector.Name()
	var deleted int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		res := db.Where("last_seen < "+heartbeatAgoSQL(dialect), heartbeatAgoArg(dialect, maxAge)).Delete(&models.ReplicaHeartbeat{})
		deleted = res.RowsAffected
		return res.Error
	})
	return deleted, repoError(ctx, "replica heartbeat cleanup", err)
}

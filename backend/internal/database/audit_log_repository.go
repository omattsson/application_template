package database

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Compile-time interface check.
var _ models.AuditLogRepository = (*GORMAuditLogRepository)(nil)

// GORMAuditLogRepository implements models.AuditLogRepository using GORM.
type GORMAuditLogRepository struct {
	*BaseRepository
}

// NewGORMAuditLogRepository creates a new GORM-backed audit log repository.
func NewGORMAuditLogRepository(db *gorm.DB) *GORMAuditLogRepository {
	return &GORMAuditLogRepository{BaseRepository: NewBaseRepository(db)}
}

// Create validates and inserts an audit log record.
func (r *GORMAuditLogRepository) Create(ctx context.Context, log *models.AuditLog) error {
	if err := log.Validate(); err != nil {
		return validationError("create", err)
	}
	if log.ID == "" {
		log.ID = uuid.New().String()
	}
	if log.Timestamp.IsZero() {
		log.Timestamp = time.Now().UTC()
	}
	err := r.Write(ctx, func(db *gorm.DB) error { return db.Create(log).Error })
	return repoError(ctx, "create", err)
}

// auditPageSize returns the page size of a list: models.DefaultPageSize for
// none, at most models.MaxPageSize.
func auditPageSize(limit int) int {
	if limit <= 0 {
		return models.DefaultPageSize
	}
	return min(limit, models.MaxPageSize)
}

// List returns audit logs matching the provided filters, newest first.
// When filters.Cursor is set, keyset (cursor-based) pagination is used for
// efficient traversal of large datasets and no total is computed. Otherwise
// OFFSET pagination is used; a full page that has rows after it carries the
// NextCursor that continues with keyset paging.
func (r *GORMAuditLogRepository) List(ctx context.Context, filters models.AuditLogFilters) (*models.AuditLogResult, error) {
	query := r.DB.WithContext(ctx).Model(&models.AuditLog{})

	if filters.Action != "" {
		query = query.Where("action = ?", filters.Action)
	}
	if filters.EntityType != "" {
		query = query.Where("entity_type = ?", filters.EntityType)
	}
	if filters.EntityID != "" {
		query = query.Where("entity_id = ?", filters.EntityID)
	}
	if filters.UserID != "" {
		query = query.Where("user_id = ?", filters.UserID)
	}
	if filters.StartDate != nil {
		query = query.Where("timestamp >= ?", filters.StartDate.UTC())
	}
	if filters.EndDate != nil {
		query = query.Where("timestamp <= ?", filters.EndDate.UTC())
	}

	limit := auditPageSize(filters.Limit)

	if filters.Cursor != "" {
		return r.listAfterCursor(ctx, query, filters.Cursor, limit)
	}

	// Use an estimated count for large unfiltered result sets to avoid an
	// expensive full scan of this append-only table. With any filter the
	// result set is bounded, so an exact count is acceptable.
	hasFilters := filters.Action != "" || filters.EntityType != "" ||
		filters.EntityID != "" || filters.UserID != "" ||
		filters.StartDate != nil || filters.EndDate != nil

	var total int64
	if hasFilters || !r.estimateRows(ctx, &total) {
		if err := query.Count(&total).Error; err != nil {
			return nil, repoError(ctx, "count", err)
		}
	}

	var logs []models.AuditLog
	if err := query.Order("timestamp DESC, id DESC").Limit(limit).Offset(max(filters.Offset, 0)).Find(&logs).Error; err != nil {
		return nil, repoError(ctx, "list", err)
	}

	result := &models.AuditLogResult{Data: logs, Total: total}
	// A full page with more rows after it gives the cursor of the next page,
	// so a client can switch to keyset paging from the first page on.
	if n := len(logs); n == limit && int64(max(filters.Offset, 0)+n) < total {
		result.NextCursor = encodeAuditCursor(logs[n-1].Timestamp, logs[n-1].ID)
	}
	return result, nil
}

// estimateRows reads the MySQL table statistics estimate (about O(1))
// instead of COUNT(*). It reports false when there is no estimate (SQLite,
// an empty or unanalyzed table), and the caller counts exactly.
func (r *GORMAuditLogRepository) estimateRows(ctx context.Context, total *int64) bool {
	if r.DB.Dialector.Name() != "mysql" {
		return false
	}
	var n int64
	err := r.DB.WithContext(ctx).
		Raw("SELECT COALESCE(table_rows, 0) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'audit_logs'").
		Scan(&n).Error
	if err != nil || n <= 0 {
		return false
	}
	*total = n
	return true
}

// listAfterCursor returns the page after cursor: rows with an earlier
// timestamp, or the same timestamp and a smaller ID (the tie-breaker). It
// reads limit+1 rows to know whether a next page exists.
func (r *GORMAuditLogRepository) listAfterCursor(ctx context.Context, query *gorm.DB, cursor string, limit int) (*models.AuditLogResult, error) {
	cursorTS, cursorID, err := decodeAuditCursor(cursor)
	if err != nil {
		return nil, validationError("list", err)
	}
	query = query.Where("(timestamp < ?) OR (timestamp = ? AND id < ?)", cursorTS, cursorTS, cursorID)

	var logs []models.AuditLog
	if err := query.Order("timestamp DESC, id DESC").Limit(limit + 1).Find(&logs).Error; err != nil {
		return nil, repoError(ctx, "list", err)
	}

	result := &models.AuditLogResult{Total: -1}
	if len(logs) > limit {
		logs = logs[:limit]
		last := logs[limit-1]
		result.NextCursor = encodeAuditCursor(last.Timestamp, last.ID)
	}
	result.Data = logs
	return result, nil
}

// encodeAuditCursor creates an opaque cursor from a timestamp and ID.
// Uses RawURLEncoding so the cursor is safe for use as a query parameter.
func encodeAuditCursor(ts time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(ts.UTC().Format(time.RFC3339Nano) + "|" + id),
	)
}

// auditCursorParts is the number of "|"-separated parts of a cursor.
const auditCursorParts = 2

// decodeAuditCursor extracts a timestamp and ID from an opaque cursor.
func decodeAuditCursor(cursor string) (time.Time, string, error) {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", errors.New("invalid cursor encoding")
	}
	parts := strings.SplitN(string(data), "|", auditCursorParts)
	if len(parts) != auditCursorParts || parts[0] == "" || parts[1] == "" {
		return time.Time{}, "", errors.New("invalid cursor format")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", errors.New("invalid cursor timestamp")
	}
	return ts.UTC(), parts[1], nil
}

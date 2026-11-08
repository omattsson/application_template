package models

import (
	"context"
	"time"
)

// MaxAuditEntityIDLen is the size of audit_logs.entity_id. A UUID has 36
// characters; a numeric item ID fewer.
const MaxAuditEntityIDLen = 64

// AuditLog records a user action for auditing purposes.
type AuditLog struct {
	Timestamp  time.Time `json:"timestamp" gorm:"not null;index:idx_audit_logs_timestamp;index:idx_audit_logs_action_timestamp,priority:2,sort:desc"`
	ID         string    `json:"id" gorm:"primaryKey;size:36"`
	UserID     string    `json:"user_id" gorm:"size:36;index:idx_audit_logs_user_id"`
	Username   string    `json:"username" gorm:"size:100"`
	Action     string    `json:"action" gorm:"size:100;index:idx_audit_logs_action_timestamp,priority:1"`
	EntityType string    `json:"entity_type" gorm:"size:100;index:idx_audit_logs_entity,priority:1"`
	EntityID   string    `json:"entity_id" gorm:"size:64;index:idx_audit_logs_entity,priority:2"`
	Details    string    `json:"details" gorm:"type:longtext"`
}

// AuditLogFilters holds optional filters for querying audit logs.
type AuditLogFilters struct {
	StartDate  *time.Time
	EndDate    *time.Time
	UserID     string
	EntityType string
	EntityID   string
	Action     string
	// Cursor is the opaque NextCursor of the previous page. With a cursor,
	// Offset is ignored and the result has no total.
	Cursor string
	// Limit is the page size: DefaultPageSize when <= 0, at most MaxPageSize.
	Limit  int
	Offset int
}

// PaginatedAuditLogs wraps a page of audit log results with pagination metadata.
type PaginatedAuditLogs struct {
	NextCursor string     `json:"next_cursor,omitempty"`
	Data       []AuditLog `json:"data"`
	Total      int64      `json:"total"`
	Limit      int        `json:"limit"`
	Offset     int        `json:"offset"`
}

// AuditLogResult holds the result of an audit log list query including cursor pagination metadata.
type AuditLogResult struct {
	NextCursor string
	Data       []AuditLog
	// Total is the number of matches, or -1 for a cursor page.
	Total int64
}

// AuditLogRepository defines data access operations for audit logs. The log
// is append-only: there is no update or delete.
type AuditLogRepository interface {
	// Create validates the log and sets ID (when empty) and Timestamp (when
	// zero).
	Create(ctx context.Context, log *AuditLog) error
	// List returns one page of the logs that match filters, newest first.
	// An invalid cursor is dberrors.ErrValidation.
	List(ctx context.Context, filters AuditLogFilters) (*AuditLogResult, error)
}

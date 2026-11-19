package models

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// OutboxEvent is one WebSocket event waiting for delivery (table
// outbox_events). With OUTBOX_ENABLED=true the Items repository writes a row
// in the transaction of every create, update and delete; every replica polls
// the table and sends the message to the sockets of its own hub. Its
// definition is copied into migration 20261117000002 (outboxEventV1); a
// change here needs a new migration, and TestOutboxEventsMatchModel fails
// without one.
//
// The ID is an auto increment, so a poller reads the rows after its cursor in
// ID order. Transactions can commit out of ID order: the poller handles the
// gaps (internal/outbox).
type OutboxEvent struct {
	// CreatedAt is the time of the insert, from the database server clock
	// (UTC_TIMESTAMP on MySQL), never from the replica's clock.
	CreatedAt time.Time `json:"created_at" gorm:"type:datetime(3);not null;index:idx_outbox_events_created_at"`
	// ID is the auto increment key and the delivery order.
	ID uint64 `json:"id" gorm:"primaryKey;autoIncrement"`
	// EventType is the message type (for example "item.created"). It is for
	// operators who read the table; delivery sends Message as it is.
	EventType string `json:"event_type" gorm:"size:64;not null"`
	// Message is the JSON {"type": ..., "payload": ...} that the hub sends
	// to the sockets, as final bytes, so every replica sends the same text.
	Message string `json:"message" gorm:"type:text;not null"`
}

// TableName returns the table name of OutboxEvent.
func (OutboxEvent) TableName() string { return "outbox_events" }

// OutboxRepository reads and cleans the outbox. The write side is the change
// hook of the repository that owns the change (WithChangeHook), which inserts
// the row in the same transaction.
type OutboxRepository interface {
	// MaxID returns the highest event ID, or 0 for an empty table.
	MaxID(ctx context.Context) (uint64, error)
	// ListAfter returns at most limit events with ID > after, in ID order.
	ListAfter(ctx context.Context, after uint64, limit int) ([]OutboxEvent, error)
	// ListByIDs returns the events with the given IDs (those that exist), in
	// ID order. It is for the IDs that a poller skipped.
	ListByIDs(ctx context.Context, ids []uint64) ([]OutboxEvent, error)
	// Age returns how old the event id is by the database clock, or false
	// when it does not exist (deleted).
	Age(ctx context.Context, id uint64) (time.Duration, bool, error)
	// DeleteOlderThan deletes at most limit events older than maxAge (database
	// time), oldest first, and returns the number deleted.
	DeleteOlderThan(ctx context.Context, maxAge time.Duration, limit int) (int64, error)
}

// ChangeKind is the kind of a change that a ChangeHook sees.
type ChangeKind string

// The changes of the Items repository.
const (
	ChangeCreated ChangeKind = "created"
	ChangeUpdated ChangeKind = "updated"
	ChangeDeleted ChangeKind = "deleted"
)

// ChangeHook runs inside the transaction of a create, update or delete, after
// the change succeeded and before the commit, with the handle of that
// transaction and the entity in its final state (ID, version and timestamps
// set). An error rolls the whole change back. The repository runs the whole
// transaction again after a deadlock or lock wait timeout, so the hook must
// use only tx, must keep no state of its own and must have no side effect
// outside the database.
type ChangeHook func(tx *gorm.DB, kind ChangeKind, entity interface{}) error

// RepositoryOption configures a GenericRepository at construction.
type RepositoryOption func(*GenericRepository)

// WithChangeHook makes the repository run hook inside the transaction of
// every create, update and delete. A nil hook means none. The
// models.Repository interface does not change.
func WithChangeHook(hook ChangeHook) RepositoryOption {
	return func(r *GenericRepository) { r.hook = hook }
}

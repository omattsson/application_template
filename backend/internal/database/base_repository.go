package database

import (
	"context"

	"backend/internal/models"

	"gorm.io/gorm"
)

// BaseRepository gives a repository the lock retry for its writes. Embed it
// in a repository struct (for example one keyed by a string UUID) and write
// through Tx or Write.
//
// The retry rule: each attempt is one whole statement or one whole
// transaction, because InnoDB rolls back the whole transaction on a deadlock
// (1213) and may on a lock wait timeout (1205). fn runs again from the start
// on a retry, so it must reset any in-memory state it changed (counters,
// generated IDs it relies on being new, version fields) and must have no
// side effect outside the database.
type BaseRepository struct {
	DB *gorm.DB
}

// NewBaseRepository returns a BaseRepository on db.
func NewBaseRepository(db *gorm.DB) *BaseRepository {
	return &BaseRepository{DB: db}
}

// Tx runs fn in one transaction with ctx and runs the whole transaction
// again after a deadlock or lock wait timeout (models.RetryOnLock). A non-nil
// error from fn rolls the transaction back and, unless it is a lock error, is
// returned at once.
//
// fn must use tx for every statement, must reset any in-memory state it
// changed, and must not retry a statement itself: each attempt is a whole
// transaction.
func (b *BaseRepository) Tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return models.RetryOnLock(ctx, func() error {
		return b.DB.WithContext(ctx).Transaction(fn)
	})
}

// Write runs fn, one write statement, with a db bound to ctx, and runs it
// again after a deadlock or lock wait timeout. Use Tx for more than one
// statement: a retry of a single statement inside a larger transaction is
// not safe. fn must reset any in-memory state it changed.
func (b *BaseRepository) Write(ctx context.Context, fn func(db *gorm.DB) error) error {
	return models.RetryOnLock(ctx, func() error {
		return fn(b.DB.WithContext(ctx))
	})
}

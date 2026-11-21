package database

import (
	"context"
	"database/sql"

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
//
// When DB is already inside a transaction (a repository built on the tx of
// an outer Tx), Tx runs fn once in a SAVEPOINT and does not retry: InnoDB
// rolled back the whole outer transaction on a deadlock, so the lock error
// goes up to the outer Tx, which runs again from the start.
func (b *BaseRepository) Tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if inTransaction(b.DB) {
		return b.DB.WithContext(ctx).Transaction(fn)
	}
	return models.RetryOnLock(ctx, func() error {
		return b.DB.WithContext(ctx).Transaction(fn)
	})
}

// TxOpts is Tx with transaction options (for example an isolation level for
// one transaction). A nested call runs in a SAVEPOINT and ignores opts: the
// outer transaction keeps its own level. Every attempt of a retry begins with
// opts again.
func (b *BaseRepository) TxOpts(ctx context.Context, opts *sql.TxOptions, fn func(tx *gorm.DB) error) error {
	if inTransaction(b.DB) {
		return b.DB.WithContext(ctx).Transaction(fn)
	}
	return models.RetryOnLock(ctx, func() error {
		return b.DB.WithContext(ctx).Transaction(fn, opts)
	})
}

// Write runs fn, one write statement, with a db bound to ctx, and runs it
// again after a deadlock or lock wait timeout. Use Tx for more than one
// statement: a retry of a single statement inside a larger transaction is
// not safe. fn must reset any in-memory state it changed.
//
// When DB is already inside a transaction, Write runs fn once and returns a
// lock error to the outer Tx (see Tx).
func (b *BaseRepository) Write(ctx context.Context, fn func(db *gorm.DB) error) error {
	if inTransaction(b.DB) {
		return fn(b.DB.WithContext(ctx))
	}
	return models.RetryOnLock(ctx, func() error {
		return fn(b.DB.WithContext(ctx))
	})
}

// inTransaction reports whether db runs inside a transaction: its connection
// pool is then a *sql.Tx (or GORM's prepared-statement tx), which commits.
func inTransaction(db *gorm.DB) bool {
	_, ok := db.Statement.ConnPool.(gorm.TxCommitter)
	return ok
}

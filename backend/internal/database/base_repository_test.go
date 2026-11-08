package database

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// baseRepoRow is a test-only model with a string ID and a unique key.
type baseRepoRow struct {
	ID       string `gorm:"primaryKey;size:36"`
	Username string `gorm:"size:64;not null;uniqueIndex"`
}

// openBaseRepoDB opens an in-memory SQLite database on one connection, so a
// transaction and the checks after it see the same database.
func openBaseRepoDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&baseRepoRow{}))
	return db
}

// failCreateOnCall makes CREATE number n (1-based) fail with err.
func failCreateOnCall(t *testing.T, db *gorm.DB, n int32, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:fail_create_n", func(tx *gorm.DB) {
		if calls.Add(1) == n {
			_ = tx.AddError(err)
		}
	}))
	return &calls
}

func countRows(t *testing.T, db *gorm.DB, username string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&baseRepoRow{}).Where("username = ?", username).Count(&n).Error)
	return n
}

func TestBaseRepositoryTxRetriesWholeTransactionOnDeadlock(t *testing.T) {
	t.Parallel()
	db := openBaseRepoDB(t)
	repo := NewBaseRepository(db)

	// The second statement of the first attempt deadlocks. The server rolled
	// back the whole transaction, so the first statement must run again too.
	creates := failCreateOnCall(t, db, 2, &mysql.MySQLError{Number: 1213, Message: "Deadlock found"})

	var attempts int
	err := repo.Tx(context.Background(), func(tx *gorm.DB) error {
		attempts++
		if err := tx.Create(&baseRepoRow{ID: "a", Username: "alice"}).Error; err != nil {
			return err
		}
		return tx.Create(&baseRepoRow{ID: "b", Username: "bob"}).Error
	})

	require.NoError(t, err)
	assert.Equal(t, 2, attempts, "the whole transaction runs again")
	assert.Equal(t, int32(4), creates.Load(), "two statements per attempt")
	assert.Equal(t, int64(1), countRows(t, db, "alice"), "the first statement is applied once")
	assert.Equal(t, int64(1), countRows(t, db, "bob"))
}

func TestBaseRepositoryTxDoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		fnErr   func(tx *gorm.DB) error
		wantIs  error
		wantDup bool
	}{
		{name: "plain error", fnErr: func(*gorm.DB) error { return errBoom }, wantIs: errBoom},
		{name: "duplicate key", fnErr: func(tx *gorm.DB) error {
			return tx.Create(&baseRepoRow{ID: "dup", Username: "taken"}).Error
		}, wantDup: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openBaseRepoDB(t)
			require.NoError(t, db.Create(&baseRepoRow{ID: "first", Username: "taken"}).Error)
			repo := NewBaseRepository(db)

			var attempts int
			err := repo.Tx(context.Background(), func(tx *gorm.DB) error {
				attempts++
				if err := tx.Create(&baseRepoRow{ID: fmt.Sprintf("x%d", attempts), Username: "rolled-back"}).Error; err != nil {
					return err
				}
				return tt.fnErr(tx)
			})

			require.Error(t, err)
			assert.Equal(t, 1, attempts, "not a lock error: no retry")
			if tt.wantIs != nil {
				assert.ErrorIs(t, err, tt.wantIs)
			}
			if tt.wantDup {
				assert.True(t, IsDuplicateKey(err))
			}
			assert.Equal(t, int64(0), countRows(t, db, "rolled-back"), "the transaction is rolled back")
		})
	}
}

func TestBaseRepositoryTxGivesUpAfterRetries(t *testing.T) {
	t.Parallel()
	db := openBaseRepoDB(t)
	repo := NewBaseRepository(db)

	var attempts int
	err := repo.Tx(context.Background(), func(*gorm.DB) error {
		attempts++
		return &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	})

	var mysqlErr *mysql.MySQLError
	require.ErrorAs(t, err, &mysqlErr)
	assert.Equal(t, uint16(1205), mysqlErr.Number)
	assert.Equal(t, 4, attempts, "first attempt and 3 retries")
}

func TestBaseRepositoryTxStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	db := openBaseRepoDB(t)
	repo := NewBaseRepository(db)

	ctx, cancel := context.WithCancel(context.Background())
	var attempts int
	err := repo.Tx(ctx, func(*gorm.DB) error {
		attempts++
		cancel()
		return &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
	})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts)
}

func TestBaseRepositoryWrite(t *testing.T) {
	t.Parallel()

	t.Run("retries a deadlocked statement once", func(t *testing.T) {
		t.Parallel()
		db := openBaseRepoDB(t)
		repo := NewBaseRepository(db)
		creates := failCreateOnCall(t, db, 1, fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1213}))

		err := repo.Write(context.Background(), func(db *gorm.DB) error {
			return db.Create(&baseRepoRow{ID: "w", Username: "writer"}).Error
		})

		require.NoError(t, err)
		assert.Equal(t, int32(2), creates.Load())
		assert.Equal(t, int64(1), countRows(t, db, "writer"))
	})

	t.Run("uses the context", func(t *testing.T) {
		t.Parallel()
		db := openBaseRepoDB(t)
		repo := NewBaseRepository(db)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := repo.Write(ctx, func(db *gorm.DB) error {
			return db.Create(&baseRepoRow{ID: "c", Username: "cancelled"}).Error
		})

		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("duplicate key is returned at once", func(t *testing.T) {
		t.Parallel()
		db := openBaseRepoDB(t)
		repo := NewBaseRepository(db)
		require.NoError(t, db.Create(&baseRepoRow{ID: "1", Username: "same"}).Error)

		var attempts int
		err := repo.Write(context.Background(), func(db *gorm.DB) error {
			attempts++
			return db.Create(&baseRepoRow{ID: "2", Username: "same"}).Error
		})

		require.Error(t, err)
		assert.True(t, IsDuplicateKey(err))
		assert.Equal(t, 1, attempts)
	})
}

// The repository error mapping turns a duplicate key on SQLite and on MySQL
// into the ErrDuplicateKey sentinel, which the handlers answer with 409.
func TestGenericRepositoryMapsDuplicateKey(t *testing.T) {
	t.Parallel()

	t.Run("SQLite unique constraint", func(t *testing.T) {
		t.Parallel()
		db := openBaseRepoDB(t)
		repo := models.NewRepository(db)
		require.NoError(t, repo.Create(context.Background(), &baseRepoRow{ID: "1", Username: "alice"}))

		err := repo.Create(context.Background(), &baseRepoRow{ID: "2", Username: "alice"})

		require.Error(t, err)
		assert.ErrorIs(t, err, dberrors.ErrDuplicateKey)
		assert.NotContains(t, err.Error(), "alice", "the duplicate value is not in the mapped error")
	})

	t.Run("MySQL 1062", func(t *testing.T) {
		t.Parallel()
		db := openBaseRepoDB(t)
		repo := models.NewRepository(db)
		failCreateOnCall(t, db, 1, &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'alice' for key 'username'"})

		err := repo.Create(context.Background(), &baseRepoRow{ID: "1", Username: "alice"})

		assert.ErrorIs(t, err, dberrors.ErrDuplicateKey)
	})

	t.Run("text that only looks like a duplicate", func(t *testing.T) {
		t.Parallel()
		db := openBaseRepoDB(t)
		repo := models.NewRepository(db)
		failCreateOnCall(t, db, 1, errors.New("Duplicate entry 'alice' for key 'username'"))

		err := repo.Create(context.Background(), &baseRepoRow{ID: "1", Username: "alice"})

		require.Error(t, err)
		assert.NotErrorIs(t, err, dberrors.ErrDuplicateKey)
	})
}

// A repository built on the tx of an outer transaction must not retry on its
// own: InnoDB rolled back the whole outer transaction on a deadlock, so only
// the outer Tx may run again, from the start.
func TestBaseRepositoryInsideTransactionDoesNotRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		inner func(ctx context.Context, repo *BaseRepository, innerCalls *int) error
	}{
		{name: "Tx", inner: func(ctx context.Context, repo *BaseRepository, innerCalls *int) error {
			return repo.Tx(ctx, func(tx *gorm.DB) error {
				*innerCalls++
				return tx.Create(&baseRepoRow{ID: "in", Username: "inner"}).Error
			})
		}},
		{name: "Write", inner: func(ctx context.Context, repo *BaseRepository, innerCalls *int) error {
			return repo.Write(ctx, func(db *gorm.DB) error {
				*innerCalls++
				return db.Create(&baseRepoRow{ID: "in", Username: "inner"}).Error
			})
		}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openBaseRepoDB(t)
			// The first CREATE (of the outer transaction) succeeds, the second
			// (the inner one) deadlocks once.
			creates := failCreateOnCall(t, db, 2, &mysql.MySQLError{Number: 1213, Message: "Deadlock found"})
			ctx := context.Background()

			var outerCalls, innerCalls int
			err := NewBaseRepository(db).Tx(ctx, func(tx *gorm.DB) error {
				outerCalls++
				if err := tx.Create(&baseRepoRow{ID: "out", Username: "outer"}).Error; err != nil {
					return err
				}
				return tt.inner(ctx, NewBaseRepository(tx), &innerCalls)
			})

			require.NoError(t, err)
			assert.Equal(t, 2, outerCalls, "the outer transaction runs again")
			assert.Equal(t, 2, innerCalls, "once per outer attempt, never retried inside")
			assert.Equal(t, int32(4), creates.Load())
			assert.Equal(t, int64(1), countRows(t, db, "outer"))
			assert.Equal(t, int64(1), countRows(t, db, "inner"))
		})
	}
}

func TestBaseRepositoryInsideTransactionReturnsLockError(t *testing.T) {
	t.Parallel()
	db := openBaseRepoDB(t)
	ctx := context.Background()

	var innerCalls int
	var innerErr error
	err := db.Transaction(func(tx *gorm.DB) error {
		innerErr = NewBaseRepository(tx).Write(ctx, func(*gorm.DB) error {
			innerCalls++
			return &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
		})
		return innerErr
	})

	require.Error(t, err)
	assert.Equal(t, 1, innerCalls, "no retry inside a transaction")
	var mysqlErr *mysql.MySQLError
	require.ErrorAs(t, innerErr, &mysqlErr)
	assert.Equal(t, uint16(1205), mysqlErr.Number)
}

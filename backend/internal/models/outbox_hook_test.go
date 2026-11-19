package models

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"backend/pkg/dberrors"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// hookRow is the table the test hook writes into, in the transaction of the
// change.
type hookRow struct {
	Kind   string
	ID     uint `gorm:"primaryKey"`
	ItemID uint
}

// openHookDB opens an in-memory SQLite database on one connection, so a
// transaction and the checks after it see the same database.
func openHookDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&Item{}, &hookRow{}))
	return db
}

// recordingHook writes one hookRow per call and remembers what it saw.
type recordingHook struct {
	err   func(call int) error
	kinds []ChangeKind
	items []Item
	calls atomic.Int32
}

func (h *recordingHook) hook(tx *gorm.DB, kind ChangeKind, entity interface{}) error {
	call := int(h.calls.Add(1))
	item, ok := entity.(*Item)
	if !ok {
		return nil
	}
	h.kinds = append(h.kinds, kind)
	h.items = append(h.items, *item)
	if err := tx.Create(&hookRow{Kind: string(kind), ItemID: item.ID}).Error; err != nil {
		return err
	}
	if h.err != nil {
		return h.err(call)
	}
	return nil
}

func countRows(t *testing.T, db *gorm.DB, model interface{}) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(model).Count(&n).Error)
	return n
}

func TestGenericRepositoryChangeHook(t *testing.T) {
	t.Parallel()

	t.Run("create writes the hook row in the same transaction", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		h := &recordingHook{}
		repo := NewRepository(db, WithChangeHook(h.hook))

		item := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(context.Background(), item))

		require.Len(t, h.items, 1)
		assert.Equal(t, ChangeCreated, h.kinds[0])
		assert.NotZero(t, h.items[0].ID, "the hook sees the generated ID")
		assert.Equal(t, uint(1), h.items[0].Version)
		assert.Equal(t, int64(1), countRows(t, db, &hookRow{}))
	})

	t.Run("a hook error rolls the change back and resets the entity", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		h := &recordingHook{err: func(int) error { return errors.New("outbox down") }}
		repo := NewRepository(db, WithChangeHook(h.hook))

		item := &Item{Name: "a", Price: 1, Version: 1}
		err := repo.Create(context.Background(), item)

		require.Error(t, err)
		var dbErr *dberrors.DatabaseError
		assert.ErrorAs(t, err, &dbErr)
		assert.Zero(t, countRows(t, db, &Item{}), "no item")
		assert.Zero(t, countRows(t, db, &hookRow{}), "no hook row")
		assert.Zero(t, item.ID, "the generated ID is reset")
	})

	t.Run("a validation error never reaches the hook", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		h := &recordingHook{}
		repo := NewRepository(db, WithChangeHook(h.hook))

		err := repo.Create(context.Background(), &Item{Name: "", Version: 1})

		assert.ErrorIs(t, err, dberrors.ErrValidation)
		assert.Zero(t, h.calls.Load())
		assert.Zero(t, countRows(t, db, &hookRow{}))
	})

	t.Run("a deadlock in the hook runs the whole transaction again once", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		h := &recordingHook{err: func(call int) error {
			if call == 1 {
				return &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
			}
			return nil
		}}
		repo := NewRepository(db, WithChangeHook(h.hook))

		item := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(context.Background(), item))

		assert.Equal(t, int32(2), h.calls.Load())
		assert.Equal(t, int64(1), countRows(t, db, &Item{}), "one item")
		assert.Equal(t, int64(1), countRows(t, db, &hookRow{}), "one hook row")
	})

	t.Run("update runs the hook with the new version", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		h := &recordingHook{}
		repo := NewRepository(db, WithChangeHook(h.hook))
		item := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(context.Background(), item))

		item.Name = "b"
		require.NoError(t, repo.Update(context.Background(), item))

		require.Len(t, h.items, 2)
		assert.Equal(t, ChangeUpdated, h.kinds[1])
		assert.Equal(t, "b", h.items[1].Name)
		assert.Equal(t, uint(2), h.items[1].Version)
		assert.Equal(t, int64(2), countRows(t, db, &hookRow{}))
	})

	t.Run("a version mismatch leaves no hook row", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		h := &recordingHook{}
		repo := NewRepository(db, WithChangeHook(h.hook))
		item := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(context.Background(), item))

		stale := &Item{Base: Base{ID: item.ID}, Name: "x", Price: 2, Version: 7}
		err := repo.Update(context.Background(), stale)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "version mismatch")
		assert.Equal(t, uint(7), stale.Version, "the version is reset")
		assert.Equal(t, int64(1), countRows(t, db, &hookRow{}), "only the create row")
	})

	t.Run("a hook error on update rolls the change back", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		fail := false
		h := &recordingHook{err: func(int) error {
			if fail {
				return errors.New("outbox down")
			}
			return nil
		}}
		repo := NewRepository(db, WithChangeHook(h.hook))
		item := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(context.Background(), item))

		fail = true
		item.Name = "b"
		require.Error(t, repo.Update(context.Background(), item))

		assert.Equal(t, uint(1), item.Version, "the version is reset")
		var stored Item
		require.NoError(t, db.First(&stored, item.ID).Error)
		assert.Equal(t, "a", stored.Name, "the update is rolled back")
		assert.Equal(t, uint(1), stored.Version)
	})

	t.Run("delete runs the hook, a missing item does not", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		h := &recordingHook{}
		repo := NewRepository(db, WithChangeHook(h.hook))
		item := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(context.Background(), item))

		require.NoError(t, repo.Delete(context.Background(), &Item{Base: Base{ID: item.ID}}))
		assert.Equal(t, ChangeDeleted, h.kinds[1])
		assert.Equal(t, item.ID, h.items[1].ID)
		assert.Equal(t, int64(2), countRows(t, db, &hookRow{}))

		err := repo.Delete(context.Background(), &Item{Base: Base{ID: 999}})
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
		assert.Equal(t, int64(2), countRows(t, db, &hookRow{}), "no row for a missing item")
		assert.Equal(t, int32(2), h.calls.Load(), "the hook ran for the create and the delete only")
	})

	t.Run("without a hook nothing changes", func(t *testing.T) {
		t.Parallel()
		db := openHookDB(t)
		repo := NewRepository(db)
		item := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(context.Background(), item))
		item.Name = "b"
		require.NoError(t, repo.Update(context.Background(), item))
		require.NoError(t, repo.Delete(context.Background(), &Item{Base: Base{ID: item.ID}}))
		assert.False(t, repo.(*GenericRepository).HasChangeHook())
	})

	t.Run("a nil option means none", func(t *testing.T) {
		t.Parallel()
		repo := NewRepository(openHookDB(t), WithChangeHook(nil))
		assert.False(t, repo.(*GenericRepository).HasChangeHook())
		repo = NewRepository(openHookDB(t), WithChangeHook(func(*gorm.DB, ChangeKind, interface{}) error { return nil }))
		assert.True(t, repo.(*GenericRepository).HasChangeHook())
	})
}

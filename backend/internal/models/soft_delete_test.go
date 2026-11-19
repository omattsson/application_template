package models

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"backend/pkg/dberrors"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// softFixture is the test-only entity of the soft delete pattern: a name and
// an optimistic lock version. The template ships no soft delete entity.
type softFixture struct {
	Base
	Name    string `gorm:"size:255;not null"`
	Version uint   `gorm:"not null;default:1"`
}

func (f *softFixture) GetVersion() uint  { return f.Version }
func (f *softFixture) SetVersion(v uint) { f.Version = v }

// softPlain has no version, so its update takes the plain (non-Versionable)
// path.
type softPlain struct {
	Base
	Name string `gorm:"size:255;not null"`
}

// deadlockError is the MySQL deadlock error (1213) that RetryOnLock retries.
func deadlockError() error {
	return &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
}

func openSoftDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&Item{}, &softFixture{}, &softPlain{}, &hookRow{}))
	return db
}

func newSoftRepo(db *gorm.DB, opts ...RepositoryOption) *GenericRepository {
	opts = append([]RepositoryOption{WithSoftDeleteOf(&softFixture{})}, opts...)
	return NewRepositoryWithFilterFields(db, []string{"name"}, opts...).(*GenericRepository)
}

// rawDeletedAt reads deleted_at straight from the table, bypassing the
// repository, and reports whether the row exists at all.
func rawDeletedAt(t *testing.T, db *gorm.DB, table string, id uint) (deletedAt *time.Time, exists bool) {
	t.Helper()
	var rows []Base
	require.NoError(t, db.Table(table).Select("id", "deleted_at").Where("id = ?", id).Find(&rows).Error)
	if len(rows) == 0 {
		return nil, false
	}
	return rows[0].DeletedAt, true
}

func TestSoftDeleteOptionEnabled(t *testing.T) {
	t.Parallel()
	db := openSoftDB(t)
	tests := []struct {
		repo *GenericRepository
		name string
		want bool
	}{
		{name: "default repository", repo: NewRepository(db).(*GenericRepository), want: false},
		{name: "WithSoftDelete", repo: NewRepository(db, WithSoftDelete()).(*GenericRepository), want: true},
		{name: "WithSoftDeleteOf", repo: newSoftRepo(db), want: true},
		{name: "WithSoftDeleteOf nil falls back to Item", repo: NewRepository(db, WithSoftDeleteOf(nil)).(*GenericRepository), want: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.repo.SoftDeleteEnabled())
			var _ SoftDeleter = tt.repo
		})
	}
}

func TestSoftDeleteDeleteMarksTheRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := newSoftRepo(db)
	f := &softFixture{Name: "a", Version: 1}
	require.NoError(t, repo.Create(ctx, f))

	before := time.Now().UTC().Add(-2 * time.Second)
	require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))

	deletedAt, exists := rawDeletedAt(t, db, "soft_fixtures", f.ID)
	require.True(t, exists, "the row stays in the table")
	require.NotNil(t, deletedAt)
	assert.True(t, deletedAt.After(before), "deleted_at is the database clock: %s", deletedAt)
	assert.WithinDuration(t, time.Now().UTC(), *deletedAt, 5*time.Second)
	assert.Equal(t, time.UTC, deletedAt.Location())
}

func TestSoftDeleteDeleteMissingOrDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := newSoftRepo(db)
	f := &softFixture{Name: "a", Version: 1}
	require.NoError(t, repo.Create(ctx, f))
	require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))
	first, _ := rawDeletedAt(t, db, "soft_fixtures", f.ID)
	require.NotNil(t, first)
	// deleted_at has millisecond precision: wait, so a rewrite would show.
	time.Sleep(15 * time.Millisecond)

	tests := []struct {
		entity interface{}
		name   string
	}{
		{name: "already deleted", entity: &softFixture{Base: Base{ID: f.ID}}},
		{name: "missing", entity: &softFixture{Base: Base{ID: 999}}},
		{name: "zero id never touches another row", entity: &softFixture{}},
	}
	// The subtests run one after the other (no t.Parallel): the comparison
	// below must run after the second delete.
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := repo.Delete(ctx, tt.entity)
			assert.ErrorIs(t, err, dberrors.ErrNotFound)
		})
	}

	second, _ := rawDeletedAt(t, db, "soft_fixtures", f.ID)
	require.NotNil(t, second)
	assert.True(t, first.Equal(*second), "a second delete does not move deleted_at: %s vs %s", first, second)
}

func TestSoftDeleteZeroIDLeavesLiveRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := newSoftRepo(db)
	f := &softFixture{Name: "a", Version: 1}
	require.NoError(t, repo.Create(ctx, f))

	assert.ErrorIs(t, repo.Delete(ctx, &softFixture{}), dberrors.ErrNotFound)

	var got softFixture
	assert.NoError(t, repo.FindByID(ctx, f.ID, &got), "the live row is untouched")
}

func TestSoftDeleteReadsHideDeletedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := newSoftRepo(db)
	keep := &softFixture{Name: "keep", Version: 1}
	gone := &softFixture{Name: "gone", Version: 1}
	require.NoError(t, repo.Create(ctx, keep))
	require.NoError(t, repo.Create(ctx, gone))
	require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: gone.ID}}))

	t.Run("FindByID", func(t *testing.T) {
		var got softFixture
		assert.NoError(t, repo.FindByID(ctx, keep.ID, &got))
		err := repo.FindByID(ctx, gone.ID, &softFixture{})
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
	})

	t.Run("deprecated List", func(t *testing.T) {
		var got []softFixture
		require.NoError(t, repo.List(ctx, &got))
		require.Len(t, got, 1)
		assert.Equal(t, "keep", got[0].Name)

		got = nil
		require.NoError(t, repo.List(ctx, &got, Filter{Field: "name", Op: "exact", Value: "gone"}))
		assert.Empty(t, got, "a filter cannot bring a deleted row back")
	})

	t.Run("Update with the version", func(t *testing.T) {
		err := repo.Update(ctx, &softFixture{Base: Base{ID: gone.ID}, Name: "x", Version: 1})
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
		var raw softFixture
		require.NoError(t, db.First(&raw, gone.ID).Error)
		assert.Equal(t, "gone", raw.Name, "the deleted row is not written")
	})

	t.Run("Update without a version", func(t *testing.T) {
		plain := NewRepositoryWithFilterFields(db, nil, WithSoftDeleteOf(&softPlain{}))
		p := &softPlain{Name: "p"}
		require.NoError(t, plain.Create(ctx, p))
		require.NoError(t, plain.Delete(ctx, &softPlain{Base: Base{ID: p.ID}}))

		err := plain.Update(ctx, &softPlain{Base: Base{ID: p.ID}, Name: "q"})
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
		var rows []softPlain
		require.NoError(t, db.Find(&rows).Error)
		require.Len(t, rows, 1, "Update never inserts a row for a deleted one")
		assert.Equal(t, "p", rows[0].Name)
	})
}

func TestSoftDeleteItemsListPaged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := NewRepository(db, WithSoftDelete())
	var ids []uint
	for i := 0; i < 5; i++ {
		it := &Item{Name: fmt.Sprintf("n%d", i), Price: float64(i + 1), Version: 1}
		require.NoError(t, repo.Create(ctx, it))
		ids = append(ids, it.ID)
	}
	require.NoError(t, repo.Delete(ctx, &Item{Base: Base{ID: ids[1]}}))
	require.NoError(t, repo.Delete(ctx, &Item{Base: Base{ID: ids[3]}}))

	tests := []struct {
		name      string
		filter    ItemFilter
		limit     int
		offset    int
		wantTotal int64
		wantLen   int
	}{
		{name: "all", limit: 0, wantTotal: 3, wantLen: 3},
		{name: "page", limit: 2, offset: 1, wantTotal: 3, wantLen: 2},
		{name: "name filter", filter: ItemFilter{Name: "n1"}, wantTotal: 0, wantLen: 0},
		{name: "price filter", filter: ItemFilter{MinPrice: 3}, wantTotal: 2, wantLen: 2},
		{name: "sorted", filter: ItemFilter{SortBy: "price", SortDesc: true}, wantTotal: 3, wantLen: 3},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []Item
			total, err := repo.ListPaged(ctx, &got, tt.limit, tt.offset, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTotal, total, "the count query hides deleted rows too")
			assert.Len(t, got, tt.wantLen)
			for _, it := range got {
				assert.NotEqual(t, ids[1], it.ID)
				assert.NotEqual(t, ids[3], it.ID)
			}
		})
	}
}

// TestSoftDeleteCreateIgnoresDeletedAt: deleted_at binds from JSON, so a create
// must not insert a row that is already deleted (hidden, outside the unique
// guard, with a back-dated timestamp).
func TestSoftDeleteCreateIgnoresDeletedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("soft delete repository: the row is live", func(t *testing.T) {
		t.Parallel()
		db := openSoftDB(t)
		repo := newSoftRepo(db)
		f := &softFixture{Base: Base{DeletedAt: &past}, Name: "a", Version: 1}
		require.NoError(t, repo.Create(ctx, f))
		assert.Nil(t, f.DeletedAt, "the entity is reset too")

		var got softFixture
		require.NoError(t, repo.FindByID(ctx, f.ID, &got))
		assert.Nil(t, got.DeletedAt)
		raw, exists := rawDeletedAt(t, db, "soft_fixtures", f.ID)
		assert.True(t, exists)
		assert.Nil(t, raw)
	})

	t.Run("soft delete repository with a hook", func(t *testing.T) {
		t.Parallel()
		db := openSoftDB(t)
		h := &recordingHook{}
		repo := NewRepository(db, WithSoftDelete(), WithChangeHook(h.hook))
		it := &Item{Base: Base{DeletedAt: &past}, Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(ctx, it))
		raw, _ := rawDeletedAt(t, db, "items", it.ID)
		assert.Nil(t, raw)
		require.Len(t, h.items, 1)
		assert.Nil(t, h.items[0].DeletedAt)
	})

	t.Run("default repository is unchanged", func(t *testing.T) {
		t.Parallel()
		db := openSoftDB(t)
		repo := NewRepository(db)
		it := &Item{Base: Base{DeletedAt: &past}, Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(ctx, it))
		raw, exists := rawDeletedAt(t, db, "items", it.ID)
		require.True(t, exists)
		require.NotNil(t, raw, "the default repository writes what it is given")
		assert.True(t, raw.Equal(past))
		assert.NotNil(t, it.DeletedAt)
	})
}

func TestSoftDeleteRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := newSoftRepo(db)
	f := &softFixture{Name: "a", Version: 1}
	require.NoError(t, repo.Create(ctx, f))

	t.Run("a live row is NotFound", func(t *testing.T) {
		assert.ErrorIs(t, repo.Restore(ctx, f.ID), dberrors.ErrNotFound)
	})
	t.Run("a missing row is NotFound", func(t *testing.T) {
		assert.ErrorIs(t, repo.Restore(ctx, 999), dberrors.ErrNotFound)
	})
	t.Run("a deleted row comes back with its data and version", func(t *testing.T) {
		require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))
		require.NoError(t, repo.Restore(ctx, f.ID))

		var got softFixture
		require.NoError(t, repo.FindByID(ctx, f.ID, &got))
		assert.Equal(t, "a", got.Name)
		assert.Equal(t, uint(1), got.Version)
		assert.Nil(t, got.DeletedAt)
		assert.ErrorIs(t, repo.Restore(ctx, f.ID), dberrors.ErrNotFound, "twice")
	})
	t.Run("a restored row can be deleted again", func(t *testing.T) {
		require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))
		assert.ErrorIs(t, repo.FindByID(ctx, f.ID, &softFixture{}), dberrors.ErrNotFound)
	})
}

func TestSoftDeletePurgeDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := newSoftRepo(db)
	now := time.Now().UTC().Truncate(time.Millisecond)

	mk := func(name string, deletedAgo *time.Duration) uint {
		f := &softFixture{Name: name, Version: 1}
		require.NoError(t, repo.Create(ctx, f))
		if deletedAgo != nil {
			ts := now.Add(-*deletedAgo)
			require.NoError(t, db.Model(&softFixture{}).Where("id = ?", f.ID).
				UpdateColumn("deleted_at", ts).Error)
		}
		return f.ID
	}
	day := 24 * time.Hour
	d10, d2 := 10*day, 2*day
	live := mk("live", nil)
	old1 := mk("old1", &d10)
	old2 := mk("old2", &d10)
	old3 := mk("old3", &d10)
	recent := mk("recent", &d2)

	cutoff := now.Add(-7 * day)

	t.Run("limit bounds the batch", func(t *testing.T) {
		n, err := repo.PurgeDeleted(ctx, cutoff, 2)
		require.NoError(t, err)
		assert.Equal(t, int64(2), n)
		var left int64
		require.NoError(t, db.Model(&softFixture{}).Where("deleted_at < ?", cutoff).Count(&left).Error)
		assert.Equal(t, int64(1), left)
	})
	t.Run("a short batch ends the purge", func(t *testing.T) {
		n, err := repo.PurgeDeleted(ctx, cutoff, 2)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)
		n, err = repo.PurgeDeleted(ctx, cutoff, 2)
		require.NoError(t, err)
		assert.Zero(t, n, "idempotent")
	})
	t.Run("only rows older than the cutoff are removed", func(t *testing.T) {
		for _, id := range []uint{old1, old2, old3} {
			_, exists := rawDeletedAt(t, db, "soft_fixtures", id)
			assert.False(t, exists)
		}
		_, exists := rawDeletedAt(t, db, "soft_fixtures", recent)
		assert.True(t, exists, "recently deleted rows stay")
		_, exists = rawDeletedAt(t, db, "soft_fixtures", live)
		assert.True(t, exists, "live rows stay")
	})
	t.Run("a later cutoff takes the recent row", func(t *testing.T) {
		n, err := repo.PurgeDeleted(ctx, now.Add(-day), 10)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)
		_, exists := rawDeletedAt(t, db, "soft_fixtures", live)
		assert.True(t, exists)
	})
	t.Run("a non-positive limit is a validation error", func(t *testing.T) {
		for _, limit := range []int{0, -1} {
			_, err := repo.PurgeDeleted(ctx, cutoff, limit)
			assert.ErrorIs(t, err, dberrors.ErrValidation)
		}
	})
}

func TestSoftDeleteRequiresTheOption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := NewRepository(db)
	var sd SoftDeleter = repo.(*GenericRepository)

	err := sd.Restore(ctx, 1)
	assert.ErrorIs(t, err, dberrors.ErrNotImplemented)
	_, err = sd.PurgeDeleted(ctx, time.Now(), 10)
	assert.ErrorIs(t, err, dberrors.ErrNotImplemented)
}

func TestSoftDeleteOptimisticLockStillWorks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := newSoftRepo(db)
	f := &softFixture{Name: "a", Version: 1}
	require.NoError(t, repo.Create(ctx, f))

	t.Run("a stale version is a mismatch, not NotFound", func(t *testing.T) {
		stale := &softFixture{Base: Base{ID: f.ID}, Name: "x", Version: 9}
		err := repo.Update(ctx, stale)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version mismatch")
		assert.NotErrorIs(t, err, dberrors.ErrNotFound)
		assert.Equal(t, uint(9), stale.Version)
	})
	t.Run("the current version updates and bumps", func(t *testing.T) {
		f.Name = "b"
		require.NoError(t, repo.Update(ctx, f))
		assert.Equal(t, uint(2), f.Version)
		var got softFixture
		require.NoError(t, repo.FindByID(ctx, f.ID, &got))
		assert.Equal(t, "b", got.Name)
		assert.Equal(t, uint(2), got.Version)
	})
	t.Run("an update never changes deleted_at", func(t *testing.T) {
		ts := time.Now().UTC()
		f.DeletedAt = &ts
		f.Name = "c"
		require.NoError(t, repo.Update(ctx, f))
		deletedAt, _ := rawDeletedAt(t, db, "soft_fixtures", f.ID)
		assert.Nil(t, deletedAt, "only Delete and Restore write deleted_at")
	})
	t.Run("an update after a delete is NotFound", func(t *testing.T) {
		require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))
		f.Name = "d"
		assert.ErrorIs(t, repo.Update(ctx, f), dberrors.ErrNotFound)
	})
	t.Run("a restore keeps the version, so the lock continues", func(t *testing.T) {
		require.NoError(t, repo.Restore(ctx, f.ID))
		var got softFixture
		require.NoError(t, repo.FindByID(ctx, f.ID, &got))
		got.Name = "e"
		require.NoError(t, repo.Update(ctx, &got))
		assert.Equal(t, uint(4), got.Version)
	})
}

func TestSoftDeleteChangeHook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("runs in the transaction of the soft delete", func(t *testing.T) {
		t.Parallel()
		db := openSoftDB(t)
		kinds := []ChangeKind{}
		hook := func(tx *gorm.DB, kind ChangeKind, entity interface{}) error {
			kinds = append(kinds, kind)
			f, ok := entity.(*softFixture)
			if !ok {
				return nil
			}
			return tx.Create(&hookRow{Kind: string(kind), ItemID: f.ID}).Error
		}
		repo := newSoftRepo(db, WithChangeHook(hook))
		f := &softFixture{Name: "a", Version: 1}
		require.NoError(t, repo.Create(ctx, f))

		require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))

		assert.Equal(t, []ChangeKind{ChangeCreated, ChangeDeleted}, kinds)
		assert.Equal(t, int64(2), countRows(t, db, &hookRow{}))
		deletedAt, exists := rawDeletedAt(t, db, "soft_fixtures", f.ID)
		assert.True(t, exists)
		assert.NotNil(t, deletedAt)
	})

	t.Run("a deleted or missing row leaves no event", func(t *testing.T) {
		t.Parallel()
		db := openSoftDB(t)
		h := &recordingHook{}
		repo := NewRepository(db, WithSoftDelete(), WithChangeHook(h.hook))
		it := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(ctx, it))
		require.NoError(t, repo.Delete(ctx, &Item{Base: Base{ID: it.ID}}))
		assert.ErrorIs(t, repo.Delete(ctx, &Item{Base: Base{ID: it.ID}}), dberrors.ErrNotFound)
		assert.ErrorIs(t, repo.Delete(ctx, &Item{Base: Base{ID: 999}}), dberrors.ErrNotFound)

		assert.Equal(t, int32(2), h.calls.Load(), "create and one delete")
		assert.Equal(t, int64(2), countRows(t, db, &hookRow{}))
	})

	t.Run("a hook error rolls the soft delete back", func(t *testing.T) {
		t.Parallel()
		db := openSoftDB(t)
		fail := false
		h := &recordingHook{err: func(int) error {
			if fail {
				return errors.New("outbox down")
			}
			return nil
		}}
		repo := NewRepository(db, WithSoftDelete(), WithChangeHook(h.hook))
		it := &Item{Name: "a", Price: 1, Version: 1}
		require.NoError(t, repo.Create(ctx, it))

		fail = true
		require.Error(t, repo.Delete(ctx, &Item{Base: Base{ID: it.ID}}))

		deletedAt, exists := rawDeletedAt(t, db, "items", it.ID)
		assert.True(t, exists)
		assert.Nil(t, deletedAt, "the row is still live")
		assert.NoError(t, repo.FindByID(ctx, it.ID, &Item{}))
	})
}

func TestSoftDeleteLockRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	attempts := 0
	hook := func(tx *gorm.DB, kind ChangeKind, entity interface{}) error {
		if kind != ChangeDeleted {
			return nil
		}
		attempts++
		if attempts == 1 {
			return deadlockError()
		}
		return nil
	}
	repo := newSoftRepo(db, WithChangeHook(hook))
	f := &softFixture{Name: "a", Version: 1}
	require.NoError(t, repo.Create(ctx, f))

	require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))

	assert.Equal(t, 2, attempts, "the soft delete runs again after a deadlock")
	deletedAt, _ := rawDeletedAt(t, db, "soft_fixtures", f.ID)
	assert.NotNil(t, deletedAt)
}

func TestSoftDeleteInvalidModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)
	repo := NewRepository(db, WithSoftDeleteOf(softFixture{})).(*GenericRepository)

	err := repo.Restore(ctx, 1)
	assert.ErrorIs(t, err, dberrors.ErrValidation)
	_, err = repo.PurgeDeleted(ctx, time.Now(), 1)
	assert.ErrorIs(t, err, dberrors.ErrValidation)
}

// TestDefaultRepositoryStillHardDeletes: without the option nothing changes.
func TestDefaultRepositoryStillHardDeletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openSoftDB(t)

	t.Run("fixture", func(t *testing.T) {
		repo := NewRepositoryWithFilterFields(db, []string{"name"})
		f := &softFixture{Name: "a", Version: 1}
		require.NoError(t, repo.Create(ctx, f))
		require.NoError(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}))
		_, exists := rawDeletedAt(t, db, "soft_fixtures", f.ID)
		assert.False(t, exists, "the row is gone")
		assert.ErrorIs(t, repo.Delete(ctx, &softFixture{Base: Base{ID: f.ID}}), dberrors.ErrNotFound)
	})

	t.Run("items", func(t *testing.T) {
		repo := NewRepository(db)
		var ids []uint
		for i := 0; i < 4; i++ {
			it := &Item{Name: fmt.Sprintf("hard%d", i), Price: float64(i + 1), Version: 1}
			require.NoError(t, repo.Create(ctx, it))
			ids = append(ids, it.ID)
		}
		require.NoError(t, repo.Delete(ctx, &Item{Base: Base{ID: ids[0]}}))
		_, exists := rawDeletedAt(t, db, "items", ids[0])
		assert.False(t, exists, "the row is gone from the table")

		// A row that carries a deleted_at is still listed: the default
		// repository adds no filter.
		ts := time.Now().UTC()
		require.NoError(t, db.Model(&Item{}).Where("id = ?", ids[1]).UpdateColumn("deleted_at", ts).Error)

		var got []Item
		total, err := repo.ListPaged(ctx, &got, 0, 0, ItemFilter{Name: "hard"})
		require.NoError(t, err)
		assert.Equal(t, int64(3), total)
		assert.Len(t, got, 3)
		assert.NoError(t, repo.FindByID(ctx, ids[1], &Item{}), "FindByID adds no deleted_at condition")

		var listed []Item
		require.NoError(t, repo.List(ctx, &listed, Filter{Field: "name", Op: "like", Value: "hard"}))
		assert.Len(t, listed, 3)
	})
}

// TestDefaultRepositorySQLHasNoDeletedAtCondition pins the SQL of the default repository: no deleted_at
// condition anywhere.
func TestDefaultRepositorySQLHasNoDeletedAtCondition(t *testing.T) {
	t.Parallel()
	db := openSoftDB(t)
	r := NewRepository(db).(*GenericRepository)
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB { return r.live(tx.Model(&Item{})).Find(&[]Item{}) })
	assert.NotContains(t, sql, "deleted_at")

	rs := NewRepository(db, WithSoftDelete()).(*GenericRepository)
	sql = db.ToSQL(func(tx *gorm.DB) *gorm.DB { return rs.live(tx.Model(&Item{})).Find(&[]Item{}) })
	assert.Contains(t, sql, "deleted_at IS NULL")
}

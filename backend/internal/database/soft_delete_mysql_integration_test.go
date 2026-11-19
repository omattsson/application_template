//go:build integration

package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// softNote is the test-only entity of the soft delete pattern (the template
// ships none). Its table is created in the test with the unique index of
// EXTENDING.md, "Adding soft delete to an entity": a generated column that
// holds the name of a live row and NULL for a deleted one. The struct has no
// field for it: MySQL refuses a write to a generated column, and GORM must
// not write it.
type softNote struct {
	models.Base
	Name    string `gorm:"size:255;not null"`
	Version uint   `gorm:"not null;default:1"`
}

func (softNote) TableName() string { return "soft_notes" }

func (n *softNote) GetVersion() uint  { return n.Version }
func (n *softNote) SetVersion(v uint) { n.Version = v }

const softNotesDDL = "CREATE TABLE soft_notes (" +
	"id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY," +
	"created_at DATETIME(3) NULL, updated_at DATETIME(3) NULL, deleted_at DATETIME(3) NULL," +
	"name VARCHAR(255) NOT NULL," +
	"version BIGINT UNSIGNED NOT NULL DEFAULT 1," +
	// NULL for a deleted row, so a deleted name never conflicts; a unique index
	// ignores NULLs.
	"active_name VARCHAR(255) GENERATED ALWAYS AS (IF(deleted_at IS NULL, name, NULL)) STORED," +
	"UNIQUE KEY uq_soft_notes_active_name (active_name)," +
	"KEY idx_soft_notes_deleted_at (deleted_at)," +
	// The list order by name of the live rows: deleted_at first (the condition),
	// then the sort column.
	"KEY idx_soft_notes_live_name (deleted_at, name)" +
	") ENGINE=InnoDB"

func openSoftNotes(t *testing.T) (*gorm.DB, *models.GenericRepository) {
	t.Helper()
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	require.NoError(t, db.DB.Exec("DROP TABLE IF EXISTS soft_notes").Error)
	require.NoError(t, db.DB.Exec(softNotesDDL).Error)
	t.Cleanup(func() { db.DB.Exec("DROP TABLE IF EXISTS soft_notes") })
	repo := models.NewRepositoryWithFilterFields(db.DB, []string{"name"}, models.WithSoftDeleteOf(&softNote{}))
	return db.DB, repo.(*models.GenericRepository)
}

// TestDatabaseMySQLSoftDeleteUniqueName proves the unique-name approach: a
// deleted name is free again, and a restore that would make two live rows with
// the same name is a duplicate key.
func TestDatabaseMySQLSoftDeleteUniqueName(t *testing.T) {
	db, repo := openSoftNotes(t)
	ctx := context.Background()

	first := &softNote{Name: "alpha", Version: 1}
	require.NoError(t, repo.Create(ctx, first))

	dup := &softNote{Name: "alpha", Version: 1}
	err := repo.Create(ctx, dup)
	require.ErrorIs(t, err, dberrors.ErrDuplicateKey, "two live rows cannot share a name")

	require.NoError(t, repo.Delete(ctx, &softNote{Base: models.Base{ID: first.ID}}))
	second := &softNote{Name: "alpha", Version: 1}
	require.NoError(t, repo.Create(ctx, second), "the name of a deleted row is free")

	err = repo.Restore(ctx, first.ID)
	require.ErrorIs(t, err, dberrors.ErrDuplicateKey, "the restore conflicts with the live row")
	assert.ErrorIs(t, repo.FindByID(ctx, first.ID, &softNote{}), dberrors.ErrNotFound, "the first row is still deleted")

	// Two deleted rows with the same name coexist.
	require.NoError(t, repo.Delete(ctx, &softNote{Base: models.Base{ID: second.ID}}))
	var deleted int64
	require.NoError(t, db.Model(&softNote{}).Where("name = ? AND deleted_at IS NOT NULL", "alpha").Count(&deleted).Error)
	assert.Equal(t, int64(2), deleted)

	require.NoError(t, repo.Restore(ctx, first.ID), "the name is free again")
	var got softNote
	require.NoError(t, repo.FindByID(ctx, first.ID, &got))
	assert.Equal(t, "alpha", got.Name)
	assert.ErrorIs(t, repo.Restore(ctx, second.ID), dberrors.ErrDuplicateKey)

	// A create cannot slip past the unique guard with a deleted_at in the body.
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	sneaky := &softNote{Base: models.Base{DeletedAt: &past}, Name: "alpha", Version: 1}
	assert.ErrorIs(t, repo.Create(ctx, sneaky), dberrors.ErrDuplicateKey, "the row is created live, so it conflicts")
}

func TestDatabaseMySQLSoftDeleteBehavior(t *testing.T) {
	db, repo := openSoftNotes(t)
	ctx := context.Background()

	a := &softNote{Name: "a", Version: 1}
	b := &softNote{Name: "b", Version: 1}
	require.NoError(t, repo.Create(ctx, a))
	require.NoError(t, repo.Create(ctx, b))

	require.NoError(t, repo.Delete(ctx, &softNote{Base: models.Base{ID: a.ID}}))

	t.Run("deleted_at is the database clock in UTC", func(t *testing.T) {
		var micros int64
		require.NoError(t, db.Raw("SELECT ABS(TIMESTAMPDIFF(MICROSECOND, deleted_at, UTC_TIMESTAMP(3))) FROM soft_notes WHERE id = ?", a.ID).Scan(&micros).Error)
		assert.Less(t, micros, int64(5*time.Second/time.Microsecond))
	})

	t.Run("reads hide the deleted row", func(t *testing.T) {
		assert.ErrorIs(t, repo.FindByID(ctx, a.ID, &softNote{}), dberrors.ErrNotFound)
		var rows []softNote
		require.NoError(t, repo.List(ctx, &rows))
		require.Len(t, rows, 1)
		assert.Equal(t, b.ID, rows[0].ID)
		err := repo.Update(ctx, &softNote{Base: models.Base{ID: a.ID}, Name: "x", Version: 1})
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
		assert.ErrorIs(t, repo.Delete(ctx, &softNote{Base: models.Base{ID: a.ID}}), dberrors.ErrNotFound)
	})

	t.Run("the optimistic lock still works", func(t *testing.T) {
		stale := &softNote{Base: models.Base{ID: b.ID}, Name: "x", Version: 7}
		err := repo.Update(ctx, stale)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version mismatch")
		b.Name = "b2"
		require.NoError(t, repo.Update(ctx, b))
		assert.Equal(t, uint(2), b.Version)
		var raw []softNote
		require.NoError(t, db.Where("id = ?", b.ID).Find(&raw).Error)
		require.Len(t, raw, 1)
		assert.Nil(t, raw[0].DeletedAt, "an update leaves deleted_at alone")
	})

	t.Run("restore and purge", func(t *testing.T) {
		require.NoError(t, repo.Restore(ctx, a.ID))
		require.NoError(t, repo.FindByID(ctx, a.ID, &softNote{}))

		require.NoError(t, repo.Delete(ctx, &softNote{Base: models.Base{ID: a.ID}}))
		require.NoError(t, db.Exec("UPDATE soft_notes SET deleted_at = UTC_TIMESTAMP(3) - INTERVAL 10 DAY WHERE id = ?", a.ID).Error)
		require.NoError(t, repo.Delete(ctx, &softNote{Base: models.Base{ID: b.ID}}))

		n, err := repo.PurgeDeleted(ctx, time.Now().Add(-7*24*time.Hour), 10)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "only the row deleted 10 days ago")
		var left int64
		require.NoError(t, db.Model(&softNote{}).Count(&left).Error)
		assert.Equal(t, int64(1), left, "the recently deleted row stays")

		n, err = repo.PurgeDeleted(ctx, time.Now().Add(-7*24*time.Hour), 10)
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}

// TestDatabaseMySQLSoftDeleteChangeHook: the change hook still runs in the
// transaction of the soft delete (the delete event of the outbox).
func TestDatabaseMySQLSoftDeleteChangeHook(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	ctx := context.Background()

	marker := fmt.Sprintf("soft-hook-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		db.DB.Where("event_type = ?", marker).Delete(&models.OutboxEvent{})
		db.DB.Where("name LIKE ?", marker+"%").Delete(&models.Item{})
	})
	hook := func(tx *gorm.DB, kind models.ChangeKind, entity interface{}) error {
		if kind != models.ChangeDeleted {
			return nil
		}
		return InsertOutboxEvent(tx, marker, []byte(`{"type":"item.deleted"}`))
	}
	repo := models.NewRepository(db.DB, models.WithSoftDelete(), models.WithChangeHook(hook))
	it := &models.Item{Name: marker + "-a", Price: 1}
	require.NoError(t, repo.Create(ctx, it))

	require.NoError(t, repo.Delete(ctx, &models.Item{Base: models.Base{ID: it.ID}}))
	var events int64
	require.NoError(t, db.DB.Model(&models.OutboxEvent{}).Where("event_type = ?", marker).Count(&events).Error)
	assert.Equal(t, int64(1), events)

	assert.ErrorIs(t, repo.Delete(ctx, &models.Item{Base: models.Base{ID: it.ID}}), dberrors.ErrNotFound)
	require.NoError(t, db.DB.Model(&models.OutboxEvent{}).Where("event_type = ?", marker).Count(&events).Error)
	assert.Equal(t, int64(1), events, "a second delete leaves no event")

	var rows []models.Item
	require.NoError(t, db.DB.Where("id = ?", it.ID).Find(&rows).Error)
	require.Len(t, rows, 1, "the row stays")
	assert.NotNil(t, rows[0].DeletedAt)
}

// sqlRecorder is a GORM logger that keeps the SQL of every statement.
type sqlRecorder struct {
	logger.Interface
	stmts []string
}

func (r *sqlRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	r.stmts = append(r.stmts, sql)
}

func explain(t *testing.T, db *gorm.DB, sql string) (key, extra string) {
	t.Helper()
	var rows []map[string]interface{}
	require.NoError(t, db.Raw("EXPLAIN "+sql).Scan(&rows).Error, sql)
	require.NotEmpty(t, rows, sql)
	str := func(v interface{}) string {
		switch x := v.(type) {
		case nil:
			return ""
		case []byte:
			return string(x)
		default:
			return fmt.Sprint(x)
		}
	}
	for _, r := range rows {
		key += str(r["key"])
		extra += str(r["Extra"])
	}
	return key, extra
}

// seedSoftNotes fills soft_notes with n live rows (n is a power of two, at
// least 2) by doubling, then soft deletes nine of every ten, so the table is
// mostly deleted rows, and refreshes the statistics.
func seedSoftNotes(t *testing.T, db *gorm.DB, n int) {
	t.Helper()
	require.NoError(t, db.Exec("INSERT INTO soft_notes (name) VALUES ('seed')").Error)
	for rows := 1; rows < n; rows *= 2 {
		require.NoError(t, db.Exec("INSERT INTO soft_notes (name) SELECT CONCAT('r', UUID()) FROM soft_notes").Error)
	}
	require.NoError(t, db.Exec("UPDATE soft_notes SET deleted_at = UTC_TIMESTAMP(3) WHERE id % 10 <> 0").Error)
	require.NoError(t, db.Exec("ANALYZE TABLE soft_notes").Error)
}

// TestDatabaseMySQLSoftDeleteIndexes checks with EXPLAIN what the indexes of a
// soft delete table give the list queries.
//
//   - The default order (id) never needs a sort: the optimizer reads PRIMARY in
//     id order and skips the deleted rows (cheap while most rows are live), and
//     FORCE INDEX shows that the deleted_at index can serve the same query
//     without a sort too (an InnoDB secondary index ends with the primary key).
//     These two checks prove the order property only, not which index the
//     optimizer picks: that depends on the statistics and the MySQL version.
//   - A list sorted by another column needs a composite index with deleted_at
//     first. On a table of mostly deleted rows the optimizer picks it, and the
//     query has no filesort.
func TestDatabaseMySQLSoftDeleteIndexes(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	ctx := context.Background()

	prefix := fmt.Sprintf("sdx-%d-", time.Now().UnixNano())
	t.Cleanup(func() { db.DB.Where("name LIKE ?", prefix+"%").Delete(&models.Item{}) })
	plain := models.NewRepository(db.DB, models.WithSoftDelete())
	var ids []uint
	for i := 0; i < 60; i++ {
		it := &models.Item{Name: fmt.Sprintf("%s%02d", prefix, i), Price: float64(i + 1)}
		require.NoError(t, plain.Create(ctx, it))
		ids = append(ids, it.ID)
	}
	for _, id := range ids[:20] {
		require.NoError(t, plain.Delete(ctx, &models.Item{Base: models.Base{ID: id}}))
	}

	t.Run("the repository query for the default order", func(t *testing.T) {
		rec := &sqlRecorder{Interface: logger.Default.LogMode(logger.Silent)}
		recDB := db.DB.Session(&gorm.Session{Logger: rec})
		repo := models.NewRepository(recDB, models.WithSoftDelete())
		var items []models.Item
		total, err := repo.ListPaged(ctx, &items, 25, 0, models.ItemFilter{Name: prefix})
		require.NoError(t, err)
		assert.Equal(t, int64(40), total, "the count hides the deleted rows")
		assert.Len(t, items, 25)

		// The default order without a filter is the case that must not sort.
		rec.stmts = nil
		_, err = repo.ListPaged(ctx, &items, 25, 0, models.ItemFilter{})
		require.NoError(t, err)
		var page string
		for _, s := range rec.stmts {
			if strings.HasPrefix(s, "SELECT `id`,") {
				page = s
			}
		}
		require.NotEmpty(t, page, "recorded: %v", rec.stmts)
		require.Contains(t, page, "deleted_at IS NULL")

		// The order property: no sort, whichever index the optimizer picks.
		key, extra := explain(t, db.DB, page)
		assert.NotEmpty(t, key)
		assert.NotContains(t, extra, "filesort", "key=%s extra=%s", key, extra)

		// The deleted_at index serves the same query without a sort.
		forced := strings.Replace(page, "FROM `items`", "FROM `items` FORCE INDEX (idx_items_deleted_at)", 1)
		require.NotEqual(t, page, forced)
		key, extra = explain(t, db.DB, forced)
		assert.Equal(t, "idx_items_deleted_at", key)
		assert.NotContains(t, extra, "filesort", "key=%s extra=%s", key, extra)
	})

	t.Run("a sort column with the composite index", func(t *testing.T) {
		notes, _ := openSoftNotes(t)
		seedSoftNotes(t, notes, 4096)
		const query = "SELECT id, name FROM soft_notes %s WHERE deleted_at IS NULL ORDER BY name ASC, id ASC LIMIT 25"

		// Whatever index the optimizer picks, it starts with deleted_at and
		// the query does not sort.
		key, extra := explain(t, db.DB, fmt.Sprintf(query, ""))
		assert.Contains(t, []string{"idx_soft_notes_live_name", "idx_soft_notes_deleted_at"}, key, "extra=%s", extra)
		assert.NotContains(t, extra, "filesort", "key=%s extra=%s", key, extra)

		// The composite index serves the order without a sort.
		key, extra = explain(t, db.DB, fmt.Sprintf(query, "FORCE INDEX (idx_soft_notes_live_name)"))
		assert.Equal(t, "idx_soft_notes_live_name", key)
		assert.NotContains(t, extra, "filesort", "key=%s extra=%s", key, extra)
	})
}

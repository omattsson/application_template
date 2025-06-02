package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err)
	return db
}

func TestMigrator(t *testing.T) {
	t.Run("applies migrations in order", func(t *testing.T) {
		db := setupTestDB(t)
		migrator := NewMigrator(db)

		// Add test migrations
		migrator.AddMigration(Migration{
			Version: "20250602000001",
			Name:    "test_migration_1",
			Up: func(tx *gorm.DB) error {
				return tx.Exec("CREATE TABLE test1 (id INTEGER PRIMARY KEY)").Error
			},
			Down: func(tx *gorm.DB) error {
				return tx.Exec("DROP TABLE test1").Error
			},
		})

		migrator.AddMigration(Migration{
			Version: "20250602000002",
			Name:    "test_migration_2",
			Up: func(tx *gorm.DB) error {
				return tx.Exec("CREATE TABLE test2 (id INTEGER PRIMARY KEY)").Error
			},
			Down: func(tx *gorm.DB) error {
				return tx.Exec("DROP TABLE test2").Error
			},
		})

		// Run migrations
		err := migrator.MigrateUp()
		assert.NoError(t, err)

		// Verify migrations were applied
		var count int64
		err = db.Model(&SchemaVersion{}).Count(&count).Error
		assert.NoError(t, err)
		assert.Equal(t, int64(2), count)

		// Verify tables exist
		assert.True(t, db.Migrator().HasTable("test1"))
		assert.True(t, db.Migrator().HasTable("test2"))
	})

	t.Run("rolls back last migration", func(t *testing.T) {
		db := setupTestDB(t)
		migrator := NewMigrator(db)

		// Add test migration
		migrator.AddMigration(Migration{
			Version: "20250602000001",
			Name:    "test_migration",
			Up: func(tx *gorm.DB) error {
				return tx.Exec("CREATE TABLE test (id INTEGER PRIMARY KEY)").Error
			},
			Down: func(tx *gorm.DB) error {
				return tx.Exec("DROP TABLE test").Error
			},
		})

		// Run migration up
		err := migrator.MigrateUp()
		assert.NoError(t, err)

		// Roll back migration
		err = migrator.MigrateDown()
		assert.NoError(t, err)

		// Verify table was dropped
		assert.False(t, db.Migrator().HasTable("test"))

		// Verify migration record was removed
		var count int64
		err = db.Model(&SchemaVersion{}).Count(&count).Error
		assert.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("handles duplicate migrations", func(t *testing.T) {
		db := setupTestDB(t)
		migrator := NewMigrator(db)

		migration := Migration{
			Version: "20250602000001",
			Name:    "test_migration",
			Up: func(tx *gorm.DB) error {
				return tx.Exec("CREATE TABLE test (id INTEGER PRIMARY KEY)").Error
			},
			Down: func(tx *gorm.DB) error {
				return tx.Exec("DROP TABLE test").Error
			},
		}

		// Add same migration twice
		migrator.AddMigration(migration)
		migrator.AddMigration(migration)

		// Run migrations - should only apply once
		err := migrator.MigrateUp()
		assert.NoError(t, err)

		var count int64
		err = db.Model(&SchemaVersion{}).Count(&count).Error
		assert.NoError(t, err)
		assert.Equal(t, int64(1), count)
	})
}

package database

import (
	"testing"

	"backend/internal/database/schema"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCreateIdempotencyKeysMigration(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	mig := createIdempotencyKeysMigration()
	assert.Equal(t, "20261117000001", mig.Version)
	assert.Equal(t, "create_idempotency_keys", mig.Name)
	require.NotNil(t, mig.Down)
	require.True(t, db.Migrator().HasTable("idempotency_keys"))
	assert.True(t, db.Migrator().HasIndex("idempotency_keys", "idx_idempotency_keys_expires_at"))

	require.NoError(t, db.Exec("INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, lock_id, content_type, status_code, locked_at, expires_at) VALUES ('s', 'k', 'h', 'l', '', 0, '2000-01-01 00:00:00.000', '2000-01-01 00:00:00.000')").Error)
	require.NoError(t, mig.Up(db), "a second Up is a no-op")
	var n int64
	require.NoError(t, db.Table("idempotency_keys").Count(&n).Error)
	assert.Equal(t, int64(1), n, "Up keeps the rows")

	require.NoError(t, mig.Down(db))
	require.NoError(t, mig.Down(db), "a second Down is a no-op")
	assert.False(t, db.Migrator().HasTable("idempotency_keys"))
	assert.True(t, db.Migrator().HasTable("replica_heartbeats"), "Down touches only its own table")

	m := schema.NewMigrator(db)
	registerMigrations(m)
	require.NoError(t, db.Exec("DELETE FROM schema_versions WHERE version = ?", mig.Version).Error)
	require.NoError(t, m.MigrateUp())
	assert.True(t, db.Migrator().HasTable("idempotency_keys"))
}

// The migration creates the table from a frozen copy of the model. A field,
// key or index without a migration fails here.
func TestIdempotencyKeysMatchModel(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	model := &models.IdempotencyKey{}
	stmt := &gorm.Statement{DB: db}
	require.NoError(t, stmt.Parse(model))
	frozen := &gorm.Statement{DB: db}
	require.NoError(t, frozen.Parse(&idempotencyKeyV1{}))
	assert.Equal(t, frozen.Schema.Table, stmt.Schema.Table)
	require.Len(t, frozen.Schema.Fields, len(stmt.Schema.Fields))
	for i, f := range stmt.Schema.Fields {
		g := frozen.Schema.Fields[i]
		assert.Equal(t, f.DBName, g.DBName)
		assert.Equal(t, f.PrimaryKey, g.PrimaryKey, f.DBName)
		assert.Equal(t, f.Size, g.Size, f.DBName)
		assert.Equal(t, f.DataType, g.DataType, f.DBName)
		assert.Equal(t, f.NotNull, g.NotNull, f.DBName)
		assert.True(t, db.Migrator().HasColumn(model, f.DBName), f.DBName)
	}
	for _, idx := range stmt.Schema.ParseIndexes() {
		assert.True(t, db.Migrator().HasIndex(model, idx.Name), "index %s", idx.Name)
	}
}

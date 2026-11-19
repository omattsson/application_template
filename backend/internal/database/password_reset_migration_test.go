package database

import (
	"testing"

	"backend/internal/database/schema"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCreatePasswordResetTokensMigration(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	mig := createPasswordResetTokensMigration()
	assert.Equal(t, "20261119000001", mig.Version)
	assert.Equal(t, "create_password_reset_tokens", mig.Name)
	require.NotNil(t, mig.Down)
	require.True(t, db.Migrator().HasTable("password_reset_tokens"))
	assert.True(t, db.Migrator().HasIndex("password_reset_tokens", "idx_password_reset_tokens_expires_at"))
	assert.True(t, db.Migrator().HasIndex("password_reset_tokens", "idx_password_reset_tokens_user_id"))
	assert.True(t, db.Migrator().HasIndex("password_reset_tokens", "idx_password_reset_tokens_token_hash"))

	require.NoError(t, db.Exec("INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at) VALUES ('t1', 'u1', ?, '2099-01-01 00:00:00.000', '2000-01-01 00:00:00.000')", sha256Hex("x")).Error)
	require.NoError(t, mig.Up(db), "a second Up is a no-op")
	var n int64
	require.NoError(t, db.Table("password_reset_tokens").Count(&n).Error)
	assert.Equal(t, int64(1), n, "Up keeps the rows")

	err := db.Exec("INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at) VALUES ('t2', 'u1', ?, '2099-01-01 00:00:00.000', '2000-01-01 00:00:00.000')", sha256Hex("x")).Error
	assert.Error(t, err, "token_hash is unique")

	require.NoError(t, mig.Down(db))
	require.NoError(t, mig.Down(db), "a second Down is a no-op")
	assert.False(t, db.Migrator().HasTable("password_reset_tokens"))
	assert.True(t, db.Migrator().HasTable("outbox_events"), "Down touches only its own table")

	m := schema.NewMigrator(db)
	registerMigrations(m)
	require.NoError(t, db.Exec("DELETE FROM schema_versions WHERE version = ?", mig.Version).Error)
	require.NoError(t, m.MigrateUp())
	assert.True(t, db.Migrator().HasTable("password_reset_tokens"))
}

// The migration creates the table from a frozen copy of the model. A field,
// key or index without a migration fails here.
func TestPasswordResetTokensMatchModel(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	model := &models.PasswordResetToken{}
	stmt := &gorm.Statement{DB: db}
	require.NoError(t, stmt.Parse(model))
	frozen := &gorm.Statement{DB: db}
	require.NoError(t, frozen.Parse(&passwordResetTokenV1{}))
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
	assert.Len(t, stmt.Schema.ParseIndexes(), len(frozen.Schema.ParseIndexes()), "the same indexes")
}

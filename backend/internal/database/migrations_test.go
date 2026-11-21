package database

import (
	"context"
	"testing"
	"time"

	"backend/internal/database/schema"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openSQLite opens an in-memory SQLite database on one connection, so a
// transaction and the reads after it see the same database.
func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// openMigratedDB returns a SQLite database with all migrations applied.
func openMigratedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openSQLite(t)
	require.NoError(t, (&Database{DB: db}).AutoMigrate())
	return db
}

func appliedVersions(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var versions []string
	require.NoError(t, db.Model(&schema.SchemaVersion{}).Order("version").Pluck("version", &versions).Error)
	return versions
}

func TestMigrationRunsTwice(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	first := appliedVersions(t, db)
	assert.Equal(t, []string{"20231201000001", "20231201000002", "20231201000003", "20260101000001", "20260101000002", "20260101000003", "20261115000001", "20261117000001", "20261117000002", "20261119000001", "20261121000001"}, first)

	// A user written between the runs survives: the second run applies nothing.
	u := &models.User{ID: uuid.New().String(), Username: "keep-me", Role: models.RoleAdmin, AuthProvider: models.AuthProviderLocal}
	require.NoError(t, db.Create(u).Error)

	require.NoError(t, (&Database{DB: db}).AutoMigrate())

	assert.Equal(t, first, appliedVersions(t, db), "the second run records nothing")
	var got models.User
	require.NoError(t, db.Where("username = ?", "keep-me").First(&got).Error)
	assert.Equal(t, u.ID, got.ID)
}

func TestMigrationOnDatabaseWithOldUsersTable(t *testing.T) {
	t.Parallel()
	db := openSQLite(t)

	// The database of an older release: migrations 1 and 2 applied, with a
	// user row of the old shape (uint ID, username, email, name).
	old := schema.NewMigrator(db)
	old.AddMigration(createBaseTablesMigration())
	old.AddMigration(addIndexesMigration())
	require.NoError(t, old.MigrateUp())
	require.NoError(t, db.Create(&legacyUser{Username: "old", Email: "old@example.com", Name: "Old User"}).Error)
	require.True(t, db.Migrator().HasColumn("users", "name"))

	require.NoError(t, (&Database{DB: db}).AutoMigrate())

	assert.Len(t, appliedVersions(t, db), 11)
	for _, col := range []string{"id", "username", "email", "password_hash", "display_name", "role", "auth_provider", "external_id", "disabled", "service_account", "created_at", "updated_at"} {
		assert.True(t, db.Migrator().HasColumn("users", col), "users.%s", col)
	}
	assert.False(t, db.Migrator().HasColumn("users", "name"), "the old table is replaced")
	assert.False(t, db.Migrator().HasColumn("users", "deleted_at"), "the old table is replaced")
	for _, table := range []string{"refresh_tokens", "api_keys", "audit_logs", "items", "session_entries"} {
		assert.True(t, db.Migrator().HasTable(table), table)
	}

	var n int64
	require.NoError(t, db.Table("users").Count(&n).Error)
	assert.Zero(t, n, "the old rows are dropped with the old table")

	// The new table takes a UUID user.
	require.NoError(t, NewGORMUserRepository(db).Create(context.Background(), &models.User{Username: "new", Role: models.RoleUser}))
}

func TestReplaceUsersMigrationDownRestoresOldTable(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	m := schema.NewMigrator(db)
	registerMigrations(m)

	require.NoError(t, m.MigrateDown(), "roll back 20261121000001")
	require.NoError(t, m.MigrateDown(), "roll back 20261119000001")
	require.NoError(t, m.MigrateDown(), "roll back 20261117000002")
	require.NoError(t, m.MigrateDown(), "roll back 20261117000001")
	require.NoError(t, m.MigrateDown(), "roll back 20261115000001")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000003")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000002")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000001")

	assert.Len(t, appliedVersions(t, db), 3)
	for _, table := range []string{"refresh_tokens", "api_keys", "audit_logs"} {
		assert.False(t, db.Migrator().HasTable(table), table)
	}
	assert.True(t, db.Migrator().HasColumn("users", "name"), "the old users table is back")
	assert.False(t, db.Migrator().HasColumn("users", "password_hash"))
	assert.True(t, db.Migrator().HasIndex(&legacyUser{}, "idx_users_email"), "migration 2's index is back, so its Down still works")
	require.NoError(t, db.Create(&legacyUser{Username: "old", Email: "old@example.com"}).Error)

	// Up again replaces the table once more.
	require.NoError(t, m.MigrateUp())
	assert.Len(t, appliedVersions(t, db), 11)
	assert.True(t, db.Migrator().HasColumn("users", "password_hash"))
	assert.True(t, db.Migrator().HasTable("audit_logs"))
}

func TestReplaceUsersMigrationIsIdempotent(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	mig := replaceUsersAndAddAuthTablesMigration()
	assert.Equal(t, "20260101000001", mig.Version)
	assert.Equal(t, "replace_users_and_add_auth_tables", mig.Name)

	u := &models.User{ID: uuid.New().String(), Username: "stays", Role: models.RoleUser, AuthProvider: models.AuthProviderLocal}
	require.NoError(t, db.Create(u).Error)

	// Up on the new table keeps it and its rows.
	require.NoError(t, mig.Up(db))
	var n int64
	require.NoError(t, db.Model(&models.User{}).Where("id = ?", u.ID).Count(&n).Error)
	assert.Equal(t, int64(1), n, "Up does not drop a users table of the new shape")

	require.NoError(t, mig.Down(db))
	require.NoError(t, mig.Down(db), "a second Down is a no-op")
	assert.True(t, db.Migrator().HasColumn("users", "name"))
	assert.False(t, db.Migrator().HasTable("refresh_tokens"))
}

// The migration creates the tables from frozen copies of the models. A model
// field or index without a migration fails here.
func TestAuthTablesMatchModels(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)

	for _, model := range []any{&models.User{}, &models.RefreshToken{}, &models.APIKey{}, &models.AuditLog{}} {
		stmt := &gorm.Statement{DB: db}
		require.NoError(t, stmt.Parse(model))
		for _, f := range stmt.Schema.Fields {
			if f.DBName == "" {
				continue
			}
			assert.True(t, db.Migrator().HasColumn(model, f.DBName), "%s.%s", stmt.Schema.Table, f.DBName)
		}
		for _, idx := range stmt.Schema.ParseIndexes() {
			assert.True(t, db.Migrator().HasIndex(model, idx.Name), "%s index %s", stmt.Schema.Table, idx.Name)
		}
	}
}

func TestCreateSessionEntriesMigration(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	mig := createSessionEntriesMigration()
	assert.Equal(t, "20260101000002", mig.Version)
	assert.Equal(t, "create_session_entries", mig.Name)
	require.True(t, db.Migrator().HasTable("session_entries"))

	// Up on the existing table keeps its rows.
	require.NoError(t, db.Create(&sessionstore.SessionEntry{EntryKey: "jti", Kind: "token_block", ExpiresAt: 1}).Error)
	require.NoError(t, mig.Up(db), "a second Up is a no-op")
	var n int64
	require.NoError(t, db.Model(&sessionstore.SessionEntry{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)

	require.NoError(t, mig.Down(db))
	require.NoError(t, mig.Down(db), "a second Down is a no-op")
	assert.False(t, db.Migrator().HasTable("session_entries"))
	assert.True(t, db.Migrator().HasTable("users"), "Down touches only its own table")

	require.NoError(t, mig.Up(db))
	assert.True(t, db.Migrator().HasTable("session_entries"))
}

func TestSessionEntriesMigrationDownKeepsTheAuthTables(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	m := schema.NewMigrator(db)
	registerMigrations(m)

	require.NoError(t, m.MigrateDown(), "roll back 20261121000001")
	require.NoError(t, m.MigrateDown(), "roll back 20261119000001")
	require.NoError(t, m.MigrateDown(), "roll back 20261117000002")
	require.NoError(t, m.MigrateDown(), "roll back 20261117000001")
	require.NoError(t, m.MigrateDown(), "roll back 20261115000001")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000003")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000002")

	assert.Len(t, appliedVersions(t, db), 4)
	assert.False(t, db.Migrator().HasTable("session_entries"))
	assert.True(t, db.Migrator().HasColumn("users", "password_hash"))
	require.NoError(t, m.MigrateUp())
	assert.True(t, db.Migrator().HasTable("session_entries"))
}

// The migration creates session_entries from a frozen copy of the model. A
// model field, key or index without a migration fails here.
func TestSessionEntriesMatchModel(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	model := &sessionstore.SessionEntry{}

	stmt := &gorm.Statement{DB: db}
	require.NoError(t, stmt.Parse(model))
	assert.Equal(t, "session_entries", stmt.Schema.Table)
	for _, f := range stmt.Schema.Fields {
		assert.True(t, db.Migrator().HasColumn(model, f.DBName), f.DBName)
	}
	for _, idx := range stmt.Schema.ParseIndexes() {
		assert.True(t, db.Migrator().HasIndex(model, idx.Name), idx.Name)
	}
	assert.True(t, db.Migrator().HasIndex(model, "idx_session_entries_expires_at"))

	frozen := &gorm.Statement{DB: db}
	require.NoError(t, frozen.Parse(&sessionEntryV1{}))
	require.Len(t, frozen.Schema.Fields, len(stmt.Schema.Fields))
	for i, f := range stmt.Schema.Fields {
		g := frozen.Schema.Fields[i]
		assert.Equal(t, f.DBName, g.DBName)
		assert.Equal(t, f.PrimaryKey, g.PrimaryKey, f.DBName)
		assert.Equal(t, f.Size, g.Size, f.DBName)
		assert.Equal(t, f.DataType, g.DataType, f.DBName)
		assert.Equal(t, f.NotNull, g.NotNull, f.DBName)
	}

	// The row of a token block round-trips, and the key is per kind.
	rows := []sessionstore.SessionEntry{
		{EntryKey: "x", Kind: "token_block", ExpiresAt: 10},
		{EntryKey: "x", Kind: "session_block", ExpiresAt: 10},
	}
	require.NoError(t, db.Create(&rows).Error)
	require.Error(t, db.Create(&sessionstore.SessionEntry{EntryKey: "x", Kind: "token_block"}).Error, "the key is unique per kind")
}

func TestUsersTableIndexes(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)

	for _, name := range []string{"idx_users_username", "idx_users_role", "idx_users_auth_provider_external_id"} {
		assert.True(t, db.Migrator().HasIndex("users", name), name)
	}
	for table, names := range map[string][]string{
		"refresh_tokens": {"idx_refresh_tokens_token_hash", "idx_refresh_tokens_user_id", "idx_refresh_tokens_family_id", "idx_refresh_tokens_expires_at"},
		"api_keys":       {"idx_api_keys_user_id", "idx_api_keys_prefix"},
		"audit_logs":     {"idx_audit_logs_user_id", "idx_audit_logs_entity", "idx_audit_logs_timestamp", "idx_audit_logs_action_timestamp"},
	} {
		for _, name := range names {
			assert.True(t, db.Migrator().HasIndex(table, name), "%s.%s", table, name)
		}
	}

	// A refresh token row with the frozen shape round-trips.
	now := time.Now().UTC()
	rt := &models.RefreshToken{ID: "rt", UserID: "u", TokenHash: hashOf("rt"), ExpiresAt: now, LastActivity: now}
	require.NoError(t, db.Create(rt).Error)
}

package database

import (
	"testing"

	"backend/internal/database/schema"
	"backend/internal/leader"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCreateReplicaTablesMigration(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	mig := createReplicaTablesMigration()
	assert.Equal(t, "20260101000003", mig.Version)
	assert.Equal(t, "create_replica_heartbeats_and_leader_leases", mig.Name)
	require.True(t, db.Migrator().HasTable("replica_heartbeats"))
	require.True(t, db.Migrator().HasTable("leader_leases"))
	assert.True(t, db.Migrator().HasIndex("replica_heartbeats", "idx_replica_heartbeats_last_seen"))

	require.NoError(t, db.Exec("INSERT INTO leader_leases (name, holder, expires_at) VALUES ('x', 'a', '2000-01-01 00:00:00.000')").Error)
	require.NoError(t, mig.Up(db), "a second Up is a no-op")
	var n int64
	require.NoError(t, db.Table("leader_leases").Count(&n).Error)
	assert.Equal(t, int64(1), n, "Up keeps the rows")

	require.NoError(t, mig.Down(db))
	require.NoError(t, mig.Down(db), "a second Down is a no-op")
	assert.False(t, db.Migrator().HasTable("replica_heartbeats"))
	assert.False(t, db.Migrator().HasTable("leader_leases"))
	assert.True(t, db.Migrator().HasTable("session_entries"), "Down touches only its own tables")

	m := schema.NewMigrator(db)
	registerMigrations(m)
	require.NoError(t, db.Exec("DELETE FROM schema_versions WHERE version = ?", mig.Version).Error)
	require.NoError(t, m.MigrateUp())
	assert.True(t, db.Migrator().HasTable("leader_leases"))
}

// The migration creates the tables from frozen copies of the row types. A
// field, key or index without a migration fails here.
func TestReplicaTablesMatchModels(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	for _, pair := range []struct{ model, frozen any }{
		{&models.ReplicaHeartbeat{}, &replicaHeartbeatV1{}},
		{&leader.LeaseRecord{}, &leaderLeaseV1{}},
	} {
		stmt := &gorm.Statement{DB: db}
		require.NoError(t, stmt.Parse(pair.model))
		frozen := &gorm.Statement{DB: db}
		require.NoError(t, frozen.Parse(pair.frozen))
		assert.Equal(t, frozen.Schema.Table, stmt.Schema.Table)
		require.Len(t, frozen.Schema.Fields, len(stmt.Schema.Fields), stmt.Schema.Table)
		for i, f := range stmt.Schema.Fields {
			g := frozen.Schema.Fields[i]
			assert.Equal(t, f.DBName, g.DBName)
			assert.Equal(t, f.PrimaryKey, g.PrimaryKey, f.DBName)
			assert.Equal(t, f.Size, g.Size, f.DBName)
			assert.Equal(t, f.DataType, g.DataType, f.DBName)
			assert.Equal(t, f.NotNull, g.NotNull, f.DBName)
			assert.True(t, db.Migrator().HasColumn(pair.model, f.DBName), "%s.%s", stmt.Schema.Table, f.DBName)
		}
		for _, idx := range stmt.Schema.ParseIndexes() {
			assert.True(t, db.Migrator().HasIndex(pair.model, idx.Name), "%s index %s", stmt.Schema.Table, idx.Name)
		}
	}
}

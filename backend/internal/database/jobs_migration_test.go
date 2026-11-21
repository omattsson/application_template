package database

import (
	"testing"

	"backend/internal/database/schema"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCreateJobsMigration(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	mig := createJobsMigration()
	assert.Equal(t, "20261121000001", mig.Version)
	assert.Equal(t, "create_jobs", mig.Name)
	require.NotNil(t, mig.Down)
	require.True(t, db.Migrator().HasTable("jobs"))
	for _, idx := range []string{"idx_jobs_claim", "idx_jobs_finished", "idx_jobs_created_at", "idx_jobs_run_at", "idx_jobs_type_status"} {
		assert.True(t, db.Migrator().HasIndex("jobs", idx), idx)
	}

	require.NoError(t, db.Exec("INSERT INTO jobs (type, payload, status, run_at, created_at, updated_at) VALUES ('t', '{}', 'pending', '2000-01-01 00:00:00.000', '2000-01-01 00:00:00.000', '2000-01-01 00:00:00.000')").Error)
	require.NoError(t, mig.Up(db), "a second Up is a no-op")
	var n int64
	require.NoError(t, db.Table("jobs").Count(&n).Error)
	assert.Equal(t, int64(1), n, "Up keeps the rows")

	var attempts, maxAttempts int
	var lockedBy, lastError string
	require.NoError(t, db.Raw("SELECT attempts, max_attempts, locked_by, last_error FROM jobs").Row().Scan(&attempts, &maxAttempts, &lockedBy, &lastError))
	assert.Equal(t, 0, attempts)
	assert.Equal(t, 5, maxAttempts)
	assert.Empty(t, lockedBy)
	assert.Empty(t, lastError)

	require.NoError(t, mig.Down(db))
	require.NoError(t, mig.Down(db), "a second Down is a no-op")
	assert.False(t, db.Migrator().HasTable("jobs"))
	assert.True(t, db.Migrator().HasTable("password_reset_tokens"), "Down touches only its own table")

	m := schema.NewMigrator(db)
	registerMigrations(m)
	require.NoError(t, db.Exec("DELETE FROM schema_versions WHERE version = ?", mig.Version).Error)
	require.NoError(t, m.MigrateUp())
	assert.True(t, db.Migrator().HasTable("jobs"))
}

// The migration creates the table from a frozen copy of the model. A field,
// key or index without a migration fails here.
func TestJobsMatchModel(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	model := &models.Job{}
	stmt := &gorm.Statement{DB: db}
	require.NoError(t, stmt.Parse(model))
	frozen := &gorm.Statement{DB: db}
	require.NoError(t, frozen.Parse(&jobV1{}))
	assert.Equal(t, frozen.Schema.Table, stmt.Schema.Table)
	require.Len(t, frozen.Schema.Fields, len(stmt.Schema.Fields))
	for _, f := range stmt.Schema.Fields {
		g := frozen.Schema.LookUpField(f.DBName)
		require.NotNil(t, g, f.DBName)
		assert.Equal(t, f.PrimaryKey, g.PrimaryKey, f.DBName)
		assert.Equal(t, f.AutoIncrement, g.AutoIncrement, f.DBName)
		assert.Equal(t, f.Size, g.Size, f.DBName)
		assert.Equal(t, f.DataType, g.DataType, f.DBName)
		assert.Equal(t, f.NotNull, g.NotNull, f.DBName)
		assert.Equal(t, f.DefaultValue, g.DefaultValue, f.DBName)
		assert.True(t, db.Migrator().HasColumn(model, f.DBName), f.DBName)
	}
	columns := func(s *gorm.Statement) map[string][]string {
		out := map[string][]string{}
		for _, idx := range s.Schema.ParseIndexes() {
			for _, f := range idx.Fields {
				out[idx.Name] = append(out[idx.Name], f.DBName)
			}
		}
		return out
	}
	want, got := columns(frozen), columns(stmt)
	assert.Equal(t, want, got, "the frozen copy has the same indexes with the same column order")
	assert.Equal(t, []string{"status", "run_at", "id"}, got["idx_jobs_claim"])
	assert.Equal(t, []string{"status", "finished_at"}, got["idx_jobs_finished"])
	assert.Equal(t, []string{"type", "status"}, got["idx_jobs_type_status"])
	for name := range got {
		assert.True(t, db.Migrator().HasIndex(model, name), "index %s", name)
	}
}

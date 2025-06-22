package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFromDBConfig(t *testing.T) {
	t.Run("With SQLite configuration", func(t *testing.T) {
		db, err := NewFromDBConfig(nil)
		require.NoError(t, err)
		require.NotNil(t, db)

		// Test database connection
		sqlDB, err := db.DB.DB()
		require.NoError(t, err)

		// Verify connection settings
		stats := sqlDB.Stats()
		assert.Equal(t, 5, stats.MaxOpenConnections)
	})

	t.Run("With MySQL configuration", func(t *testing.T) {
		cfg := &Config{
			Host:     "invalid",
			Port:     "3306",
			User:     "test",
			Password: "test",
			DBName:   "test",
		}

		db, err := NewFromDBConfig(cfg)
		assert.Error(t, err)
		assert.Nil(t, db)
		assert.Contains(t, err.Error(), "failed to connect to database")
	})
}

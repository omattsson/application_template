package database

import (
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFromConfig(t *testing.T) {
	t.Run("Valid configuration", func(t *testing.T) {
		cfg := &config.Config{
			Database: config.DatabaseConfig{
				Host:            "localhost",
				Port:            "3306",
				User:            "testuser",
				Password:        "testpass",
				DBName:          "testdb",
				MaxOpenConns:    10,
				MaxIdleConns:    5,
				ConnMaxLifetime: time.Minute * 5,
			},
		}

		db, err := NewFromConfig(cfg)
		require.NoError(t, err)
		require.NotNil(t, db)

		// Test database connection
		sqlDB, err := db.DB.DB()
		require.NoError(t, err)

		// Verify connection settings
		assert.Equal(t, 10, sqlDB.Stats().MaxOpenConnections)
		assert.Equal(t, 5, sqlDB.Stats().MaxIdleConns)
	})

	t.Run("Invalid configuration", func(t *testing.T) {
		cfg := &config.Config{
			Database: config.DatabaseConfig{
				Host:     "nonexistent",
				Port:     "1234",
				User:     "invalid",
				Password: "invalid",
				DBName:   "invalid",
			},
		}

		db, err := NewFromConfig(cfg)
		assert.Error(t, err)
		assert.Nil(t, db)
		assert.Contains(t, err.Error(), "failed to connect to database")
	})
}

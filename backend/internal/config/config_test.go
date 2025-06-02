package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	// Test with environment variables set
	t.Run("With environment variables", func(t *testing.T) {
		// Set test environment variables
		envVars := map[string]string{
			"APP_NAME":    "testapp",
			"APP_ENV":     "testing",
			"APP_DEBUG":   "true",
			"DB_HOST":     "testhost",
			"DB_PORT":     "3306",
			"DB_USER":     "testuser",
			"DB_PASSWORD": "testpass",
			"DB_NAME":     "testdb",
			"SERVER_HOST": "localhost",
			"PORT":        "3000",
			"LOG_LEVEL":   "debug",
			"LOG_FILE":    "test.log",
		}

		// Set environment variables
		for k, v := range envVars {
			os.Setenv(k, v)
		}
		defer func() {
			// Clean up environment variables
			for k := range envVars {
				os.Unsetenv(k)
			}
		}()

		config, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, config)

		// Check app config
		assert.Equal(t, "testapp", config.App.Name)
		assert.Equal(t, "testing", config.App.Environment)
		assert.True(t, config.App.Debug)

		// Check database config
		assert.Equal(t, "testhost", config.Database.Host)
		assert.Equal(t, "3306", config.Database.Port)
		assert.Equal(t, "testuser", config.Database.User)
		assert.Equal(t, "testpass", config.Database.Password)
		assert.Equal(t, "testdb", config.Database.DBName)

		// Check server config
		assert.Equal(t, "localhost", config.Server.Host)
		assert.Equal(t, "3000", config.Server.Port)

		// Check logging config
		assert.Equal(t, "debug", config.Logging.Level)
		assert.Equal(t, "test.log", config.Logging.File)
	})

	// Test with default values
	t.Run("With default values", func(t *testing.T) {
		// Clear all relevant environment variables
		vars := []string{
			"APP_NAME", "APP_ENV", "APP_DEBUG",
			"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
			"SERVER_HOST", "PORT", "LOG_LEVEL", "LOG_FILE",
		}
		for _, v := range vars {
			os.Unsetenv(v)
		}

		config, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, config)

		// Check default app config
		assert.Equal(t, "application", config.App.Name)
		assert.Equal(t, "development", config.App.Environment)
		assert.True(t, config.App.Debug)

		// Check default database config
		assert.Equal(t, "localhost", config.Database.Host)
		assert.Equal(t, "3306", config.Database.Port)
		assert.Equal(t, "appuser", config.Database.User)
		assert.Equal(t, "apppass", config.Database.Password)
		assert.Equal(t, "app", config.Database.DBName)
		assert.Equal(t, 25, config.Database.MaxOpenConns)
		assert.Equal(t, 25, config.Database.MaxIdleConns)
		assert.Equal(t, 5*time.Minute, config.Database.ConnMaxLifetime)

		// Check default server config
		assert.Equal(t, "0.0.0.0", config.Server.Host)
		assert.Equal(t, "8081", config.Server.Port)
		assert.Equal(t, 15*time.Second, config.Server.ReadTimeout)
		assert.Equal(t, 15*time.Second, config.Server.WriteTimeout)
		assert.Equal(t, 30*time.Second, config.Server.ShutdownTimeout)

		// Check default logging config
		assert.Equal(t, "info", config.Logging.Level)
		assert.Empty(t, config.Logging.File)
	})
}

func TestDatabaseDSN(t *testing.T) {
	dbConfig := DatabaseConfig{
		Host:     "testhost",
		Port:     "3306",
		User:     "testuser",
		Password: "testpass",
		DBName:   "testdb",
	}

	expected := "testuser:testpass@tcp(testhost:3306)/testdb?charset=utf8mb4&parseTime=True&loc=Local"
	assert.Equal(t, expected, dbConfig.DSN())
}

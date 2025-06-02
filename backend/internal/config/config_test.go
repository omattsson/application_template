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
			"APP_NAME":                 "testapp",
			"GO_ENV":                   "testing",
			"APP_DEBUG":                "true",
			"DB_HOST":                  "testhost",
			"DB_PORT":                  "3306",
			"DB_USER":                  "testuser",
			"DB_PASSWORD":              "testpass",
			"DB_NAME":                  "testdb",
			"SERVER_HOST":              "localhost",
			"SERVER_PORT":              "3000",
			"LOG_LEVEL":                "debug",
			"LOG_FILE":                 "test.log",
			"USE_AZURE_TABLE":          "true",
			"USE_AZURITE":              "true",
			"AZURE_TABLE_ACCOUNT_NAME": "devstoreaccount1",
			"AZURE_TABLE_ACCOUNT_KEY":  "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==",
			"AZURE_TABLE_ENDPOINT":     "127.0.0.1:10002",
			"AZURE_TABLE_NAME":         "testitems",
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

		// Check Azure Table config
		assert.True(t, config.AzureTable.UseAzureTable)
		assert.True(t, config.AzureTable.UseAzurite)
		assert.Equal(t, "devstoreaccount1", config.AzureTable.AccountName)
		assert.Equal(t, "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==", config.AzureTable.AccountKey)
		assert.Equal(t, "127.0.0.1:10002", config.AzureTable.Endpoint)
		assert.Equal(t, "testitems", config.AzureTable.TableName)
	})

	// Test with default values
	t.Run("With default values", func(t *testing.T) {
		// Clear all relevant environment variables
		vars := []string{
			"APP_NAME", "GO_ENV", "APP_DEBUG",
			"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
			"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
			"SERVER_HOST", "SERVER_PORT", "SERVER_READ_TIMEOUT", "SERVER_WRITE_TIMEOUT", "SERVER_SHUTDOWN_TIMEOUT",
			"LOG_LEVEL", "LOG_FILE",
			"USE_AZURE_TABLE", "USE_AZURITE",
			"AZURE_TABLE_ACCOUNT_NAME", "AZURE_TABLE_ACCOUNT_KEY",
			"AZURE_TABLE_ENDPOINT", "AZURE_TABLE_NAME",
		}
		for _, v := range vars {
			os.Unsetenv(v)
		}

		config, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, config)

		// Check default app config
		assert.Equal(t, "backend-api", config.App.Name)
		assert.Equal(t, "development", config.App.Environment)
		assert.True(t, config.App.Debug)

		// Check default database config
		assert.Equal(t, "localhost", config.Database.Host)
		assert.Equal(t, "3306", config.Database.Port)
		assert.Equal(t, "root", config.Database.User)
		assert.Equal(t, "", config.Database.Password)
		assert.Equal(t, "app", config.Database.DBName)
		assert.Equal(t, 25, config.Database.MaxOpenConns)
		assert.Equal(t, 5, config.Database.MaxIdleConns)
		assert.Equal(t, 5*time.Minute, config.Database.ConnMaxLifetime)

		// Check default server config
		assert.Equal(t, "", config.Server.Host)
		assert.Equal(t, "8081", config.Server.Port)
		assert.Equal(t, 10*time.Second, config.Server.ReadTimeout)
		assert.Equal(t, 10*time.Second, config.Server.WriteTimeout)
		assert.Equal(t, 30*time.Second, config.Server.ShutdownTimeout)

		// Check default logging config
		assert.Equal(t, "info", config.Logging.Level)
		assert.Empty(t, config.Logging.File)

		// Check default Azure Table config
		assert.False(t, config.AzureTable.UseAzureTable)
		assert.False(t, config.AzureTable.UseAzurite)
		assert.Equal(t, "", config.AzureTable.AccountName)
		assert.Equal(t, "", config.AzureTable.AccountKey)
		assert.Equal(t, "", config.AzureTable.Endpoint)
		assert.Equal(t, "items", config.AzureTable.TableName)
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

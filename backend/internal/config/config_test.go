package config_test

import (
	"os"
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process env

	// Test with environment variables set
	t.Run("With environment variables", func(t *testing.T) {
		// Set test environment variables
		envVars := map[string]string{
			"APP_NAME":    "testapp",
			"GO_ENV":      "testing",
			"APP_DEBUG":   "true",
			"DB_HOST":     "testhost",
			"DB_PORT":     "3306",
			"DB_USER":     "testuser",
			"DB_PASSWORD": "testpass",
			"DB_NAME":     "testdb",
			"SERVER_HOST": "localhost",
			"SERVER_PORT": "3000",
			"LOG_LEVEL":   "debug",
			"LOG_FILE":    "test.log",
		}

		// Set environment variables
		for k, v := range envVars {
			t.Setenv(k, v)
		}
		defer func() {
			// Clean up environment variables
			for k := range envVars {
				os.Unsetenv(k)
			}
		}()

		config, err := config.LoadConfig()
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
			"APP_NAME", "GO_ENV", "APP_DEBUG",
			"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
			"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
			"SERVER_HOST", "SERVER_PORT", "SERVER_READ_TIMEOUT", "SERVER_WRITE_TIMEOUT", "SERVER_SHUTDOWN_TIMEOUT",
			"LOG_LEVEL", "LOG_FILE",
		}
		for _, v := range vars {
			os.Unsetenv(v)
		}

		config, err := config.LoadConfig()
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
		assert.Empty(t, config.Database.Password)
		assert.Equal(t, "app", config.Database.DBName)
		assert.Equal(t, int32(25), config.Database.MaxOpenConns)
		assert.Equal(t, int32(5), config.Database.MaxIdleConns)
		assert.Equal(t, 5*time.Minute, config.Database.ConnMaxLifetime)

		// Check default server config
		assert.Empty(t, config.Server.Host)
		assert.Equal(t, "8081", config.Server.Port)
		assert.Equal(t, 10*time.Second, config.Server.ReadTimeout)
		assert.Equal(t, time.Duration(0), config.Server.WriteTimeout) // disabled; per-write deadlines enforced in WebSocket write pump				assert.Equal(t, 30*time.Second, config.Server.IdleTimeout)
		assert.Equal(t, 30*time.Second, config.Server.ShutdownTimeout)
		// Check default logging config
		assert.Equal(t, "info", config.Logging.Level)
		assert.Empty(t, config.Logging.File)
	})
}

func TestDatabaseDSN(t *testing.T) {
	t.Parallel()
	dbConfig := config.DatabaseConfig{
		Host:     "testhost",
		Port:     "3306",
		User:     "testuser",
		Password: "testpass",
		DBName:   "testdb",
	}

	expected := "testuser:testpass@tcp(testhost:3306)/testdb?charset=utf8mb4&parseTime=True&loc=UTC" + config.MySQLSessionParams
	assert.Equal(t, expected, dbConfig.DSN())
}
func TestConfigValidate(t *testing.T) {
	t.Parallel()

	t.Run("valid config returns nil", func(t *testing.T) {
		t.Parallel()
		validConfig := &config.Config{
			App: config.AppConfig{
				Name:        "myapp",
				Environment: "production",
				Debug:       false,
			},
			Database: config.DatabaseConfig{
				Host:            "localhost",
				Port:            "3306",
				User:            "user",
				Password:        "pass",
				DBName:          "dbname",
				MaxOpenConns:    10,
				MaxIdleConns:    5,
				ConnMaxLifetime: 1 * time.Minute,
			},
			Server: config.ServerConfig{
				Host:                    "127.0.0.1",
				Port:                    "8080",
				ReadTimeout:             5 * time.Second,
				WriteTimeout:            5 * time.Second,
				IdleTimeout:             30 * time.Second,
				ShutdownTimeout:         10 * time.Second,
				RateLimit:               100,
				LoginRateLimit:          10,
				WSMaxConnectionsPerUser: 16,
			},
		}

		assert.NoError(t, validConfig.Validate())
	})

	t.Run("invalid app config", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{
			App: config.AppConfig{
				Name:        "",
				Environment: "production",
				Debug:       false,
			},
			Database: config.DatabaseConfig{
				Host:            "localhost",
				Port:            "3306",
				User:            "user",
				Password:        "pass",
				DBName:          "dbname",
				MaxOpenConns:    10,
				MaxIdleConns:    5,
				ConnMaxLifetime: 1 * time.Minute,
			},
			Server: config.ServerConfig{
				Host:                    "127.0.0.1",
				Port:                    "8080",
				ReadTimeout:             5 * time.Second,
				WriteTimeout:            5 * time.Second,
				IdleTimeout:             30 * time.Second,
				ShutdownTimeout:         10 * time.Second,
				RateLimit:               100,
				LoginRateLimit:          10,
				WSMaxConnectionsPerUser: 16,
			},
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app config")
	})

	t.Run("invalid database config", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{
			App: config.AppConfig{
				Name:        "myapp",
				Environment: "production",
				Debug:       false,
			},
			Database: config.DatabaseConfig{
				Host:            "",
				Port:            "3306",
				User:            "user",
				Password:        "pass",
				DBName:          "dbname",
				MaxOpenConns:    10,
				MaxIdleConns:    5,
				ConnMaxLifetime: 1 * time.Minute,
			},
			Server: config.ServerConfig{
				Host:                    "127.0.0.1",
				Port:                    "8080",
				ReadTimeout:             5 * time.Second,
				WriteTimeout:            5 * time.Second,
				IdleTimeout:             30 * time.Second,
				ShutdownTimeout:         10 * time.Second,
				RateLimit:               100,
				LoginRateLimit:          10,
				WSMaxConnectionsPerUser: 16,
			},
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database config")
	})

	t.Run("invalid server config", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{
			App: config.AppConfig{
				Name:        "myapp",
				Environment: "production",
				Debug:       false,
			},
			Database: config.DatabaseConfig{
				Host:            "localhost",
				Port:            "3306",
				User:            "user",
				Password:        "pass",
				DBName:          "dbname",
				MaxOpenConns:    10,
				MaxIdleConns:    5,
				ConnMaxLifetime: 1 * time.Minute,
			},
			Server: config.ServerConfig{
				Host:                    "127.0.0.1",
				Port:                    "",
				ReadTimeout:             5 * time.Second,
				WriteTimeout:            5 * time.Second,
				IdleTimeout:             30 * time.Second,
				ShutdownTimeout:         10 * time.Second,
				RateLimit:               100,
				LoginRateLimit:          10,
				WSMaxConnectionsPerUser: 16,
			},
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server config")
	})

	t.Run("zero WriteTimeout passes validation", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{
			App: config.AppConfig{
				Name:        "myapp",
				Environment: "production",
			},
			Database: config.DatabaseConfig{
				Host:            "localhost",
				Port:            "3306",
				User:            "user",
				DBName:          "dbname",
				MaxOpenConns:    10,
				MaxIdleConns:    5,
				ConnMaxLifetime: 1 * time.Minute,
			},
			Server: config.ServerConfig{
				Port:                    "8080",
				ReadTimeout:             5 * time.Second,
				WriteTimeout:            0, // disabled at server level; per-write deadlines in WebSocket pump
				IdleTimeout:             30 * time.Second,
				ShutdownTimeout:         10 * time.Second,
				RateLimit:               100,
				LoginRateLimit:          10,
				WSMaxConnectionsPerUser: 16,
			},
		}
		assert.NoError(t, cfg.Validate())
	})

	t.Run("negative WriteTimeout fails validation", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{
			App: config.AppConfig{
				Name:        "myapp",
				Environment: "production",
			},
			Database: config.DatabaseConfig{
				Host:            "localhost",
				Port:            "3306",
				User:            "user",
				DBName:          "dbname",
				MaxOpenConns:    10,
				MaxIdleConns:    5,
				ConnMaxLifetime: 1 * time.Minute,
			},
			Server: config.ServerConfig{
				Port:                    "8080",
				ReadTimeout:             5 * time.Second,
				WriteTimeout:            -1 * time.Second,
				IdleTimeout:             30 * time.Second,
				ShutdownTimeout:         10 * time.Second,
				RateLimit:               100,
				LoginRateLimit:          10,
				WSMaxConnectionsPerUser: 16,
			},
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "write timeout")
	})
}

func TestLoadConfigRejectsUnsupportedDBType(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process env.
	tests := []struct {
		name     string
		dbType   string
		wantType string
		wantErr  bool
	}{
		{name: "unsupported type is rejected", dbType: "postgres", wantErr: true},
		{name: "mysql is supported", dbType: "mysql", wantType: "mysql"},
		{name: "sqlite is supported", dbType: "sqlite", wantType: "sqlite"},
		{name: "value is trimmed and lowercased", dbType: " MySQL ", wantType: "mysql"},
		{name: "unset defaults to mysql", dbType: "", wantType: "mysql"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DB_TYPE", tt.dbType)

			cfg, err := config.LoadConfig()
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, cfg)
				assert.Contains(t, err.Error(), "unsupported")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantType, cfg.Database.Type)
		})
	}
}

func TestDatabaseConfigValidateUnsupportedType(t *testing.T) {
	t.Parallel()

	cfg := config.DatabaseConfig{
		Type:            "postgres",
		Host:            "localhost",
		Port:            "3306",
		User:            "user",
		DBName:          "db",
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Minute,
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestLoadConfigUIConfigEnv(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process env.
	tests := []struct {
		name      string
		title     string
		auth      string
		wantTitle string
		wantAuth  bool
	}{
		{name: "defaults", wantTitle: "Application Template", wantAuth: false},
		{name: "custom", title: "My App", auth: "true", wantTitle: "My App", wantAuth: true},
		{name: "auth false", auth: "false", wantTitle: "Application Template", wantAuth: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENV_FILE", "/nonexistent/.env")
			t.Setenv("APP_TITLE", tt.title)
			t.Setenv("AUTH_ENABLED", tt.auth)
			// AUTH_ENABLED=true needs a secret to start; this test is about the
			// ui-config values, not the secret rules (see config_auth_test.go).
			t.Setenv("JWT_SECRET", "ui-config-test-secret-value")

			cfg, err := config.LoadConfig()
			require.NoError(t, err)
			assert.Equal(t, tt.wantTitle, cfg.App.Title)
			assert.Equal(t, tt.wantAuth, cfg.Auth.Enabled)
		})
	}
}

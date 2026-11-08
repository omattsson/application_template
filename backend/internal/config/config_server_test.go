package config_test

import (
	"log/slog"
	"testing"

	"backend/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigLogLevel(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process env.
	tests := []struct {
		name    string
		raw     string
		want    slog.Level
		wantErr bool
	}{
		{name: "unset defaults to info", raw: "", want: slog.LevelInfo},
		{name: "debug", raw: "debug", want: slog.LevelDebug},
		{name: "info", raw: "info", want: slog.LevelInfo},
		{name: "warn", raw: "warn", want: slog.LevelWarn},
		{name: "warning alias", raw: "warning", want: slog.LevelWarn},
		{name: "error", raw: "error", want: slog.LevelError},
		{name: "case-insensitive and trimmed", raw: " DEBUG ", want: slog.LevelDebug},
		{name: "bogus is rejected", raw: "bogus", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENV_FILE", "/nonexistent/.env")
			t.Setenv("LOG_LEVEL", tt.raw)

			cfg, err := config.LoadConfig()
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, cfg)
				assert.Contains(t, err.Error(), "LOG_LEVEL")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.LogLevel)
		})
	}
}

func TestConfigGinMode(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process env.
	tests := []struct {
		name    string
		debug   string
		ginMode string
		want    string
	}{
		{name: "APP_DEBUG false is release", debug: "false", want: gin.ReleaseMode},
		{name: "APP_DEBUG true is debug", debug: "true", want: gin.DebugMode},
		{name: "APP_DEBUG unset defaults to debug", debug: "", want: gin.DebugMode},
		{name: "GIN_MODE wins over APP_DEBUG true", debug: "true", ginMode: gin.TestMode, want: gin.TestMode},
		{name: "GIN_MODE wins over APP_DEBUG false", debug: "false", ginMode: gin.DebugMode, want: gin.DebugMode},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENV_FILE", "/nonexistent/.env")
			t.Setenv("APP_DEBUG", tt.debug)
			t.Setenv("GIN_MODE", tt.ginMode)

			cfg, err := config.LoadConfig()
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.GinMode())
		})
	}
}

func TestLoadConfigServerSettings(t *testing.T) {
	// Not parallel: subtests use t.Setenv which modifies process env.
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("ENV_FILE", "/nonexistent/.env")
		for _, k := range []string{"RATE_LIMIT", "LOGIN_RATE_LIMIT", "ENABLE_SWAGGER", "TRUSTED_PROXIES", "APP_DEBUG"} {
			t.Setenv(k, "")
		}

		cfg, err := config.LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, int32(100), cfg.Server.RateLimit)
		assert.Equal(t, int32(10), cfg.Server.LoginRateLimit)
		assert.False(t, cfg.Server.EnableSwagger)
		assert.Nil(t, cfg.Server.TrustedProxies)
		assert.True(t, cfg.Server.AppDebug)
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("ENV_FILE", "/nonexistent/.env")
		t.Setenv("RATE_LIMIT", "250")
		t.Setenv("LOGIN_RATE_LIMIT", "3")
		t.Setenv("ENABLE_SWAGGER", "true")
		t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8")
		t.Setenv("APP_DEBUG", "false")

		cfg, err := config.LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, int32(250), cfg.Server.RateLimit)
		assert.Equal(t, int32(3), cfg.Server.LoginRateLimit)
		assert.True(t, cfg.Server.EnableSwagger)
		assert.Equal(t, []string{"10.0.0.0/8"}, cfg.Server.TrustedProxies)
		assert.False(t, cfg.Server.AppDebug)
		assert.False(t, cfg.App.Debug)
	})
}

func TestDatabaseDSNIncludesMySQLSessionParams(t *testing.T) {
	t.Parallel()

	dsn := (&config.DatabaseConfig{Host: "h", Port: "3306", User: "u", DBName: "d"}).DSN()
	assert.Contains(t, dsn, config.MySQLSessionParams)
	assert.Contains(t, dsn, "&sql_mode=")
	assert.Contains(t, dsn, "&time_zone=%27%2B00%3A00%27")
	assert.Contains(t, dsn, "&foreign_key_checks=1")
}

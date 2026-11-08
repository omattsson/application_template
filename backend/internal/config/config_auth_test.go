package config_test

import (
	"strings"
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authEnvKeys are the variables that LoadConfig reads for AuthConfig.
var authEnvKeys = []string{
	"AUTH_ENABLED", "JWT_SECRET", "JWT_EXPIRATION", "ACCESS_TOKEN_EXPIRATION",
	"SESSION_IDLE_TIMEOUT", "SESSION_MAX_LIFETIME", "ADMIN_USERNAME",
	"ADMIN_PASSWORD", "SELF_REGISTRATION",
}

// clearAuthEnv unsets the auth variables for the test (t.Setenv restores them)
// and points ENV_FILE at a file that does not exist.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ENV_FILE", "/nonexistent/.env")
	for _, k := range authEnvKeys {
		t.Setenv(k, "")
	}
}

func TestLoadConfigAuthDefaults(t *testing.T) {
	// Not parallel: t.Setenv modifies the process environment.
	clearAuthEnv(t)

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.False(t, cfg.Auth.Enabled)
	assert.Empty(t, cfg.Auth.JWTSecret, "there is no default JWT secret")
	assert.Empty(t, cfg.Auth.AdminPassword, "there is no default admin password")
	assert.Equal(t, "admin", cfg.Auth.AdminUsername)
	assert.False(t, cfg.Auth.SelfRegistration)
	assert.Equal(t, 24*time.Hour, cfg.Auth.JWTExpiration)
	assert.Equal(t, 15*time.Minute, cfg.Auth.AccessTokenExpiration)
	assert.Equal(t, 30*time.Minute, cfg.Auth.SessionIdleTimeout)
	assert.Equal(t, 12*time.Hour, cfg.Auth.SessionMaxLifetime)
}

func TestLoadConfigAuthEnabledSecret(t *testing.T) {
	// Not parallel: subtests use t.Setenv.
	const secret15 = "123456789012345"
	const secret16 = "1234567890123456"
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "enabled without secret fails", env: map[string]string{"AUTH_ENABLED": "true"}, wantErr: "jwt_secret is required"},
		{name: "enabled with 15 characters fails", env: map[string]string{"AUTH_ENABLED": "true", "JWT_SECRET": secret15}, wantErr: "at least 16 characters"},
		{name: "enabled with 16 characters passes", env: map[string]string{"AUTH_ENABLED": "true", "JWT_SECRET": secret16}},
		{name: "15 multi-byte characters fail", env: map[string]string{"AUTH_ENABLED": "true", "JWT_SECRET": strings.Repeat("é", 15)}, wantErr: "at least 16 characters"},
		{name: "16 multi-byte characters pass", env: map[string]string{"AUTH_ENABLED": "true", "JWT_SECRET": strings.Repeat("é", 16)}},
		{name: "disabled ignores a missing secret", env: map[string]string{"AUTH_ENABLED": "false"}},
		{name: "disabled ignores a short secret", env: map[string]string{"AUTH_ENABLED": "false", "JWT_SECRET": "short"}},
		{name: "empty admin password is allowed", env: map[string]string{"AUTH_ENABLED": "true", "JWT_SECRET": secret16, "ADMIN_PASSWORD": ""}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			clearAuthEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := config.LoadConfig()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				if s := tt.env["JWT_SECRET"]; s != "" {
					assert.NotContains(t, err.Error(), s, "the error must not contain the secret")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.env["AUTH_ENABLED"] == "true", cfg.Auth.Enabled)
			assert.Empty(t, cfg.Auth.AdminPassword)
		})
	}
}

func TestLoadConfigAuthEnv(t *testing.T) {
	// Not parallel: t.Setenv modifies the process environment.
	clearAuthEnv(t)
	t.Setenv("AUTH_ENABLED", "true")
	t.Setenv("JWT_SECRET", "a-secret-of-sufficient-length")
	t.Setenv("JWT_EXPIRATION", "2h")
	t.Setenv("ACCESS_TOKEN_EXPIRATION", "10m")
	t.Setenv("SESSION_IDLE_TIMEOUT", "20m")
	t.Setenv("SESSION_MAX_LIFETIME", "8h")
	t.Setenv("ADMIN_USERNAME", "root")
	t.Setenv("ADMIN_PASSWORD", "pw")
	t.Setenv("SELF_REGISTRATION", "true")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "a-secret-of-sufficient-length", cfg.Auth.JWTSecret)
	assert.Equal(t, 2*time.Hour, cfg.Auth.JWTExpiration)
	assert.Equal(t, 10*time.Minute, cfg.Auth.AccessTokenExpiration)
	assert.Equal(t, 20*time.Minute, cfg.Auth.SessionIdleTimeout)
	assert.Equal(t, 8*time.Hour, cfg.Auth.SessionMaxLifetime)
	assert.Equal(t, "root", cfg.Auth.AdminUsername)
	assert.Equal(t, "pw", cfg.Auth.AdminPassword)
	assert.True(t, cfg.Auth.SelfRegistration)
}

func TestLoadConfigAuthInvalidDuration(t *testing.T) {
	// Not parallel: subtests use t.Setenv.
	for _, key := range []string{"JWT_EXPIRATION", "ACCESS_TOKEN_EXPIRATION", "SESSION_IDLE_TIMEOUT", "SESSION_MAX_LIFETIME"} {
		key := key
		t.Run(key+" typo fails when auth is on", func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv("AUTH_ENABLED", "true")
			t.Setenv("JWT_SECRET", "a-secret-of-sufficient-length")
			t.Setenv(key, "24 hours")

			_, err := config.LoadConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
		t.Run(key+" typo is ignored when auth is off", func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(key, "24 hours")

			_, err := config.LoadConfig()
			require.NoError(t, err)
		})
	}
}

func TestAuthConfigValidate(t *testing.T) {
	t.Parallel()
	valid := func() config.AuthConfig {
		return config.AuthConfig{
			Enabled:               true,
			JWTSecret:             "1234567890123456",
			JWTExpiration:         24 * time.Hour,
			AccessTokenExpiration: 15 * time.Minute,
			SessionIdleTimeout:    30 * time.Minute,
			SessionMaxLifetime:    12 * time.Hour,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*config.AuthConfig)
		wantErr string
	}{
		{name: "valid", mutate: func(*config.AuthConfig) {}},
		{name: "empty secret", mutate: func(c *config.AuthConfig) { c.JWTSecret = "" }, wantErr: "jwt_secret is required"},
		{name: "short secret", mutate: func(c *config.AuthConfig) { c.JWTSecret = "123456789012345" }, wantErr: "at least 16 characters"},
		{name: "zero jwt expiration", mutate: func(c *config.AuthConfig) { c.JWTExpiration = 0 }, wantErr: "jwt_expiration must be positive"},
		{name: "negative access expiration", mutate: func(c *config.AuthConfig) { c.AccessTokenExpiration = -time.Minute }, wantErr: "access_token_expiration must be positive"},
		{name: "zero idle timeout", mutate: func(c *config.AuthConfig) { c.SessionIdleTimeout = 0 }, wantErr: "session_idle_timeout must be positive"},
		{name: "zero max lifetime", mutate: func(c *config.AuthConfig) { c.SessionMaxLifetime = 0 }, wantErr: "session_max_lifetime must be positive"},
		{name: "idle above max lifetime", mutate: func(c *config.AuthConfig) { c.SessionIdleTimeout = 13 * time.Hour }, wantErr: "session_idle_timeout must not exceed session_max_lifetime"},
		{name: "idle equal to max lifetime", mutate: func(c *config.AuthConfig) { c.SessionIdleTimeout = c.SessionMaxLifetime }},
		{name: "access above idle", mutate: func(c *config.AuthConfig) { c.AccessTokenExpiration = time.Hour }, wantErr: "access_token_expiration must not exceed session_idle_timeout"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			if c.JWTSecret != "" {
				assert.NotContains(t, err.Error(), c.JWTSecret)
			}
		})
	}
}

func TestLoadConfigAuthEnabledParsing(t *testing.T) {
	// Not parallel: subtests use t.Setenv.
	tests := []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "true", value: "true", want: true},
		{name: "TRUE", value: "TRUE", want: true},
		{name: "1", value: "1", want: true},
		{name: "false", value: "false"},
		{name: "0", value: "0"},
		{name: "empty means false", value: ""},
		{name: "surrounding spaces are trimmed", value: " true ", want: true},
		{name: "ture", value: "ture", wantErr: true},
		{name: "yes", value: "yes", wantErr: true},
		{name: "on", value: "on", wantErr: true},
		{name: "enabled", value: "enabled", wantErr: true},
		// This row replaces "invalid auth falls back to false" of
		// TestLoadConfigUIConfigEnv, which pre-dates auth.
		{name: "maybe", value: "maybe", wantErr: true},
		{name: "unique garbage is not echoed", value: "zz9plural-alpha", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv("AUTH_ENABLED", tt.value)
			t.Setenv("JWT_SECRET", "a-secret-of-sufficient-length")

			cfg, err := config.LoadConfig()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "AUTH_ENABLED")
				assert.NotContains(t, err.Error(), "zz9plural", "the error must not echo the value")
				assert.NotContains(t, err.Error(), "maybe")
				assert.NotContains(t, err.Error(), "ture")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Auth.Enabled)
		})
	}
}

package config_test

import (
	"testing"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigSessionStore(t *testing.T) {
	// Not parallel: subtests use t.Setenv.
	tests := []struct {
		name    string
		auth    string
		value   string
		want    string
		wantErr string
	}{
		{name: "default is mysql", auth: "true", value: "", want: config.SessionStoreMySQL},
		{name: "mysql", auth: "true", value: "mysql", want: config.SessionStoreMySQL},
		{name: "memory", auth: "true", value: "memory", want: config.SessionStoreMemory},
		{name: "case and spaces are ignored", auth: "true", value: "  Memory ", want: config.SessionStoreMemory},
		{name: "unknown value is an error", auth: "true", value: "redis", wantErr: "SESSION_STORE must be mysql or memory"},
		{name: "unknown value with auth off is not used", auth: "false", value: "redis", want: "redis"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv("AUTH_ENABLED", tt.auth)
			t.Setenv("JWT_SECRET", "1234567890123456")
			t.Setenv("SESSION_STORE", tt.value)

			cfg, err := config.LoadConfig()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Auth.SessionStore)
		})
	}
}

func TestAuthConfigValidateSessionStore(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", config.SessionStoreMySQL, config.SessionStoreMemory, "redis"} {
		c := config.AuthConfig{
			Enabled: true, JWTSecret: "1234567890123456", JWTExpiration: 1, AccessTokenExpiration: 1,
			SessionIdleTimeout: 1, SessionMaxLifetime: 1, RefreshTokenExpiration: 1, SessionStore: v,
		}
		err := c.Validate()
		if v == "redis" {
			assert.Error(t, err, v)
		} else {
			assert.NoError(t, err, "%q (empty is the default, mysql)", v)
		}
	}
}

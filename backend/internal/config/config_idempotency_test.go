package config_test

import (
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigIdempotencyDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.False(t, cfg.Idempotency.Enabled, "the module is off by default")
	assert.Equal(t, 24*time.Hour, cfg.Idempotency.TTL)
	assert.Equal(t, 60*time.Second, cfg.Idempotency.LockTimeout)
}

func TestLoadConfigIdempotency(t *testing.T) {
	tests := []struct {
		env         map[string]string
		name        string
		wantErr     string
		wantTTL     time.Duration
		wantLock    time.Duration
		wantEnabled bool
	}{
		{name: "on with defaults", env: map[string]string{"IDEMPOTENCY_ENABLED": "true"}, wantEnabled: true, wantTTL: 24 * time.Hour, wantLock: time.Minute},
		{name: "on with values", env: map[string]string{"IDEMPOTENCY_ENABLED": " 1 ", "IDEMPOTENCY_TTL": "2h", "IDEMPOTENCY_LOCK_TIMEOUT": "5s"}, wantEnabled: true, wantTTL: 2 * time.Hour, wantLock: 5 * time.Second},
		{name: "explicit false", env: map[string]string{"IDEMPOTENCY_ENABLED": "false"}, wantTTL: 24 * time.Hour, wantLock: time.Minute},
		{name: "typo in the switch", env: map[string]string{"IDEMPOTENCY_ENABLED": "yes"}, wantErr: "IDEMPOTENCY_ENABLED"},
		{name: "off reads a bad TTL leniently", env: map[string]string{"IDEMPOTENCY_TTL": "soon", "IDEMPOTENCY_LOCK_TIMEOUT": "-1s"}, wantTTL: 24 * time.Hour, wantLock: -time.Second},
		{name: "on rejects a TTL that does not parse", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_TTL": "soon"}, wantErr: "IDEMPOTENCY_TTL"},
		{name: "on rejects a lock timeout that does not parse", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_LOCK_TIMEOUT": "1 minute"}, wantErr: "IDEMPOTENCY_LOCK_TIMEOUT"},
		{name: "on rejects a zero TTL", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_TTL": "0s"}, wantErr: "IDEMPOTENCY_TTL must be positive"},
		{name: "on rejects a negative TTL", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_TTL": "-1h"}, wantErr: "IDEMPOTENCY_TTL must be positive"},
		{name: "on rejects a zero lock timeout", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_LOCK_TIMEOUT": "0"}, wantErr: "IDEMPOTENCY_LOCK_TIMEOUT must be positive"},
		{name: "on rejects a negative lock timeout", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_LOCK_TIMEOUT": "-5s"}, wantErr: "IDEMPOTENCY_LOCK_TIMEOUT must be positive"},
		{name: "on rejects a TTL shorter than the lock timeout", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_TTL": "30s", "IDEMPOTENCY_LOCK_TIMEOUT": "60s"}, wantErr: "IDEMPOTENCY_TTL must not be shorter than IDEMPOTENCY_LOCK_TIMEOUT"},
		{name: "on accepts a TTL equal to the lock timeout", env: map[string]string{"IDEMPOTENCY_ENABLED": "true", "IDEMPOTENCY_TTL": "60s", "IDEMPOTENCY_LOCK_TIMEOUT": "60s"}, wantEnabled: true, wantTTL: time.Minute, wantLock: time.Minute},
		{name: "off ignores a TTL shorter than the lock timeout", env: map[string]string{"IDEMPOTENCY_TTL": "30s", "IDEMPOTENCY_LOCK_TIMEOUT": "60s"}, wantTTL: 30 * time.Second, wantLock: time.Minute},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENV_FILE", "/nonexistent/.env")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := config.LoadConfig()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantEnabled, cfg.Idempotency.Enabled)
			assert.Equal(t, tt.wantTTL, cfg.Idempotency.TTL)
			assert.Equal(t, tt.wantLock, cfg.Idempotency.LockTimeout)
		})
	}
}

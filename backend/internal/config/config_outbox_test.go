package config_test

import (
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigOutboxDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.False(t, cfg.Outbox.Enabled, "the module is off by default")
	assert.Equal(t, 500*time.Millisecond, cfg.Outbox.PollInterval)
	assert.Equal(t, time.Hour, cfg.Outbox.Retention)
}

func TestLoadConfigOutbox(t *testing.T) {
	tests := []struct {
		env           map[string]string
		name          string
		wantErr       string
		wantPoll      time.Duration
		wantRetention time.Duration
		wantEnabled   bool
	}{
		{name: "on with defaults", env: map[string]string{"OUTBOX_ENABLED": "true"}, wantEnabled: true, wantPoll: 500 * time.Millisecond, wantRetention: time.Hour},
		{name: "on with values", env: map[string]string{"OUTBOX_ENABLED": " 1 ", "OUTBOX_POLL_INTERVAL": "2s", "OUTBOX_RETENTION": "30m"}, wantEnabled: true, wantPoll: 2 * time.Second, wantRetention: 30 * time.Minute},
		{name: "explicit false", env: map[string]string{"OUTBOX_ENABLED": "false"}, wantPoll: 500 * time.Millisecond, wantRetention: time.Hour},
		{name: "typo in the switch", env: map[string]string{"OUTBOX_ENABLED": "yes"}, wantErr: "OUTBOX_ENABLED"},
		{name: "off reads bad values leniently", env: map[string]string{"OUTBOX_POLL_INTERVAL": "soon", "OUTBOX_RETENTION": "-1s"}, wantPoll: 500 * time.Millisecond, wantRetention: -time.Second},
		{name: "off ignores a retention below the minimum", env: map[string]string{"OUTBOX_RETENTION": "5s"}, wantPoll: 500 * time.Millisecond, wantRetention: 5 * time.Second},
		{name: "on rejects a poll interval that does not parse", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_POLL_INTERVAL": "soon"}, wantErr: "OUTBOX_POLL_INTERVAL"},
		{name: "on rejects a retention that does not parse", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_RETENTION": "1 hour"}, wantErr: "OUTBOX_RETENTION"},
		{name: "on rejects a zero poll interval", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_POLL_INTERVAL": "0"}, wantErr: "OUTBOX_POLL_INTERVAL must be positive"},
		{name: "on rejects a negative poll interval", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_POLL_INTERVAL": "-1s"}, wantErr: "OUTBOX_POLL_INTERVAL must be positive"},
		{name: "on rejects a zero retention", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_RETENTION": "0s"}, wantErr: "OUTBOX_RETENTION must be positive"},
		{name: "on rejects a negative retention", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_RETENTION": "-1h"}, wantErr: "OUTBOX_RETENTION must be positive"},
		{name: "on rejects a retention below one minute", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_RETENTION": "59s"}, wantErr: "OUTBOX_RETENTION must be at least 1m"},
		{name: "on accepts a retention of one minute", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_RETENTION": "1m"}, wantEnabled: true, wantPoll: 500 * time.Millisecond, wantRetention: time.Minute},
		{name: "on rejects a retention below ten poll intervals", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_POLL_INTERVAL": "10s", "OUTBOX_RETENTION": "99s"}, wantErr: "OUTBOX_RETENTION must be at least 10 times OUTBOX_POLL_INTERVAL"},
		{name: "on accepts a retention of ten poll intervals", env: map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_POLL_INTERVAL": "10s", "OUTBOX_RETENTION": "100s"}, wantEnabled: true, wantPoll: 10 * time.Second, wantRetention: 100 * time.Second},
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
			assert.Equal(t, tt.wantEnabled, cfg.Outbox.Enabled)
			assert.Equal(t, tt.wantPoll, cfg.Outbox.PollInterval)
			assert.Equal(t, tt.wantRetention, cfg.Outbox.Retention)
		})
	}
}

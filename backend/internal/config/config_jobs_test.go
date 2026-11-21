package config_test

import (
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigJobsDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	j := cfg.Jobs
	assert.False(t, j.Enabled, "the module is off by default")
	assert.Equal(t, 4, j.Workers)
	assert.Equal(t, time.Second, j.PollInterval)
	assert.Equal(t, 5*time.Minute, j.VisibilityTimeout)
	assert.Equal(t, 5, j.MaxAttempts)
	assert.Equal(t, 10*time.Second, j.BackoffBase)
	assert.Equal(t, time.Hour, j.BackoffMax)
	assert.Equal(t, 24*time.Hour, j.Retention)
	assert.Equal(t, 168*time.Hour, j.DeadRetention)
	assert.Equal(t, 5*time.Minute, j.StaleAfter)
}

func TestLoadConfigJobs(t *testing.T) {
	on := func(kv ...string) map[string]string {
		m := map[string]string{"JOBS_ENABLED": "true"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		env     map[string]string
		check   func(t *testing.T, c config.JobsConfig)
		name    string
		wantErr string
	}{
		{name: "on with defaults", env: on(), check: func(t *testing.T, c config.JobsConfig) {
			assert.True(t, c.Enabled)
			assert.Equal(t, 4, c.Workers)
		}},
		{name: "on with values", env: on("JOBS_WORKERS", "64", "JOBS_POLL_INTERVAL", "100ms", "JOBS_VISIBILITY_TIMEOUT", "10s", "JOBS_MAX_ATTEMPTS", "50",
			"JOBS_BACKOFF_BASE", "1s", "JOBS_BACKOFF_MAX", "1s", "JOBS_RETENTION", "1m", "JOBS_DEAD_RETENTION", "1m", "JOBS_STALE_AFTER", "10s"),
			check: func(t *testing.T, c config.JobsConfig) {
				assert.Equal(t, 64, c.Workers)
				assert.Equal(t, 100*time.Millisecond, c.PollInterval)
				assert.Equal(t, 10*time.Second, c.VisibilityTimeout)
				assert.Equal(t, 50, c.MaxAttempts)
				assert.Equal(t, time.Second, c.BackoffBase)
				assert.Equal(t, time.Second, c.BackoffMax)
				assert.Equal(t, time.Minute, c.Retention)
				assert.Equal(t, time.Minute, c.DeadRetention)
				assert.Equal(t, 10*time.Second, c.StaleAfter)
			}},
		{name: "switch accepts 1", env: map[string]string{"JOBS_ENABLED": " 1 "}, check: func(t *testing.T, c config.JobsConfig) { assert.True(t, c.Enabled) }},
		{name: "explicit false", env: map[string]string{"JOBS_ENABLED": "false"}, check: func(t *testing.T, c config.JobsConfig) { assert.False(t, c.Enabled) }},
		{name: "typo in the switch", env: map[string]string{"JOBS_ENABLED": "yes"}, wantErr: "JOBS_ENABLED"},
		{name: "off reads bad values leniently", env: map[string]string{"JOBS_WORKERS": "many", "JOBS_POLL_INTERVAL": "soon", "JOBS_BACKOFF_MAX": "-1s", "JOBS_MAX_ATTEMPTS": "0"},
			check: func(t *testing.T, c config.JobsConfig) {
				assert.False(t, c.Enabled)
				assert.Equal(t, 4, c.Workers, "an unparsable value falls back")
				assert.Equal(t, time.Second, c.PollInterval)
				assert.Equal(t, -time.Second, c.BackoffMax, "a parsable value is kept and not validated")
				assert.Equal(t, 0, c.MaxAttempts)
			}},
		{name: "on rejects a workers value that does not parse", env: on("JOBS_WORKERS", "many"), wantErr: "JOBS_WORKERS"},
		{name: "on rejects zero workers", env: on("JOBS_WORKERS", "0"), wantErr: "JOBS_WORKERS must be from 1 to 64"},
		{name: "on rejects 65 workers", env: on("JOBS_WORKERS", "65"), wantErr: "JOBS_WORKERS must be from 1 to 64"},
		{name: "on rejects a poll interval that does not parse", env: on("JOBS_POLL_INTERVAL", "soon"), wantErr: "JOBS_POLL_INTERVAL"},
		{name: "on rejects a poll interval below 100ms", env: on("JOBS_POLL_INTERVAL", "99ms"), wantErr: "JOBS_POLL_INTERVAL must be at least 100ms"},
		{name: "on rejects a visibility timeout below 10s", env: on("JOBS_VISIBILITY_TIMEOUT", "9s"), wantErr: "JOBS_VISIBILITY_TIMEOUT must be at least 10s"},
		{name: "on rejects zero max attempts", env: on("JOBS_MAX_ATTEMPTS", "0"), wantErr: "JOBS_MAX_ATTEMPTS must be from 1 to 50"},
		{name: "on rejects 51 max attempts", env: on("JOBS_MAX_ATTEMPTS", "51"), wantErr: "JOBS_MAX_ATTEMPTS must be from 1 to 50"},
		{name: "on rejects a backoff base below 1s", env: on("JOBS_BACKOFF_BASE", "999ms"), wantErr: "JOBS_BACKOFF_BASE must be at least 1s"},
		{name: "on rejects a backoff max below the base", env: on("JOBS_BACKOFF_BASE", "1m", "JOBS_BACKOFF_MAX", "59s"), wantErr: "JOBS_BACKOFF_MAX must be at least JOBS_BACKOFF_BASE"},
		{name: "on rejects a retention below 1m", env: on("JOBS_RETENTION", "59s"), wantErr: "JOBS_RETENTION must be at least 1m"},
		{name: "on rejects a dead retention below the retention", env: on("JOBS_RETENTION", "2h", "JOBS_DEAD_RETENTION", "1h"), wantErr: "JOBS_DEAD_RETENTION must be at least JOBS_RETENTION"},
		{name: "on rejects a stale threshold below 10s", env: on("JOBS_STALE_AFTER", "9s"), wantErr: "JOBS_STALE_AFTER must be at least 10s"},
		{name: "on rejects a duration that does not parse", env: on("JOBS_RETENTION", "1 day"), wantErr: "JOBS_RETENTION"},
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
			tt.check(t, cfg.Jobs)
		})
	}
}

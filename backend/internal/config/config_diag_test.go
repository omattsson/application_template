package config_test

import (
	"testing"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigDiagnosticsDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.False(t, cfg.Server.PprofEnabled, "pprof is off by default")
	assert.Equal(t, "127.0.0.1:6060", cfg.Server.PprofAddr, "pprof listens on loopback by default")
	assert.False(t, cfg.Server.PprofAllowNonLoopback)
	assert.Zero(t, cfg.Server.PprofMutexFraction)
	assert.Zero(t, cfg.Server.PprofBlockRate)
	assert.Equal(t, ":9090", cfg.Otel.MetricsAddr)
}

func TestLoadConfigDiagnosticsValues(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")
	t.Setenv("PPROF_ENABLED", "true")
	t.Setenv("PPROF_ADDR", "localhost:7070")
	t.Setenv("PPROF_MUTEX_FRACTION", "5")
	t.Setenv("PPROF_BLOCK_RATE", "1000")
	t.Setenv("METRICS_ENABLED", "true")
	t.Setenv("METRICS_ADDR", "127.0.0.1:9191")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.True(t, cfg.Server.PprofEnabled)
	assert.Equal(t, "localhost:7070", cfg.Server.PprofAddr)
	assert.Equal(t, 5, cfg.Server.PprofMutexFraction)
	assert.Equal(t, 1000, cfg.Server.PprofBlockRate)
	assert.Equal(t, "127.0.0.1:9191", cfg.Otel.MetricsAddr)
}

func TestLoadConfigDiagnosticsValidation(t *testing.T) {
	tests := []struct {
		env     map[string]string
		name    string
		wantErr string // empty: the load succeeds
	}{
		{name: "typo in PPROF_ENABLED", env: map[string]string{"PPROF_ENABLED": "ture"}, wantErr: "PPROF_ENABLED"},
		{name: "pprof on a public address is refused", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": "0.0.0.0:6060"}, wantErr: "PPROF_ALLOW_NON_LOOPBACK"},
		{name: "pprof on all interfaces (no host) is refused", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": ":6060"}, wantErr: "PPROF_ALLOW_NON_LOOPBACK"},
		{name: "pprof on a host name is refused", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": "example.com:6060"}, wantErr: "PPROF_ALLOW_NON_LOOPBACK"},
		{name: "pprof non-loopback needs a boolean opt in", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": ":6060", "PPROF_ALLOW_NON_LOOPBACK": "maybe"}, wantErr: "PPROF_ALLOW_NON_LOOPBACK"},
		{name: "pprof non-loopback with the explicit opt in", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": ":6060", "PPROF_ALLOW_NON_LOOPBACK": "true"}},
		{name: "pprof on IPv6 loopback", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": "[::1]:6060"}},
		{name: "pprof address without a port", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": "127.0.0.1"}, wantErr: "PPROF_ADDR"},
		{name: "pprof port out of range", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": "127.0.0.1:70000"}, wantErr: "PPROF_ADDR"},
		{name: "pprof port is not a number", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_ADDR": "127.0.0.1:http"}, wantErr: "PPROF_ADDR"},
		{name: "negative mutex fraction", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_MUTEX_FRACTION": "-1"}, wantErr: "PPROF_MUTEX_FRACTION"},
		{name: "block rate that is not a number", env: map[string]string{"PPROF_ENABLED": "true", "PPROF_BLOCK_RATE": "fast"}, wantErr: "PPROF_BLOCK_RATE"},
		{
			// Nothing reads the pprof settings while pprof is off.
			name: "bad pprof settings are ignored while disabled",
			env:  map[string]string{"PPROF_ADDR": "nonsense", "PPROF_BLOCK_RATE": "fast", "PPROF_MUTEX_FRACTION": "-3"},
		},
		{name: "metrics address without a port", env: map[string]string{"METRICS_ENABLED": "true", "METRICS_ADDR": "localhost"}, wantErr: "METRICS_ADDR"},
		{name: "metrics port out of range", env: map[string]string{"METRICS_ENABLED": "true", "METRICS_ADDR": ":99999"}, wantErr: "METRICS_ADDR"},
		{name: "bad metrics address is ignored while disabled", env: map[string]string{"METRICS_ADDR": "nonsense"}},
		{name: "metrics on a chosen port", env: map[string]string{"METRICS_ENABLED": "true", "METRICS_ADDR": ":9464"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENV_FILE", "/nonexistent/.env")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := config.LoadConfig()
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6060", true},
		{"127.5.5.5:1", true},
		{"[::1]:6060", true},
		{"localhost:6060", true},
		{"LOCALHOST:6060", true},
		{":6060", false},
		{"0.0.0.0:6060", false},
		{"[::]:6060", false},
		{"10.0.0.5:6060", false},
		{"example.com:6060", false},
		{"not an address", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, config.IsLoopbackAddr(tt.addr))
		})
	}
}

package config_test

import (
	"testing"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigOtelDefaults(t *testing.T) {
	// Not parallel: t.Setenv changes the process environment.
	t.Setenv("ENV_FILE", "/nonexistent/.env")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.False(t, cfg.Otel.Enabled, "OTEL_ENABLED defaults to false")
	assert.False(t, cfg.Otel.MetricsEnabled, "METRICS_ENABLED defaults to false")
	assert.Equal(t, "localhost:4317", cfg.Otel.Endpoint)
	assert.Equal(t, "backend-api", cfg.Otel.ServiceName)
	assert.InDelta(t, 1.0, cfg.Otel.SampleRate, 0)
}

func TestLoadConfigOtelValues(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")
	t.Setenv("OTEL_ENABLED", "true")
	t.Setenv("METRICS_ENABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4317")
	t.Setenv("OTEL_SERVICE_NAME", "my-service")
	t.Setenv("OTEL_TRACE_SAMPLE_RATE", "0.25")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.True(t, cfg.Otel.Enabled)
	assert.True(t, cfg.Otel.MetricsEnabled)
	assert.Equal(t, "collector:4317", cfg.Otel.Endpoint)
	assert.Equal(t, "my-service", cfg.Otel.ServiceName)
	assert.InDelta(t, 0.25, cfg.Otel.SampleRate, 0)
}

func TestLoadConfigOtelValidation(t *testing.T) {
	tests := []struct {
		env     map[string]string
		name    string
		wantErr string // empty: the load succeeds
	}{
		{name: "typo in OTEL_ENABLED", env: map[string]string{"OTEL_ENABLED": "ture"}, wantErr: "OTEL_ENABLED"},
		{name: "typo in METRICS_ENABLED", env: map[string]string{"METRICS_ENABLED": "yes please"}, wantErr: "METRICS_ENABLED"},
		{
			name:    "sample rate above 1",
			env:     map[string]string{"OTEL_ENABLED": "true", "OTEL_TRACE_SAMPLE_RATE": "1.5"},
			wantErr: "OTEL_TRACE_SAMPLE_RATE",
		},
		{
			name:    "negative sample rate",
			env:     map[string]string{"OTEL_ENABLED": "true", "OTEL_TRACE_SAMPLE_RATE": "-0.1"},
			wantErr: "OTEL_TRACE_SAMPLE_RATE",
		},
		{
			name:    "sample rate that is not a number",
			env:     map[string]string{"OTEL_ENABLED": "true", "OTEL_TRACE_SAMPLE_RATE": "half"},
			wantErr: "OTEL_TRACE_SAMPLE_RATE",
		},
		{
			name:    "NaN sample rate",
			env:     map[string]string{"OTEL_ENABLED": "true", "OTEL_TRACE_SAMPLE_RATE": "NaN"},
			wantErr: "OTEL_TRACE_SAMPLE_RATE",
		},
		{
			name:    "endpoint with a scheme",
			env:     map[string]string{"OTEL_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317"},
			wantErr: "OTEL_EXPORTER_OTLP_ENDPOINT",
		},
		{
			name:    "endpoint without a port",
			env:     map[string]string{"OTEL_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "collector"},
			wantErr: "OTEL_EXPORTER_OTLP_ENDPOINT",
		},
		{
			name:    "blank service name",
			env:     map[string]string{"OTEL_ENABLED": "true", "OTEL_SERVICE_NAME": "   "},
			wantErr: "OTEL_SERVICE_NAME",
		},
		{
			// Nothing reads the OTEL settings while OTEL_ENABLED is false:
			// a bad value must not stop the start.
			name: "bad settings are ignored while disabled",
			env:  map[string]string{"OTEL_TRACE_SAMPLE_RATE": "half", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://x"},
		},
		{
			name: "valid enabled settings",
			env:  map[string]string{"OTEL_ENABLED": "true", "OTEL_TRACE_SAMPLE_RATE": "0"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENV_FILE", "/nonexistent/.env")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := config.LoadConfig()
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.NotNil(t, cfg)

				return
			}

			require.Error(t, err)
			assert.Nil(t, cfg)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

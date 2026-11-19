package config_test

import (
	"testing"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigCompressionDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.True(t, cfg.Compression.Enabled, "compression is on by default")
	assert.Equal(t, 1024, cfg.Compression.MinSize)
}

func TestLoadConfigCompression(t *testing.T) {
	tests := []struct {
		env         map[string]string
		name        string
		wantErr     string
		wantMin     int
		wantEnabled bool
	}{
		{name: "explicit on", env: map[string]string{"COMPRESSION_ENABLED": " true "}, wantEnabled: true, wantMin: 1024},
		{name: "explicit off", env: map[string]string{"COMPRESSION_ENABLED": "false"}, wantMin: 1024},
		{name: "off with 0", env: map[string]string{"COMPRESSION_ENABLED": "0"}, wantMin: 1024},
		{name: "empty values keep the defaults", env: map[string]string{"COMPRESSION_ENABLED": "", "COMPRESSION_MIN_SIZE": ""}, wantEnabled: true, wantMin: 1024},
		{name: "typo in the switch", env: map[string]string{"COMPRESSION_ENABLED": "yes"}, wantErr: "COMPRESSION_ENABLED"},
		{name: "custom minimum", env: map[string]string{"COMPRESSION_MIN_SIZE": " 256 "}, wantEnabled: true, wantMin: 256},
		{name: "one byte", env: map[string]string{"COMPRESSION_MIN_SIZE": "1"}, wantEnabled: true, wantMin: 1},
		{name: "the largest minimum", env: map[string]string{"COMPRESSION_MIN_SIZE": "1048576"}, wantEnabled: true, wantMin: 1 << 20},
		{name: "on rejects text", env: map[string]string{"COMPRESSION_MIN_SIZE": "big"}, wantErr: "COMPRESSION_MIN_SIZE"},
		{name: "on rejects a decimal", env: map[string]string{"COMPRESSION_MIN_SIZE": "1.5"}, wantErr: "COMPRESSION_MIN_SIZE"},
		{name: "on rejects zero", env: map[string]string{"COMPRESSION_MIN_SIZE": "0"}, wantErr: "COMPRESSION_MIN_SIZE must be positive"},
		{name: "on rejects a negative number", env: map[string]string{"COMPRESSION_MIN_SIZE": "-1"}, wantErr: "COMPRESSION_MIN_SIZE must be positive"},
		{name: "on rejects a minimum over the cap", env: map[string]string{"COMPRESSION_MIN_SIZE": "1048577"}, wantErr: "COMPRESSION_MIN_SIZE must not be larger than 1048576"},
		{name: "on rejects a number over int32", env: map[string]string{"COMPRESSION_MIN_SIZE": "99999999999"}, wantErr: "COMPRESSION_MIN_SIZE"},
		{name: "off reads text leniently", env: map[string]string{"COMPRESSION_ENABLED": "false", "COMPRESSION_MIN_SIZE": "big"}, wantMin: 1024},
		{name: "off ignores zero", env: map[string]string{"COMPRESSION_ENABLED": "false", "COMPRESSION_MIN_SIZE": "0"}},
		{name: "off ignores a minimum over the cap", env: map[string]string{"COMPRESSION_ENABLED": "false", "COMPRESSION_MIN_SIZE": "5000000"}, wantMin: 5000000},
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
			assert.Equal(t, tt.wantEnabled, cfg.Compression.Enabled)
			assert.Equal(t, tt.wantMin, cfg.Compression.MinSize)
		})
	}
}

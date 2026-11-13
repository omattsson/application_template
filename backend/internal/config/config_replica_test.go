package config_test

import (
	"testing"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigReplicaDefaults(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.False(t, cfg.Replica.LeaderElectionEnabled, "leader election is off by default")
	assert.False(t, cfg.Replica.HeartbeatEnabled, "the heartbeat is off by default")
}

func TestLoadConfigReplica(t *testing.T) {
	tests := []struct {
		env          map[string]string
		name         string
		wantErr      string
		wantElection bool
		wantBeat     bool
	}{
		{name: "both on", env: map[string]string{"LEADER_ELECTION_ENABLED": "true", "REPLICA_HEARTBEAT_ENABLED": "true"}, wantElection: true, wantBeat: true},
		{name: "election only", env: map[string]string{"LEADER_ELECTION_ENABLED": "1"}, wantElection: true},
		{name: "heartbeat only", env: map[string]string{"REPLICA_HEARTBEAT_ENABLED": " TRUE "}, wantBeat: true},
		{name: "explicit false", env: map[string]string{"LEADER_ELECTION_ENABLED": "false", "REPLICA_HEARTBEAT_ENABLED": "0"}},
		{name: "typo in LEADER_ELECTION_ENABLED", env: map[string]string{"LEADER_ELECTION_ENABLED": "yes"}, wantErr: "LEADER_ELECTION_ENABLED"},
		{name: "typo in REPLICA_HEARTBEAT_ENABLED", env: map[string]string{"REPLICA_HEARTBEAT_ENABLED": "on"}, wantErr: "REPLICA_HEARTBEAT_ENABLED"},
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
			assert.Equal(t, tt.wantElection, cfg.Replica.LeaderElectionEnabled)
			assert.Equal(t, tt.wantBeat, cfg.Replica.HeartbeatEnabled)
		})
	}
}

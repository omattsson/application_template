package config

// ReplicaConfig holds the switches for running several backend replicas on
// one database. Both are off by default, which keeps the single-process
// behaviour: every process runs the leader workers and writes no heartbeat.
type ReplicaConfig struct {
	// LeaderElectionEnabled is LEADER_ELECTION_ENABLED (default false, strict
	// boolean). With true the leader workers run only on the replica that
	// holds the MySQL lease row (leader.Lease); with false every process
	// starts them at once.
	LeaderElectionEnabled bool
	// HeartbeatEnabled is REPLICA_HEARTBEAT_ENABLED (default false, strict
	// boolean). With true every process writes its row in
	// replica_heartbeats (replica.Heartbeat).
	HeartbeatEnabled bool
}

// loadReplicaConfig reads the replica switches. A value that is set but not
// a boolean is an error: a typo must not silently run the leader workers on
// every replica.
func loadReplicaConfig() (ReplicaConfig, error) {
	var (
		c   ReplicaConfig
		err error
	)

	if c.LeaderElectionEnabled, err = parseEnvBoolStrict("LEADER_ELECTION_ENABLED", false); err != nil {
		return ReplicaConfig{}, err
	}

	if c.HeartbeatEnabled, err = parseEnvBoolStrict("REPLICA_HEARTBEAT_ENABLED", false); err != nil {
		return ReplicaConfig{}, err
	}

	return c, nil
}

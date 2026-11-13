package models

import (
	"context"
	"time"
)

// ReplicaHeartbeat is the last sign of life of one backend process (table
// replica_heartbeats). With REPLICA_HEARTBEAT_ENABLED=true every process
// writes its row every heartbeat interval and deletes it on a clean
// shutdown, so the table lists the processes that run now (plus, until the
// cleanup, the ones that crashed). Its definition is copied into migration
// 20260101000003 (replicaHeartbeatV1); a change here needs a new migration,
// and TestReplicaTablesMatchModels fails without one.
//
// The ID is the process identity: the host name plus a random suffix per
// process (replica.ProcessID). A restarted process gets a new ID, so the row
// of a stopped process never becomes fresh again. Each process writes only
// its own row, so the table has no version column.
type ReplicaHeartbeat struct {
	// LastSeen is the time of the last heartbeat, from the database server
	// clock (UTC_TIMESTAMP on MySQL), never from the replica's clock.
	LastSeen time.Time `json:"last_seen" gorm:"type:datetime(3);not null;index:idx_replica_heartbeats_last_seen"`
	// ID is the process identity (at most 253 characters).
	ID string `json:"id" gorm:"primaryKey;size:253"`
}

// TableName returns the table name of ReplicaHeartbeat.
func (ReplicaHeartbeat) TableName() string { return "replica_heartbeats" }

// ReplicaHeartbeatRepository stores the heartbeats of the backend processes.
// All times are written and compared in SQL with the database server clock,
// so the clocks of the replicas do not matter.
type ReplicaHeartbeatRepository interface {
	// Beat inserts or updates the row of id with last_seen = the current
	// database time.
	Beat(ctx context.Context, id string) error
	// Remove deletes the row of id (clean shutdown). A missing row is not an
	// error.
	Remove(ctx context.Context, id string) error
	// DeleteOlderThan deletes the rows whose last heartbeat is more than
	// maxAge old (database time) and returns the number of deleted rows.
	DeleteOlderThan(ctx context.Context, maxAge time.Duration) (int64, error)
}

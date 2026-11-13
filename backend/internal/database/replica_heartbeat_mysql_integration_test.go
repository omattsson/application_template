//go:build integration

package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDatabaseReplicaHeartbeat runs the heartbeat repository on MySQL: the
// columns have the migrated types, Beat upserts with the server's UTC clock,
// DeleteOlderThan compares in SQL and Remove deletes.
func TestDatabaseReplicaHeartbeat(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	repo := NewGORMReplicaHeartbeatRepository(db.DB)
	ctx := context.Background()

	for col, want := range map[string]string{"last_seen": "datetime(3)", "id": "varchar(253)"} {
		var got string
		require.NoError(t, db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'replica_heartbeats' AND column_name = ?", col).Scan(&got).Error)
		assert.Equal(t, want, got, col)
	}
	for col, want := range map[string]string{"expires_at": "datetime(3)", "holder": "varchar(253)", "name": "varchar(64)"} {
		var got string
		require.NoError(t, db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'leader_leases' AND column_name = ?", col).Scan(&got).Error)
		assert.Equal(t, want, got, col)
	}

	id := "it-" + uuid.New().String()[:8]
	old := "it-old-" + uuid.New().String()[:8]
	t.Cleanup(func() { _ = db.Exec("DELETE FROM replica_heartbeats WHERE id IN (?, ?)", id, old).Error })

	require.NoError(t, repo.Beat(ctx, id))
	ageMicros := func() int64 {
		var age int64
		require.NoError(t, db.Raw("SELECT TIMESTAMPDIFF(MICROSECOND, last_seen, UTC_TIMESTAMP(3)) FROM replica_heartbeats WHERE id = ?", id).Scan(&age).Error)
		return age
	}
	assert.Less(t, ageMicros(), int64(5*time.Second/time.Microsecond), "last_seen is the server's UTC time")
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, repo.Beat(ctx, id), "the second beat updates the row")
	var n int64
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM replica_heartbeats WHERE id = ?", id).Scan(&n).Error)
	assert.Equal(t, int64(1), n)

	require.NoError(t, db.Exec("INSERT INTO replica_heartbeats (id, last_seen) VALUES (?, UTC_TIMESTAMP(3) - INTERVAL 1 HOUR)", old).Error)
	deleted, err := repo.DeleteOlderThan(ctx, 30*time.Minute)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(1))
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM replica_heartbeats WHERE id IN (?, ?)", id, old).Scan(&n).Error)
	assert.Equal(t, int64(1), n, "only the old row is deleted")

	require.NoError(t, repo.Remove(ctx, id))
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM replica_heartbeats WHERE id = ?", id).Scan(&n).Error)
	assert.Zero(t, n)
}

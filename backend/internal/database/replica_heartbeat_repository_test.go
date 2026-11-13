package database

import (
	"context"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lastSeenOf(t *testing.T, repo *GORMReplicaHeartbeatRepository, id string) (string, bool) {
	t.Helper()
	var rows []string
	require.NoError(t, repo.DB.Raw("SELECT last_seen FROM replica_heartbeats WHERE id = ?", id).Scan(&rows).Error)
	if len(rows) == 0 {
		return "", false
	}
	return rows[0], true
}

func TestReplicaHeartbeatBeatUpsertsAndRemoves(t *testing.T) {
	t.Parallel()
	repo := NewGORMReplicaHeartbeatRepository(openMigratedDB(t))
	ctx := context.Background()

	require.NoError(t, repo.Beat(ctx, "p1"))
	first, ok := lastSeenOf(t, repo, "p1")
	require.True(t, ok, "the first beat inserts the row")

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, repo.Beat(ctx, "p1"))
	second, ok := lastSeenOf(t, repo, "p1")
	require.True(t, ok)
	assert.Greater(t, second, first, "a second beat moves last_seen forward")

	var n int64
	require.NoError(t, repo.DB.Model(&models.ReplicaHeartbeat{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "one row per process")

	require.NoError(t, repo.Remove(ctx, "p1"))
	_, ok = lastSeenOf(t, repo, "p1")
	assert.False(t, ok)
	require.NoError(t, repo.Remove(ctx, "p1"), "a missing row is not an error")
}

func TestReplicaHeartbeatDeleteOlderThan(t *testing.T) {
	t.Parallel()
	repo := NewGORMReplicaHeartbeatRepository(openMigratedDB(t))
	ctx := context.Background()
	require.NoError(t, repo.Beat(ctx, "fresh"))
	require.NoError(t, repo.DB.Exec("INSERT INTO replica_heartbeats (id, last_seen) VALUES (?, ?)", "old", "2000-01-01 00:00:00.000").Error)

	n, err := repo.DeleteOlderThan(ctx, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	_, ok := lastSeenOf(t, repo, "fresh")
	assert.True(t, ok)
	_, ok = lastSeenOf(t, repo, "old")
	assert.False(t, ok)
}

func TestReplicaHeartbeatErrors(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	repo := NewGORMReplicaHeartbeatRepository(db)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	ctx := context.Background()
	require.Error(t, repo.Beat(ctx, "p1"))
	require.Error(t, repo.Remove(ctx, "p1"))
	_, err = repo.DeleteOlderThan(ctx, time.Minute)
	require.Error(t, err)
}

func TestReplicaHeartbeatTimeSQL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "UTC_TIMESTAMP(3)", heartbeatNowSQL("mysql"))
	assert.Equal(t, "(UTC_TIMESTAMP(3) - INTERVAL ? MICROSECOND)", heartbeatAgoSQL("mysql"))
	assert.Contains(t, heartbeatNowSQL("sqlite"), "strftime")
	assert.Equal(t, int64(90_000_000), heartbeatAgoArg("mysql", 90*time.Second))
	assert.Equal(t, "-90.000 seconds", heartbeatAgoArg("sqlite", 90*time.Second))
}

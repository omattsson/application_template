//go:build integration

package leader

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/database"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The integration tests run the lease on a real MySQL with one connection
// pool per replica, as separate processes would. Each test uses a lease
// name of its own, so they can run in parallel and with -count=3.

func mysqlLeaseConfig() *config.Config {
	getenv := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return &config.Config{
		App: config.AppConfig{Name: "testapp", Environment: "testing"},
		Database: config.DatabaseConfig{
			Host:            getenv("DB_HOST", "localhost"),
			Port:            getenv("DB_PORT", "3306"),
			User:            getenv("DB_USER", "appuser"),
			Password:        getenv("DB_PASSWORD", "apppass"),
			DBName:          getenv("DB_NAME", "app"),
			MaxOpenConns:    4,
			MaxIdleConns:    2,
			ConnMaxLifetime: 5 * time.Minute,
		},
	}
}

// openMySQLReplica opens a new migrated pool with a fault injector.
func openMySQLReplica(t *testing.T) *replicaDB {
	t.Helper()
	d, err := database.NewFromAppConfig(mysqlLeaseConfig())
	require.NoError(t, err)
	require.NoError(t, d.AutoMigrate())
	sqlDB, err := d.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	r := &replicaDB{db: d.DB}
	require.NoError(t, d.DB.Callback().Raw().Before("gorm:raw").Register("test:fault", r.inject))
	require.NoError(t, d.DB.Callback().Row().Before("gorm:row").Register("test:fault", r.inject))
	return r
}

func uniqueLeaseName(t *testing.T, admin *replicaDB) string {
	t.Helper()
	name := "it-" + uuid.New().String()[:8]
	t.Cleanup(func() {
		_ = admin.db.Exec("DELETE FROM leader_leases WHERE name = ?", name).Error
	})
	return name
}

// TestDatabaseLeaseTwoReplicas: two replicas with pools of their own elect
// exactly one leader; when the leader's database hangs it stops before its
// lease can expire, and only then the other replica starts; a cancelled
// leader releases, and the other takes over before the lease would expire.
func TestDatabaseLeaseTwoReplicas(t *testing.T) {
	t.Parallel()
	admin := openMySQLReplica(t)
	name := uniqueLeaseName(t, admin)
	tr := newTracker()
	a := startReplica(t, tr, openMySQLReplica(t), name, "a", testLeaseConfig)
	b := startReplica(t, tr, openMySQLReplica(t), name, "b", testLeaseConfig)

	tr.waitFor(t, 3*time.Second, "one leader", func() bool { return len(tr.leaders()) == 1 })
	time.Sleep(3 * testLeaseConfig.Duration)
	leaders := tr.leaders()
	require.Len(t, leaders, 1)
	first, second := a, b
	if leaders[0] == "b" {
		first, second = b, a
	}
	termsFirst, maxActive := tr.snapshot(first.id)
	termsSecond, _ := tr.snapshot(second.id)
	assert.Len(t, termsFirst, 1, "the leader renewed without a gap")
	assert.Empty(t, termsSecond)
	assert.Equal(t, 1, maxActive)

	// The column is DATETIME(3) and the expiry is the server's UTC clock
	// plus Duration.
	var colType string
	require.NoError(t, admin.db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'leader_leases' AND column_name = 'expires_at'").Scan(&colType).Error)
	assert.Equal(t, "datetime(3)", colType)
	var leftMicros int64
	require.NoError(t, admin.db.Raw("SELECT TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(3), expires_at) FROM leader_leases WHERE name = ?", name).Scan(&leftMicros).Error)
	assert.Greater(t, leftMicros, int64(0))
	assert.LessOrEqual(t, leftMicros, testLeaseConfig.Duration.Microseconds())

	// The leader's database hangs.
	hung := time.Now()
	first.db.set(faultHang)
	tr.waitFor(t, 3*time.Second, "takeover", func() bool {
		ts, _ := tr.snapshot(second.id)
		return len(ts) == 1
	})
	termsFirst, _ = tr.snapshot(first.id)
	termsSecond, maxActive = tr.snapshot(second.id)
	require.False(t, termsFirst[0].stopped.IsZero())
	assert.Less(t, termsFirst[0].stopped.Sub(hung), testLeaseConfig.Duration, "the hung leader stops before its lease can expire")
	assert.True(t, termsFirst[0].stopped.Before(termsSecond[0].started), "no overlap")
	assert.Equal(t, 1, maxActive)

	// The first replica's database works again: it campaigns but the second
	// holds the lease. Then the second is cancelled and releases.
	first.db.set(faultNone)
	time.Sleep(3 * testLeaseConfig.RetryPeriod)
	assert.Equal(t, []string{second.id}, tr.leaders())
	cancelled := time.Now()
	second.stop(t)
	tr.waitFor(t, 3*time.Second, "first leads again", func() bool {
		ts, _ := tr.snapshot(first.id)
		return len(ts) == 2
	})
	termsFirst, maxActive = tr.snapshot(first.id)
	assert.Less(t, termsFirst[1].started.Sub(cancelled), testLeaseConfig.Duration, "the release lets the next replica in before expiry")
	assert.Equal(t, 1, maxActive)
}

// Several replicas race for an empty row: the duplicate-key losers are
// followers.
func TestDatabaseLeaseConcurrentFirstAcquire(t *testing.T) {
	t.Parallel()
	admin := openMySQLReplica(t)
	name := uniqueLeaseName(t, admin)
	tr := newTracker()
	for i := range 4 {
		startReplica(t, tr, openMySQLReplica(t), name, fmt.Sprintf("r%d", i), testLeaseConfig)
	}
	tr.waitFor(t, 3*time.Second, "one leader", func() bool { return len(tr.leaders()) == 1 })
	time.Sleep(2 * testLeaseConfig.Duration)
	_, maxActive := tr.snapshot("r0")
	assert.Equal(t, 1, maxActive)
	assert.Len(t, tr.leaders(), 1)
}

// MySQL reports changed rows, not matched rows: an UPDATE that matches the
// row but changes nothing (a renewal in the same millisecond) gives 0. This
// is why tryAcquire reads the holder after an UPDATE of 0 rows instead of
// treating 0 as "not acquired" (the fallback itself is covered by
// TestLeaseZeroRowsAffectedStillHeld).
func TestDatabaseLeaseRenewWithoutChange(t *testing.T) {
	t.Parallel()
	admin := openMySQLReplica(t)
	name := uniqueLeaseName(t, admin)
	l := NewLease(admin.db, name, "a", testLeaseConfig)
	ctx := context.Background()
	ok, _, err := l.tryAcquire(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	res := admin.db.Exec("UPDATE leader_leases SET holder = ? WHERE name = ?", "a", name)
	require.NoError(t, res.Error)
	assert.Zero(t, res.RowsAffected, "MySQL counts changed rows, not matched rows")
	ok, holder, err := l.tryAcquire(ctx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "a", holder)
	other := NewLease(admin.db, name, "b", testLeaseConfig)
	ok, holder, err = other.tryAcquire(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "a", holder)
}

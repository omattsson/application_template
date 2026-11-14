package replica

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/database"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// lockedBuffer is a bytes.Buffer that several goroutines may write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestHeartbeatOutageIsLoggedOnce: with the database down every heartbeat
// write fails. The heartbeat logs the outage once (its own line); the GORM
// logger of the application database must not add an Error line per write.
func TestHeartbeatOutageIsLoggedOnce(t *testing.T) {
	// Not parallel: it replaces the default slog logger.
	var slogOut lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&slogOut, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var gormOut lockedBuffer
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.New(log.New(&gormOut, "", 0), logger.Config{LogLevel: logger.Info, SlowThreshold: time.Hour}),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	// The table does not exist: the connection works, the transaction opens
	// and every write statement itself fails (a closed pool would fail
	// before the statement and GORM logs nothing then).

	hb := NewHeartbeat(database.NewGORMReplicaHeartbeatRepository(db), "replica-1", 10*time.Millisecond)
	hb.Start()
	time.Sleep(120 * time.Millisecond) // about a dozen failed writes
	hb.Stop(time.Second)

	assert.Equal(t, 1, strings.Count(slogOut.String(), "Replica heartbeat write failed"), "the outage is logged once by the heartbeat")
	assert.Empty(t, gormOut.String(), "the GORM logger writes nothing for the heartbeat statements")
}

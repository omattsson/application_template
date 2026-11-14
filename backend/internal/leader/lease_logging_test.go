package leader

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestLeaseOutageIsLoggedOnce: while the database is down every campaign
// attempt fails. The lease logs the outage once (its own line); the GORM
// logger of the application database must not add an Error line per attempt.
func TestLeaseOutageIsLoggedOnce(t *testing.T) {
	// Not parallel: it replaces the default slog logger.
	var slogOut lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&slogOut, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A GORM logger that writes every statement and error it sees.
	var gormOut lockedBuffer
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.New(log.New(&gormOut, "", 0), logger.Config{LogLevel: logger.Info, SlowThreshold: time.Hour}),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close()) // every statement fails

	l := NewLease(db, "workers", "a", testLeaseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx, func(context.Context) { t.Error("onStarted without the lease") }, func() {})
	}()
	time.Sleep(6 * testLeaseConfig.RetryPeriod) // about six failed attempts
	cancel()
	<-done

	assert.Equal(t, 1, strings.Count(slogOut.String(), "campaign failed"), "the outage is logged once by the lease")
	assert.Empty(t, gormOut.String(), "the GORM logger writes nothing for the lease statements")
}

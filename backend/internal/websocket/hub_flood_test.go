package websocket

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type syncLog struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestBroadcastDropDoesNotFloodTheLog: with the broadcast channel full every
// Broadcast drops its message. One Warn line goes out per interval; the other
// drops are Debug lines, and the drop counter counts every one.
// Not parallel: it replaces the default logger and the global MeterProvider.
func TestBroadcastDropDoesNotFloodTheLog(t *testing.T) {
	var buf syncLog
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		slog.SetDefault(prev)
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(noopmetric.NewMeterProvider())
	})

	hub := NewHub() // Run is not started: nothing drains the channel
	for i := 0; i < broadcastBufferSize; i++ {
		hub.Broadcast([]byte("fill"))
	}
	require.Empty(t, buf.String(), "the buffer takes broadcastBufferSize messages")

	const dropped = 100
	for i := 0; i < dropped; i++ {
		hub.Broadcast([]byte("dropped"))
	}

	var warn, debug int
	for _, l := range strings.Split(buf.String(), "\n") {
		switch {
		case strings.Contains(l, "level=WARN") && strings.Contains(l, "broadcast channel full"):
			warn++
		case strings.Contains(l, "level=DEBUG") && strings.Contains(l, "broadcast channel full"):
			debug++
		}
	}
	assert.Equal(t, 1, warn, "one Warn line, not one per dropped message")
	assert.Equal(t, dropped-1, debug)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "ws.broadcast.dropped.total" {
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					total += dp.Value
				}
			}
		}
	}
	assert.EqualValues(t, dropped, total, "every drop is counted")
}

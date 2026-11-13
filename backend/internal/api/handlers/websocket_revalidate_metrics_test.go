package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The tests of this file install a process-wide MeterProvider and the default
// slog logger: none of them runs in parallel. The checker reads the provider
// when it is built, so each test installs its provider first.

func installMeterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(noopmetric.NewMeterProvider())
	})
	return reader
}

// counterTotals returns the sum of the counter per value of attribute attr
// ("" sums everything under the key "").
func counterTotals(t *testing.T, reader *sdkmetric.ManualReader, name, attr string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				key := ""
				if attr != "" {
					v, _ := dp.Attributes.Value(attribute.Key(attr))
					key = v.AsString()
				}
				out[key] += dp.Value
			}
		}
	}
	return out
}

type lockedLog struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureDefaultLog(t *testing.T) *lockedLog {
	t.Helper()
	var buf lockedLog
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestRevocationCheckerCountsEscalationCloses: the identities that the
// fail-closed escalation closes after M unverified runs in a row are counted
// (WebSocket sockets closed for that reason are otherwise only visible in a
// rate-limited log line).
func TestRevocationCheckerCountsEscalationCloses(t *testing.T) {
	const m = defaultMaxUnverifiedRuns
	const sockets = 3

	t.Run("fail closed counts the identities closed in the M-th run", func(t *testing.T) {
		reader := installMeterReader(t)
		store := newCountingStore(t)
		store.budget = 0 // the store is too slow: the run reaches no identity
		check := newRevocationChecker(store, newFakeUsers(100), nil,
			checkerConfig{failClosed: true, lookupTimeout: time.Hour, maxUnverifiedRuns: m}).check
		ids := jwtIdentities(sockets)

		for run := 1; run < m; run++ {
			assert.Equal(t, make([]bool, sockets), runOnce(check, store, ids))
			assert.Empty(t, counterTotals(t, reader, "ws.revalidation.escalation.total", ""), "run %d: nothing closed yet", run)
		}
		closed := runOnce(check, store, ids)
		assert.Equal(t, []bool{true, true, true}, closed)
		assert.Equal(t, map[string]int64{"": sockets}, counterTotals(t, reader, "ws.revalidation.escalation.total", ""))
	})

	t.Run("fail open never escalates", func(t *testing.T) {
		reader := installMeterReader(t)
		store := newCountingStore(t)
		store.budget = 0
		check := newRevocationChecker(store, newFakeUsers(100), nil,
			checkerConfig{lookupTimeout: time.Hour, maxUnverifiedRuns: m}).check
		for run := 1; run <= 2*m; run++ {
			runOnce(check, store, jwtIdentities(sockets))
		}
		assert.Empty(t, counterTotals(t, reader, "ws.revalidation.escalation.total", ""))
	})

	t.Run("a healthy run closes and counts nothing", func(t *testing.T) {
		reader := installMeterReader(t)
		store := newCountingStore(t)
		check := newRevocationChecker(store, newFakeUsers(100), nil,
			checkerConfig{failClosed: true, lookupTimeout: time.Hour, maxUnverifiedRuns: m}).check
		for run := 1; run <= 2*m; run++ {
			assert.Equal(t, make([]bool, sockets), runOnce(check, store, jwtIdentities(sockets)))
		}
		assert.Empty(t, counterTotals(t, reader, "ws.revalidation.escalation.total", ""))
	})
}

// TestRevocationCheckerRepeatedFailureIsCountedButLoggedOnce: a store that
// stays down fails the same lookup in every run. The log line is rate limited
// (once a minute); the failure counter counts every failure, so the flood
// protection does not hide the outage from the metrics.
func TestRevocationCheckerRepeatedFailureIsCountedButLoggedOnce(t *testing.T) {
	reader := installMeterReader(t)
	buf := captureDefaultLog(t)
	store := newCountingStore(t)
	store.failErr = errors.New("dial tcp: store down")
	check := newRevocationChecker(store, newFakeUsers(100), nil,
		checkerConfig{failClosed: true, lookupTimeout: time.Hour}).check

	const runs = 5
	for run := 0; run < runs; run++ {
		runOnce(check, store, jwtIdentities(2))
	}

	assert.Equal(t, map[string]int64{"token blocklist check": runs}, counterTotals(t, reader, "ws.revalidation.failure.total", "lookup"))
	var errLines int
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "level=ERROR") && strings.Contains(l, "token blocklist check failed") {
			errLines++
		}
	}
	assert.Equal(t, 1, errLines, "one Error line in %d failing runs; log:\n%s", runs, buf.String())
}

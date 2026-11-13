package telemetry_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"backend/internal/config"
	"backend/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartDBMetrics_RegistersPoolInstruments(t *testing.T) {
	m := newMemory(t, config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 1})

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(7)
	conn, err := db.Conn(context.Background()) // one connection in use
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, telemetry.StartDBMetrics(db))
	rm := m.collect(t)

	gauges := map[string]int64{
		"db.pool.max_open": 7,
		"db.pool.open":     1,
		"db.pool.in_use":   1,
		"db.pool.idle":     0,
	}
	for name, want := range gauges {
		mt, ok := findMetric(rm, name)
		require.True(t, ok, "%s is registered", name)
		g, ok := mt.Data.(metricdata.Gauge[int64])
		require.True(t, ok, "%s is a gauge", name)
		require.Len(t, g.DataPoints, 1, name)
		assert.Equal(t, want, g.DataPoints[0].Value, name)
	}
	for _, name := range []string{"db.pool.wait_count", "db.pool.max_idle_closed", "db.pool.max_lifetime_closed"} {
		mt, ok := findMetric(rm, name)
		require.True(t, ok, "%s is registered", name)
		_, isSum := mt.Data.(metricdata.Sum[int64])
		assert.True(t, isSum, "%s is a counter", name)
	}
	wait, ok := findMetric(rm, "db.pool.wait_duration_ms")
	require.True(t, ok)
	_, isSum := wait.Data.(metricdata.Sum[float64])
	assert.True(t, isSum, "wait duration is a float counter")

	// A later collection sees the pool change: the callback reads the live
	// stats.
	require.NoError(t, conn.Close())
	mt, ok := findMetric(m.collect(t), "db.pool.in_use")
	require.True(t, ok)
	assert.EqualValues(t, 0, mt.Data.(metricdata.Gauge[int64]).DataPoints[0].Value)
}

// failingMeter fails the creation of the instrument named failName (or the
// callback registration when failName is "callback"), as a broken provider
// would.
type failingMeter struct {
	noopmetric.Meter
	failName string
}

var errInstrument = errors.New("instrument refused")

func (m failingMeter) Int64ObservableGauge(name string, opts ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	if name == m.failName {
		return nil, errInstrument
	}
	return m.Meter.Int64ObservableGauge(name, opts...)
}

func (m failingMeter) Int64ObservableCounter(name string, opts ...metric.Int64ObservableCounterOption) (metric.Int64ObservableCounter, error) {
	if name == m.failName {
		return nil, errInstrument
	}
	return m.Meter.Int64ObservableCounter(name, opts...)
}

func (m failingMeter) Float64ObservableCounter(name string, opts ...metric.Float64ObservableCounterOption) (metric.Float64ObservableCounter, error) {
	if name == m.failName {
		return nil, errInstrument
	}
	return m.Meter.Float64ObservableCounter(name, opts...)
}

func (m failingMeter) RegisterCallback(f metric.Callback, instruments ...metric.Observable) (metric.Registration, error) {
	if m.failName == "callback" {
		return nil, errInstrument
	}
	return m.Meter.RegisterCallback(f, instruments...)
}

type failingProvider struct {
	noopmetric.MeterProvider
	failName string
}

func (p failingProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return failingMeter{failName: p.failName}
}

// TestStartDBMetrics_ReportsAnInstrumentThatCannotBeCreated: a start error is
// returned (the bootstrap logs it and goes on without pool metrics).
func TestStartDBMetrics_ReportsAnInstrumentThatCannotBeCreated(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	names := []string{
		"db.pool.max_open", "db.pool.open", "db.pool.in_use", "db.pool.idle", "db.pool.wait_count",
		"db.pool.wait_duration_ms", "db.pool.max_idle_closed", "db.pool.max_lifetime_closed", "callback",
	}
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			otel.SetMeterProvider(failingProvider{failName: name})
			t.Cleanup(func() { otel.SetMeterProvider(noopmetric.NewMeterProvider()) })

			assert.ErrorIs(t, telemetry.StartDBMetrics(db), errInstrument)
		})
	}
}

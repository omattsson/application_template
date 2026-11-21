package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestMetricsRecordWork(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := newMetrics(provider.Meter("jobs"))
	require.NoError(t, err)
	t.Cleanup(m.Close)

	m.Processed("mail", OutcomeSucceeded)
	m.Processed("mail", OutcomeSucceeded)
	m.Processed("mail", OutcomeRetried)
	m.Processed("report", OutcomeDead)
	m.Processed("report", OutcomeReleased)
	m.Duration("mail", 1500*time.Millisecond)
	m.Duration("mail", 500*time.Millisecond)
	m.ObserveStats(models.JobStats{Pending: 4, Running: 2, Succeeded: 9, Dead: 1, Due: 3, OldestDueAge: 90 * time.Second})

	got := collect(t, reader)

	processed, ok := got["jobs.processed"].Data.(metricdata.Sum[int64])
	require.True(t, ok)
	counts := map[string]int64{}
	for _, dp := range processed.DataPoints {
		require.Equal(t, 2, dp.Attributes.Len(), "type and outcome, nothing else")
		typ, _ := dp.Attributes.Value("type")
		outcome, _ := dp.Attributes.Value("outcome")
		counts[typ.AsString()+"/"+outcome.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{"mail/succeeded": 2, "mail/retried": 1, "report/dead": 1, "report/released": 1}, counts)

	hist, ok := got["jobs.duration"].Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 1)
	assert.Equal(t, uint64(2), hist.DataPoints[0].Count)
	assert.InDelta(t, 2.0, hist.DataPoints[0].Sum, 0.001)

	depth, ok := got["jobs.depth"].Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	byStatus := map[string]int64{}
	for _, dp := range depth.DataPoints {
		a := dp.Attributes.ToSlice()
		require.Len(t, a, 1)
		byStatus[a[0].Value.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{"pending": 4, "running": 2, "succeeded": 9, "dead": 1}, byStatus)

	age, ok := got["jobs.oldest_due.age"].Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	assert.InDelta(t, 90.0, age.DataPoints[0].Value, 0.001)
}

func TestNilMetricsDoNothing(t *testing.T) {
	t.Parallel()
	var m *Metrics
	assert.NotPanics(t, func() {
		m.Processed("a", OutcomeDead)
		m.Duration("a", time.Second)
		m.ObserveStats(models.JobStats{})
		m.Close()
	})
}

// Not parallel: it replaces the global MeterProvider for its duration.
func TestNewMetricsFollowsTheGlobalProvider(t *testing.T) {
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	otel.SetMeterProvider(noopmetric.NewMeterProvider())
	assert.Nil(t, NewMetrics(), "nothing is allocated with telemetry off")

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	otel.SetMeterProvider(provider)
	m := NewMetrics()
	require.NotNil(t, m)
	m.Close()
}

// failingMeter fails the n-th instrument it is asked for (1 counter,
// 2 histogram, 3 depth gauge, 4 age gauge, 5 the callback registration).
type failingMeter struct {
	metric.Meter
	failAt int
	calls  int
}

var errMeter = errors.New("meter failure")

func (f *failingMeter) step() error {
	f.calls++
	if f.calls == f.failAt {
		return errMeter
	}
	return nil
}

func (f *failingMeter) Int64Counter(name string, o ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	if err := f.step(); err != nil {
		return nil, err
	}
	return f.Meter.Int64Counter(name, o...)
}

func (f *failingMeter) Float64Histogram(name string, o ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	if err := f.step(); err != nil {
		return nil, err
	}
	return f.Meter.Float64Histogram(name, o...)
}

func (f *failingMeter) Int64ObservableGauge(name string, o ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	if err := f.step(); err != nil {
		return nil, err
	}
	return f.Meter.Int64ObservableGauge(name, o...)
}

func (f *failingMeter) Float64ObservableGauge(name string, o ...metric.Float64ObservableGaugeOption) (metric.Float64ObservableGauge, error) {
	if err := f.step(); err != nil {
		return nil, err
	}
	return f.Meter.Float64ObservableGauge(name, o...)
}

func (f *failingMeter) RegisterCallback(cb metric.Callback, i ...metric.Observable) (metric.Registration, error) {
	if err := f.step(); err != nil {
		return nil, err
	}
	return f.Meter.RegisterCallback(cb, i...)
}

// A failure to create any instrument is returned, not swallowed into a
// half-built Metrics.
func TestNewMetricsReturnsInstrumentErrors(t *testing.T) {
	t.Parallel()
	for failAt := 1; failAt <= 5; failAt++ {
		failAt := failAt
		t.Run(fmt.Sprintf("step %d", failAt), func(t *testing.T) {
			t.Parallel()
			provider := sdkmetric.NewMeterProvider()
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			m, err := newMetrics(&failingMeter{Meter: provider.Meter("jobs"), failAt: failAt})
			require.ErrorIs(t, err, errMeter)
			assert.Nil(t, m)
		})
	}
}

// Metrics.Close is safe on nil and without a registration.
func TestMetricsCloseIsSafe(t *testing.T) {
	t.Parallel()
	var nilMetrics *Metrics
	assert.NotPanics(t, nilMetrics.Close)
	assert.NotPanics(t, (&Metrics{}).Close)
}

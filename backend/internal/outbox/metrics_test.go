package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collect(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Metrics {
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

func TestMetricsRecordDeliveryAndLag(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	m, err := newMetrics(provider.Meter("outbox"))
	require.NoError(t, err)
	t.Cleanup(m.Close)

	m.Observe(Stats{Delivered: 3, Age: 1500 * time.Millisecond, HasAge: true, IDLag: 7, HasLag: true})
	m.Observe(Stats{Delivered: 2, HasLag: true})
	m.Observe(Stats{Delivered: 1}) // the lag is unknown: the gauge keeps its value

	got := collect(t, reader)

	delivered, ok := got["outbox.events.delivered"].Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, delivered.DataPoints, 1)
	assert.Equal(t, int64(6), delivered.DataPoints[0].Value)
	assert.Empty(t, delivered.DataPoints[0].Attributes.ToSlice(), "no label")

	age, ok := got["outbox.delivery.age"].Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	assert.InDelta(t, 1.5, age.DataPoints[0].Value, 0.001, "an idle poll keeps the age of the last delivery")

	lag, ok := got["outbox.cursor.lag"].Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	assert.Equal(t, int64(0), lag.DataPoints[0].Value, "the last poll reached the end")
}

// Not parallel: it replaces the global MeterProvider for its duration.
func TestNewMetricsFollowsTheGlobalProvider(t *testing.T) {
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	otel.SetMeterProvider(noopmetric.NewMeterProvider())
	assert.Nil(t, NewMetrics(), "nothing is allocated with telemetry off")

	provider := metric.NewMeterProvider(metric.WithReader(metric.NewManualReader()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	otel.SetMeterProvider(provider)
	m := NewMetrics()
	require.NotNil(t, m)
	m.Close()
}

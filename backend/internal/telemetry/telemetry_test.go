package telemetry_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file set the process-wide OTel providers. None of them
// runs in parallel.

func TestInit_DisabledReturnsUsableNoop(t *testing.T) {
	tel, err := telemetry.Init(config.OtelConfig{Enabled: false})
	require.NoError(t, err)
	require.NotNil(t, tel)

	assert.Nil(t, tel.TracerProvider)
	assert.Nil(t, tel.MeterProvider)
	assert.Nil(t, tel.LoggerProvider)
	assert.Nil(t, tel.MetricsHandler)

	// The global providers are no-ops: spans are not recording, nothing is
	// kept.
	_, span := otel.Tracer("test").Start(context.Background(), "ignored")
	assert.False(t, span.IsRecording())
	span.End()
	_, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	assert.False(t, isSDK, "no SDK tracer provider when disabled")
	_, isNoopMeter := otel.GetMeterProvider().(noop.MeterProvider)
	assert.True(t, isNoopMeter, "the meter provider is the no-op one, so HTTPMetrics short-circuits")

	// Every operation on the disabled instance works.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	assert.NoError(t, telemetry.StartDBMetrics(db))
	assert.NoError(t, tel.Shutdown(context.Background()))
	assert.NoError(t, tel.Shutdown(context.Background()), "a second Shutdown is harmless")
}

func TestTelemetry_ShutdownOnNilAndZeroValue(t *testing.T) {
	var nilTel *telemetry.Telemetry
	assert.NoError(t, nilTel.Shutdown(context.Background()))
	assert.NoError(t, (&telemetry.Telemetry{}).Shutdown(context.Background()))
}

func TestInitWithBackends_RecordsSpansMetricsAndResource(t *testing.T) {
	m := newMemory(t, config.OtelConfig{Enabled: true, ServiceName: "svc-under-test", SampleRate: 1})

	ctx, span := otel.Tracer("test").Start(context.Background(), "the-span")
	counter, err := otel.Meter("test").Int64Counter("test.counter")
	require.NoError(t, err)
	counter.Add(ctx, 3, metric.WithAttributes(attribute.String("k", "v")))
	span.End()

	spans := m.spans.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "the-span", spans[0].Name)
	assert.Contains(t, spans[0].Resource.Attributes(), attribute.String("service.name", "svc-under-test"))

	mt, ok := findMetric(m.collect(t), "test.counter")
	require.True(t, ok, "the counter is exported")
	sum, ok := mt.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 1)
	assert.EqualValues(t, 3, sum.DataPoints[0].Value)
}

func TestInitWithBackends_SampleRate(t *testing.T) {
	tests := []struct {
		name      string
		rate      float64
		wantSpans int
	}{
		{name: "rate 1 samples everything", rate: 1, wantSpans: 1},
		{name: "rate 0 samples nothing", rate: 0, wantSpans: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			m := newMemory(t, config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: tt.rate})
			_, span := otel.Tracer("test").Start(context.Background(), "root")
			span.End()
			assert.Len(t, m.spans.GetSpans(), tt.wantSpans)
		})
	}
}

// TestInitWithBackends_HonoursTheParentSamplingDecision: a request that
// arrives with a sampled parent is traced even with a low local rate, and one
// with an unsampled parent is not (ParentBased).
func TestInitWithBackends_HonoursTheParentSamplingDecision(t *testing.T) {
	m := newMemory(t, config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 0})

	_, root := otel.Tracer("test").Start(context.Background(), "root")
	root.End()
	require.Empty(t, m.spans.GetSpans(), "the root span is not sampled at rate 0")

	sampled := sampledParentContext(t)
	_, child := otel.Tracer("test").Start(sampled, "child")
	child.End()
	assert.Len(t, m.spans.GetSpans(), 1, "a sampled parent wins over the local rate")
}

func TestInitWithBackends_ShutdownFlushesPendingSpans(t *testing.T) {
	mem := &keepingExporter{}
	tel, err := telemetry.InitWithBackends(config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 1}, telemetry.Backends{
		// A batch that never fills and a timeout of an hour: only Shutdown
		// can deliver the span.
		Spans: sdktrace.NewBatchSpanProcessor(mem, sdktrace.WithBatchTimeout(time.Hour)),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = telemetry.Init(config.OtelConfig{}) })

	_, span := otel.Tracer("test").Start(context.Background(), "pending")
	span.End()
	require.Empty(t, mem.names(), "the batcher holds the span")

	require.NoError(t, tel.Shutdown(context.Background()))
	require.Equal(t, []string{"pending"}, mem.names(), "Shutdown flushed the span")
	assert.NoError(t, tel.Shutdown(context.Background()), "a second Shutdown is harmless")
}

func TestInit_EnabledWithOTLPExportersStartsAndStops(t *testing.T) {
	// The exporters dial lazily: no collector is needed to start. Shutdown
	// must return within its context even though nothing listens.
	tel, err := telemetry.Init(config.OtelConfig{
		Enabled: true, MetricsEnabled: true, Endpoint: "127.0.0.1:1", ServiceName: "otlp-test", SampleRate: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, tel.TracerProvider)
	require.NotNil(t, tel.MeterProvider)
	require.NotNil(t, tel.LoggerProvider)
	require.NotNil(t, tel.MetricsHandler)
	t.Cleanup(func() { _, _ = telemetry.Init(config.OtelConfig{}) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_ = tel.Shutdown(ctx) // an export error to the missing collector is expected
	assert.Less(t, time.Since(start), 10*time.Second, "Shutdown is bounded by its context")
}

func TestInit_MetricsOnlyServesPrometheus(t *testing.T) {
	tel, err := telemetry.Init(config.OtelConfig{MetricsEnabled: true, ServiceName: "prom-test"})
	require.NoError(t, err)
	require.NotNil(t, tel.MetricsHandler)
	assert.Nil(t, tel.TracerProvider, "no tracing without OTEL_ENABLED")
	assert.Nil(t, tel.LoggerProvider, "no OTLP logs without OTEL_ENABLED")
	require.NotNil(t, tel.MeterProvider)

	counter, err := otel.Meter("prom").Int64Counter("prom.test.events")
	require.NoError(t, err)
	counter.Add(context.Background(), 2)

	rec := httptest.NewRecorder()
	tel.MetricsHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "prom_test_events_total", "the OTel counter is served")
	assert.Contains(t, string(body), "go_goroutines", "the Go runtime collector is served")

	// A second Init in the same process must not collide on a shared registry.
	tel2, err := telemetry.Init(config.OtelConfig{MetricsEnabled: true, ServiceName: "prom-test"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.NoError(t, tel2.Shutdown(ctx))
	assert.NoError(t, tel.Shutdown(ctx))
	_, _ = telemetry.Init(config.OtelConfig{})
}

func TestInit_FailsTheStartOnABadConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.OtelConfig
	}{
		{name: "sample rate above 1", cfg: config.OtelConfig{Enabled: true, Endpoint: "c:4317", ServiceName: "s", SampleRate: 7}},
		{name: "endpoint with a scheme", cfg: config.OtelConfig{Enabled: true, Endpoint: "http://c:4317", ServiceName: "s", SampleRate: 1}},
		{name: "no service name", cfg: config.OtelConfig{Enabled: true, Endpoint: "c:4317", SampleRate: 1}},
		// A host that no gRPC target can be built from: the exporter refuses it.
		{name: "exporter refuses the address", cfg: config.OtelConfig{Enabled: true, Endpoint: "%zz:4317", ServiceName: "s", SampleRate: 1}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			tel, err := telemetry.Init(tt.cfg)
			require.Error(t, err)
			assert.Nil(t, tel)
		})
	}
}

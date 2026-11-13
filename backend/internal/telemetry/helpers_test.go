package telemetry_test

import (
	"context"
	"sync"
	"testing"

	"backend/internal/config"
	"backend/internal/telemetry"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/stretchr/testify/require"
)

// memLogExporter keeps the exported OTel log records in memory.
type memLogExporter struct {
	records []sdklog.Record
	mu      sync.Mutex
}

func (e *memLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}

func (e *memLogExporter) Shutdown(context.Context) error   { return nil }
func (e *memLogExporter) ForceFlush(context.Context) error { return nil }

func (e *memLogExporter) bodies() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.records))
	for _, r := range e.records {
		out = append(out, r.Body().AsString())
	}
	return out
}

func (e *memLogExporter) find(body string) (sdklog.Record, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.records {
		if r.Body().AsString() == body {
			return r.Clone(), true
		}
	}
	return sdklog.Record{}, false
}

// memory is a Telemetry with in-memory backends: spans and log records are
// exported synchronously, metrics are read with reader.Collect.
type memory struct {
	tel    *telemetry.Telemetry
	spans  *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
	logs   *memLogExporter
}

// newMemory installs an enabled Telemetry on in-memory backends as the global
// providers. It restores no-op providers afterwards, so a test that uses it
// must not run in parallel.
func newMemory(t *testing.T, cfg config.OtelConfig) *memory {
	t.Helper()
	m := &memory{
		spans:  tracetest.NewInMemoryExporter(),
		reader: sdkmetric.NewManualReader(),
		logs:   &memLogExporter{},
	}
	tel, err := telemetry.InitWithBackends(cfg, telemetry.Backends{
		Spans:   sdktrace.NewSimpleSpanProcessor(m.spans),
		Metrics: m.reader,
		Logs:    sdklog.NewSimpleProcessor(m.logs),
	})
	require.NoError(t, err)
	m.tel = tel
	t.Cleanup(func() {
		require.NoError(t, tel.Shutdown(context.Background()))
		_, err := telemetry.Init(config.OtelConfig{}) // no-op providers again
		require.NoError(t, err)
	})
	return m
}

func (m *memory) collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, m.reader.Collect(context.Background(), &rm))
	return rm
}

// findMetric returns the metric of that name from any scope.
func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name == name {
				return mt, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// sampledParentContext returns a context with a remote, sampled span context:
// a request that arrives with a traceparent header that has the sampled flag.
func sampledParentContext(t *testing.T) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	require.NoError(t, err)
	return trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
}

// keepingExporter keeps every exported span after Shutdown. The exporter of
// tracetest drops its spans when it is shut down, which would hide what a
// Shutdown flushed.
type keepingExporter struct {
	spans []sdktrace.ReadOnlySpan
	mu    sync.Mutex
}

func (e *keepingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *keepingExporter) Shutdown(context.Context) error { return nil }

func (e *keepingExporter) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.spans))
	for _, s := range e.spans {
		out = append(out, s.Name())
	}
	return out
}

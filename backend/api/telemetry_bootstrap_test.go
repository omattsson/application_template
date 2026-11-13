package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/telemetry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// otelLogSink keeps the exported OTel log records.
type otelLogSink struct {
	records []sdklog.Record
	mu      sync.Mutex
}

func (s *otelLogSink) Export(_ context.Context, records []sdklog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		s.records = append(s.records, r.Clone())
	}
	return nil
}

func (s *otelLogSink) Shutdown(context.Context) error   { return nil }
func (s *otelLogSink) ForceFlush(context.Context) error { return nil }

func (s *otelLogSink) count(body string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.records {
		if strings.Contains(r.Body().AsString(), body) {
			n++
		}
	}
	return n
}

func (s *otelLogSink) find(body string) (sdklog.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if strings.Contains(r.Body().AsString(), body) {
			return r.Clone(), true
		}
	}
	return sdklog.Record{}, false
}

// keepingExporter keeps the exported spans after Shutdown (the in-memory
// exporter of tracetest drops them).
type keepingExporter struct {
	spans []string
	mu    sync.Mutex
}

func (e *keepingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range spans {
		e.spans = append(e.spans, s.Name())
	}
	return nil
}

func (e *keepingExporter) Shutdown(context.Context) error { return nil }

func (e *keepingExporter) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.spans...)
}

// lockedBuffer is a bytes.Buffer that is safe for concurrent writes.
type lockedBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
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

// withLogTelemetry returns a Telemetry with an in-memory OTel log sink and
// restores the default loggers when the test ends.
func withLogTelemetry(t *testing.T) (*telemetry.Telemetry, *otelLogSink) {
	t.Helper()
	sink := &otelLogSink{}
	tel, err := telemetry.InitWithBackends(config.OtelConfig{Enabled: true, ServiceName: "log-test", SampleRate: 1}, telemetry.Backends{
		Spans: sdktrace.NewSimpleSpanProcessor(tracetest.NewInMemoryExporter()),
		Logs:  sdklog.NewSimpleProcessor(sink),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, tel.Shutdown(context.Background()))
		_, _ = telemetry.Init(config.OtelConfig{})
	})
	return tel, sink
}

func restoreLogging(t *testing.T) {
	t.Helper()
	prevDefault, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prevDefault)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
}

// runWithDeadline runs fn and fails the test when it blocks.
func runWithDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked", what)
	}
}

// TestSetupOTelLogging changes the process-wide default logger, so it does
// not run in parallel.
func TestSetupOTelLogging(t *testing.T) {
	restoreLogging(t)

	t.Run("without an OTel log pipeline the default logger stays", func(t *testing.T) {
		before := slog.Default()
		setupOTelLogging(&telemetry.Telemetry{}, slog.LevelDebug)
		setupOTelLogging(nil, slog.LevelDebug)
		assert.Same(t, before, slog.Default(), "OTEL_ENABLED=false changes nothing")
	})

	t.Run("with a pipeline the logs go to both, at LOG_LEVEL", func(t *testing.T) {
		tel, sink := withLogTelemetry(t)
		var local lockedBuffer
		setupOTelLoggingTo(tel, slog.LevelWarn, &local)

		slog.Warn("warn record", "k", "v")
		slog.Info("info record below the level")
		slog.Error("error record")

		assert.Contains(t, local.String(), `msg="warn record" k=v`)
		assert.Equal(t, 1, sink.count("warn record"))
		assert.Equal(t, 1, sink.count("error record"))
		assert.NotContains(t, local.String(), "info record below the level")
		assert.Zero(t, sink.count("info record below the level"), "LOG_LEVEL applies to OTel too")
	})
}

// TestSetupOTelLoggingNoDeadlock: the bridge never wraps the handler that was
// the default before. The package default handler of log/slog writes through
// the log package, which writes through the default slog logger again, so
// wrapping it would deadlock on the first log.Print. Installing the pipeline
// twice (main does it once, a test or a reload may repeat it) must neither
// recurse nor chain the handlers.
func TestSetupOTelLoggingNoDeadlock(t *testing.T) {
	restoreLogging(t)

	t.Run("log package records go through the new default logger", func(t *testing.T) {
		tel, sink := withLogTelemetry(t)
		var local lockedBuffer
		setupOTelLoggingTo(tel, slog.LevelInfo, &local)

		runWithDeadline(t, "log.Print through the new default logger", func() {
			log.Print("log package record")
			slog.Info("slog record")
		})

		assert.Contains(t, local.String(), "log package record")
		assert.Equal(t, 1, sink.count("log package record"), "OTel gets each record once")
		assert.Equal(t, 1, sink.count("slog record"))
	})

	t.Run("setting it up twice does not recurse and does not chain", func(t *testing.T) {
		tel, sink := withLogTelemetry(t)
		var first, second lockedBuffer
		setupOTelLoggingTo(tel, slog.LevelInfo, &first)
		setupOTelLoggingTo(tel, slog.LevelInfo, &second) // replaces, does not wrap

		runWithDeadline(t, "logging after a second setup", func() {
			log.Print("record one")
			slog.Info("record two")
		})

		assert.Contains(t, second.String(), "record one")
		assert.NotContains(t, first.String(), "record", "the first handler is no longer in the chain")
		assert.Equal(t, 1, sink.count("record one"), "no duplicate OTel records from a chained bridge")
		assert.Equal(t, 1, sink.count("record two"))
	})
}

// TestLogRecordsCarryTraceAndSpanID: a log call with the context of a span
// exports a record with that span's trace and span ID.
func TestLogRecordsCarryTraceAndSpanID(t *testing.T) {
	restoreLogging(t)
	tel, sink := withLogTelemetry(t)
	var local lockedBuffer
	setupOTelLoggingTo(tel, slog.LevelInfo, &local)

	ctx, span := otel.Tracer("test").Start(context.Background(), "request")
	slog.InfoContext(ctx, "handled", "k", "v")
	span.End()

	rec, ok := sink.find("handled")
	require.True(t, ok)
	assert.Equal(t, span.SpanContext().TraceID(), rec.TraceID())
	assert.Equal(t, span.SpanContext().SpanID(), rec.SpanID())
	assert.True(t, trace.SpanContextFromContext(ctx).IsSampled())
}

// TestInitTelemetryDisabledChangesNothing: with both switches off the
// bootstrap leaves the logger and the providers as they are.
func TestInitTelemetryDisabledChangesNothing(t *testing.T) {
	restoreLogging(t)
	before := slog.Default()

	tel, err := initTelemetry(&config.Config{})
	require.NoError(t, err)
	require.NotNil(t, tel)
	assert.Same(t, before, slog.Default())
	assert.NoError(t, tel.Shutdown(context.Background()))
}

func TestInitTelemetryReturnsTheInitError(t *testing.T) {
	// LoadConfig rejects a sample rate outside [0, 1], and so does Init.
	_, err := initTelemetry(&config.Config{Otel: config.OtelConfig{Enabled: true, SampleRate: 7, ServiceName: "x", Endpoint: "localhost:4317"}})
	assert.Error(t, err)
}

// TestShutdownFlushesTelemetry: the telemetry hook of the shutdown flushes
// what the providers still hold, after the services stopped.
func TestShutdownFlushesTelemetry(t *testing.T) {
	spans := &keepingExporter{}
	tel, err := telemetry.InitWithBackends(config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 1}, telemetry.Backends{
		Spans: sdktrace.NewBatchSpanProcessor(spans, sdktrace.WithBatchTimeout(time.Hour)),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = telemetry.Init(config.OtelConfig{}) })

	_, span := otel.Tracer("test").Start(context.Background(), "last request")
	span.End()
	require.Empty(t, spans.names(), "the batcher still holds the span")

	rec := &callRecorder{}
	s := fakeServices(rec)
	attachTelemetry(s, tel)

	require.NoError(t, shutdown(context.Background(), s))
	assert.Equal(t, []string{"last request"}, spans.names(), "shutdown flushed the pending span")
	assert.Equal(t, []string{"server", "leader group", "in-flight", "hub", "rate limiters", "database"}, rec.list(),
		"the hook replaced the fake telemetry step and the order of the other steps is unchanged")
}

// TestAttachTelemetryNilKeepsTheNoOpHook: a nil Telemetry (init failed or not
// run) must not break shutdown.
func TestAttachTelemetryNilKeepsTheNoOpHook(t *testing.T) {
	s := &services{telemetryShutdown: func(context.Context) error { return nil }}
	attachTelemetry(s, nil)
	require.NotNil(t, s.telemetryShutdown)
	assert.NoError(t, s.telemetryShutdown(context.Background()))
}

// dbRepo is a repository that exposes its gorm handle, like
// models.GenericRepository.
type dbRepo struct {
	models.Repository
	db *gorm.DB
}

func (r *dbRepo) DB() *gorm.DB { return r.db }

func TestRegisterDBMetrics(t *testing.T) {
	newReader := func(t *testing.T) *sdkmetric.ManualReader {
		reader := sdkmetric.NewManualReader()
		_, err := telemetry.InitWithBackends(config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 1}, telemetry.Backends{Metrics: reader})
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = telemetry.Init(config.OtelConfig{}) })
		return reader
	}
	poolMetrics := func(t *testing.T, reader *sdkmetric.ManualReader) []string {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var names []string
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if strings.HasPrefix(m.Name, "db.pool.") {
					names = append(names, m.Name)
				}
			}
		}
		return names
	}

	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	t.Run("registers the pool gauges when telemetry is on", func(t *testing.T) {
		reader := newReader(t)
		registerDBMetrics(&config.Config{Otel: config.OtelConfig{Enabled: true}}, &dbRepo{db: gdb})
		assert.Contains(t, poolMetrics(t, reader), "db.pool.in_use")
		assert.Contains(t, poolMetrics(t, reader), "db.pool.max_open")
	})

	t.Run("registers them with METRICS_ENABLED alone", func(t *testing.T) {
		reader := newReader(t)
		registerDBMetrics(&config.Config{Otel: config.OtelConfig{MetricsEnabled: true}}, &dbRepo{db: gdb})
		assert.NotEmpty(t, poolMetrics(t, reader))
	})

	t.Run("registers nothing when telemetry is off", func(t *testing.T) {
		reader := newReader(t)
		registerDBMetrics(&config.Config{}, &dbRepo{db: gdb})
		assert.Empty(t, poolMetrics(t, reader))
	})

	t.Run("a repository without a database handle is skipped", func(t *testing.T) {
		reader := newReader(t)
		assert.NotPanics(t, func() {
			registerDBMetrics(&config.Config{Otel: config.OtelConfig{Enabled: true}}, &stubRepository{})
		})
		assert.Empty(t, poolMetrics(t, reader))
	})
}

package telemetry_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"backend/internal/config"
	"backend/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogHandler_WithoutLoggerProviderReturnsBase(t *testing.T) {
	t.Parallel()

	base := slog.NewTextHandler(&bytes.Buffer{}, nil)
	tests := []struct {
		tel  *telemetry.Telemetry
		name string
	}{
		{name: "nil telemetry", tel: nil},
		{name: "no logger provider (OTEL_ENABLED=false)", tel: &telemetry.Telemetry{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Same(t, base, tt.tel.LogHandler(base, slog.LevelInfo))
		})
	}
}

func TestLogHandler_SendsRecordsToBaseAndOTel(t *testing.T) {
	tests := []struct {
		name      string
		level     slog.Level
		logLevel  slog.Level
		wantOTel  bool
		wantLocal bool
	}{
		{name: "info at level info", level: slog.LevelInfo, logLevel: slog.LevelInfo, wantOTel: true, wantLocal: true},
		{name: "error at level info", level: slog.LevelInfo, logLevel: slog.LevelError, wantOTel: true, wantLocal: true},
		{name: "debug below level info", level: slog.LevelInfo, logLevel: slog.LevelDebug, wantOTel: false, wantLocal: false},
		{name: "debug at level debug", level: slog.LevelDebug, logLevel: slog.LevelDebug, wantOTel: true, wantLocal: true},
		{name: "warn below level error", level: slog.LevelError, logLevel: slog.LevelWarn, wantOTel: false, wantLocal: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			m := newMemory(t, config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 1})
			var local bytes.Buffer
			base := slog.NewTextHandler(&local, &slog.HandlerOptions{Level: tt.level})
			logger := slog.New(m.tel.LogHandler(base, tt.level))

			logger.Log(context.Background(), tt.logLevel, "the message", "key", "value")

			_, gotOTel := m.logs.find("the message")
			assert.Equal(t, tt.wantOTel, gotOTel, "OTel record; exported: %v", m.logs.bodies())
			assert.Equal(t, tt.wantLocal, strings.Contains(local.String(), "the message"), "local output: %q", local.String())
		})
	}
}

func TestLogHandler_KeepsAttributesGroupsAndTraceContext(t *testing.T) {
	m := newMemory(t, config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 1})
	var local bytes.Buffer
	logger := slog.New(m.tel.LogHandler(slog.NewTextHandler(&local, nil), slog.LevelInfo)).
		With("component", "test").
		WithGroup("req")

	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	require.NoError(t, err)
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	logger.InfoContext(ctx, "with context", "id", "42")

	rec, ok := m.logs.find("with context")
	require.True(t, ok, "exported: %v", m.logs.bodies())
	assert.Equal(t, otellog.SeverityInfo, rec.Severity())
	assert.Equal(t, traceID, rec.TraceID(), "the record carries the trace of the context")
	assert.Equal(t, spanID, rec.SpanID())

	attrs := map[string]attribute.Value{}
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	assert.Equal(t, "test", attrs["component"].AsString())
	req, ok := attrs["req"]
	require.True(t, ok, "the group is an attribute; got %v", attrs)
	require.Equal(t, attribute.MAP, req.Type())
	group := map[string]string{}
	for _, kv := range req.AsMap() {
		group[string(kv.Key)] = kv.Value.AsString()
	}
	assert.Equal(t, map[string]string{"id": "42"}, group)

	assert.Contains(t, local.String(), "component=test")
	assert.Contains(t, local.String(), "req.id=42")
}

// TestLogHandler_RecordInsideALiveSpanCarriesItsIDs: the IDs come from a real
// span of the tracer provider, the way a request handler logs.
func TestLogHandler_RecordInsideALiveSpanCarriesItsIDs(t *testing.T) {
	m := newMemory(t, config.OtelConfig{Enabled: true, ServiceName: "s", SampleRate: 1})
	logger := slog.New(m.tel.LogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil), slog.LevelInfo))

	ctx, span := otel.Tracer("test").Start(context.Background(), "request")
	logger.InfoContext(ctx, "inside the span")
	logger.Info("no context") // plain call: no span in scope
	span.End()

	inside, ok := m.logs.find("inside the span")
	require.True(t, ok)
	assert.Equal(t, span.SpanContext().TraceID(), inside.TraceID())
	assert.Equal(t, span.SpanContext().SpanID(), inside.SpanID())

	outside, ok := m.logs.find("no context")
	require.True(t, ok)
	assert.False(t, outside.TraceID().IsValid(), "a record without a span context has no trace ID")
}

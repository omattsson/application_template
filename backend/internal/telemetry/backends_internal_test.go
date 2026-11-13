package telemetry

import (
	"context"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingLogExporter struct{ shutdowns int }

func (e *countingLogExporter) Export(context.Context, []sdklog.Record) error { return nil }
func (e *countingLogExporter) Shutdown(context.Context) error                { e.shutdowns++; return nil }
func (e *countingLogExporter) ForceFlush(context.Context) error              { return nil }

// TestBackendsShutdownReleasesEveryProcessor: Init releases the backends it
// built when a later step fails, so a failed start leaves no exporter behind.
func TestBackendsShutdownReleasesEveryProcessor(t *testing.T) {
	logs := &countingLogExporter{}
	reader := sdkmetric.NewManualReader()
	b := Backends{
		Spans:   sdktrace.NewSimpleSpanProcessor(tracetest.NewInMemoryExporter()),
		Metrics: reader,
		Logs:    sdklog.NewSimpleProcessor(logs),
	}

	require.NoError(t, b.shutdown(context.Background()))

	assert.Equal(t, 1, logs.shutdowns, "the log exporter was shut down")
	assert.Error(t, reader.Collect(context.Background(), nil), "the reader refuses to collect after its shutdown")
	assert.NoError(t, Backends{}.shutdown(context.Background()), "an empty Backends has nothing to release")
}

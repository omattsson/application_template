// Package telemetry sets up OpenTelemetry: traces, metrics and logs sent to an
// OTLP collector, and a Prometheus handler for the metrics. With
// OTEL_ENABLED=false and METRICS_ENABLED=false (the defaults) Init sets no-op
// providers and builds nothing else: no exporter, no goroutine, no handler.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"backend/internal/config"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// releaseTimeout bounds the release of the backends of a start that failed.
const releaseTimeout = 5 * time.Second

// Telemetry holds the initialised OTel SDK providers. A field is nil when its
// signal is off; the zero value (what Init returns when telemetry is off) is
// usable: Shutdown does nothing and LogHandler returns the base handler.
type Telemetry struct {
	// TracerProvider is set with OTEL_ENABLED=true.
	TracerProvider *sdktrace.TracerProvider
	// MeterProvider is set with OTEL_ENABLED=true or METRICS_ENABLED=true.
	MeterProvider *sdkmetric.MeterProvider
	// LoggerProvider is set with OTEL_ENABLED=true.
	LoggerProvider *sdklog.LoggerProvider
	// MetricsHandler serves the Prometheus text format; it is set with
	// METRICS_ENABLED=true. It reads a registry of its own (the OTel
	// instruments plus the Go runtime and process collectors), not the
	// default Prometheus registry. The caller serves it on a port that is not
	// public.
	MetricsHandler http.Handler

	shutdownErr  error
	shutdownOnce sync.Once
}

// Backends are the destinations of the three signals. Init builds the OTLP
// ones; InitWithBackends takes them from the caller, so a test can use
// in-memory ones. A nil field turns that signal off.
type Backends struct {
	// Spans receives the finished spans (a batcher for OTLP).
	Spans sdktrace.SpanProcessor
	// Metrics is an extra reader of the metrics (a periodic OTLP reader). The
	// Prometheus reader of METRICS_ENABLED is added to it.
	Metrics sdkmetric.Reader
	// Logs receives the log records of the slog bridge.
	Logs sdklog.Processor
}

// Init bootstraps OpenTelemetry according to cfg and installs the providers as
// the global ones. With neither cfg.Enabled nor cfg.MetricsEnabled it installs
// no-op providers and returns an empty Telemetry.
//
// With cfg.Enabled the spans, the metrics and the logs go to the OTLP gRPC
// endpoint cfg.Endpoint without transport security. The exporters connect
// lazily: an unreachable collector does not stop the start, it only loses the
// data (the SDK reports the export errors through otel.Handle).
func Init(cfg config.OtelConfig) (*Telemetry, error) {
	if !cfg.Enabled && !cfg.MetricsEnabled {
		return InitWithBackends(cfg, Backends{})
	}

	// LoadConfig validates already; a caller that builds the config by hand
	// gets the same check (a sample rate of 7 would silently mean 1).
	if cfg.Enabled {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}

	var backends Backends
	if cfg.Enabled {
		b, err := otlpBackends(context.Background(), cfg)
		if err != nil {
			return nil, err
		}
		backends = b
	}

	tel, err := InitWithBackends(cfg, backends)
	if err != nil {
		// The backends were built but not owned by a Telemetry: release them.
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		_ = backends.shutdown(ctx)
		return nil, err
	}
	if cfg.Enabled {
		slog.Info("OpenTelemetry enabled",
			"endpoint", cfg.Endpoint,
			"service", cfg.ServiceName,
			"sample_rate", cfg.SampleRate,
		)
	}
	if cfg.MetricsEnabled {
		slog.Info("Prometheus metrics enabled")
	}
	return tel, nil
}

// InitWithBackends is Init with the destinations given by the caller. With no
// backend and cfg.MetricsEnabled false it installs no-op providers. The
// Prometheus reader is added when cfg.MetricsEnabled is true.
func InitWithBackends(cfg config.OtelConfig, b Backends) (*Telemetry, error) {
	tel := &Telemetry{}

	readers := []sdkmetric.Reader{}
	if b.Metrics != nil {
		readers = append(readers, b.Metrics)
	}
	if cfg.MetricsEnabled {
		reader, handler, err := prometheusReader()
		if err != nil {
			return nil, err
		}
		readers = append(readers, reader)
		tel.MetricsHandler = handler
	}

	if b.Spans == nil && b.Logs == nil && len(readers) == 0 {
		slog.Info("OpenTelemetry disabled, using no-op providers")
		otel.SetTracerProvider(nooptrace.NewTracerProvider())
		otel.SetMeterProvider(noopmetric.NewMeterProvider())
		return tel, nil
	}

	// Use attribute.String directly to avoid semconv schema version conflicts
	// between resource.Default() (latest) and a pinned semconv version.
	res := resource.NewWithAttributes("", attribute.String("service.name", cfg.ServiceName))

	if b.Spans != nil {
		tel.TracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(b.Spans),
			sdktrace.WithResource(res),
			// A request that arrives with a sampling decision keeps it
			// (a trace is not cut in the middle); new traces use the rate.
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))),
		)
		otel.SetTracerProvider(tel.TracerProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
	} else {
		otel.SetTracerProvider(nooptrace.NewTracerProvider())
	}

	if len(readers) > 0 {
		opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
		for _, r := range readers {
			opts = append(opts, sdkmetric.WithReader(r))
		}
		tel.MeterProvider = sdkmetric.NewMeterProvider(opts...)
		otel.SetMeterProvider(tel.MeterProvider)

		if err := otelruntime.Start(
			otelruntime.WithMeterProvider(tel.MeterProvider),
			otelruntime.WithMinimumReadMemStatsInterval(time.Second),
		); err != nil {
			_ = tel.Shutdown(context.Background())
			return nil, err
		}
	} else {
		otel.SetMeterProvider(noopmetric.NewMeterProvider())
	}

	if b.Logs != nil {
		tel.LoggerProvider = sdklog.NewLoggerProvider(
			sdklog.WithProcessor(b.Logs),
			sdklog.WithResource(res),
		)
	}

	return tel, nil
}

// prometheusReader returns the Prometheus reader of the metrics and the
// handler that serves it. It uses a registry of its own, so building two
// Telemetry values in one process (tests) cannot collide on the default
// registry. The registry has the Go runtime and process collectors, as the
// default one does.
func prometheusReader() (sdkmetric.Reader, http.Handler, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, nil, err
	}

	return exporter, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), nil
}

// otlpBackends builds the OTLP gRPC exporters of the three signals. On an
// error it releases the ones it built.
func otlpBackends(ctx context.Context, cfg config.OtelConfig) (Backends, error) {
	var b Backends

	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return Backends{}, err
	}
	b.Spans = sdktrace.NewBatchSpanProcessor(traceExporter)

	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		_ = b.shutdown(ctx)
		return Backends{}, err
	}
	b.Metrics = sdkmetric.NewPeriodicReader(metricExporter)

	logExporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		_ = b.shutdown(ctx)
		return Backends{}, err
	}
	b.Logs = sdklog.NewBatchProcessor(logExporter)

	return b, nil
}

// shutdown releases the processors and readers of b that no provider owns yet.
func (b Backends) shutdown(ctx context.Context) error {
	var errs []error
	if b.Spans != nil {
		errs = append(errs, b.Spans.Shutdown(ctx))
	}
	if b.Metrics != nil {
		errs = append(errs, b.Metrics.Shutdown(ctx))
	}
	if b.Logs != nil {
		errs = append(errs, b.Logs.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// Shutdown flushes and shuts down every provider, traces first, then metrics,
// then logs, and returns the joined errors. It returns when the data is sent
// or ctx ends, whichever is first. It is safe to call on a nil or empty
// Telemetry and to call twice (the second call does nothing and returns nil).
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}

	t.shutdownOnce.Do(func() {
		var errs []error
		if t.TracerProvider != nil {
			errs = append(errs, t.TracerProvider.Shutdown(ctx))
		}
		if t.MeterProvider != nil {
			errs = append(errs, t.MeterProvider.Shutdown(ctx))
		}
		if t.LoggerProvider != nil {
			errs = append(errs, t.LoggerProvider.Shutdown(ctx))
		}
		t.shutdownErr = errors.Join(errs...)
	})

	return t.shutdownErr
}

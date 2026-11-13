package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
)

// unmatchedRoute is the route label of a request that no route matched. The
// path of such a request is client input (a scanner can send any path), so it
// never becomes a label.
const unmatchedRoute = "<unmatched>"

// durationBuckets are the bucket bounds of the request duration, in seconds:
// 5 ms to 30 s (exports). The OpenTelemetry default buckets (0 to 10000) are
// for milliseconds: in seconds every value fell into the first bucket.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// HTTPMetrics returns a Gin middleware that records HTTP request metrics.
// Records:
//
//	http.server.request.duration (Float64Histogram, unit: "s")
//	http.server.request.total    (Int64Counter, unit: "{request}")
//
// Labels: http.request.method, http.route, http.response.status_code. The
// route label is the route pattern (c.FullPath()), "<unmatched>" for a request
// that no route matched.
//
// It reads the global MeterProvider when it is called, so register it after
// telemetry.Init. Routes.Setup registers it only with OTEL_ENABLED or
// METRICS_ENABLED; it also returns a bare pass-through when the global
// provider is the no-op one, so a disabled process pays nothing per request.
func HTTPMetrics() gin.HandlerFunc {
	mp := otel.GetMeterProvider()
	if _, ok := mp.(noopmetric.MeterProvider); ok {
		return func(c *gin.Context) { c.Next() }
	}

	meter := mp.Meter("http")

	duration, err := meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
		metric.WithDescription("Duration of HTTP server requests."),
	)
	if err != nil {
		otel.Handle(err)
		return func(c *gin.Context) { c.Next() }
	}

	total, err := meter.Int64Counter(
		"http.server.request.total",
		metric.WithUnit("{request}"),
		metric.WithDescription("Total number of HTTP server requests."),
	)
	if err != nil {
		otel.Handle(err)
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = unmatchedRoute
		}

		attrs := metric.WithAttributes(
			attribute.String("http.request.method", c.Request.Method),
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", c.Writer.Status()),
		)

		duration.Record(c.Request.Context(), time.Since(start).Seconds(), attrs)
		total.Add(c.Request.Context(), 1, attrs)
	}
}

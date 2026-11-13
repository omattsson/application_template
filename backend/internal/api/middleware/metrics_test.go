package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The tests that install a MeterProvider change a process-wide global, so
// none of them runs in parallel (parallel tests of the package wait until the
// sequential ones are done).

// installReader installs a ManualReader-backed MeterProvider as the global
// one and restores the no-op provider afterwards.
func installReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(noopmetric.NewMeterProvider())
	})
	return reader
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return rm
}

func findCounter(rm metricdata.ResourceMetrics, name string) (metricdata.Sum[int64], bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if c, ok := m.Data.(metricdata.Sum[int64]); ok {
					return c, true
				}
			}
		}
	}
	return metricdata.Sum[int64]{}, false
}

func findHistogram(rm metricdata.ResourceMetrics, name string) (metricdata.Histogram[float64], bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
					return h, true
				}
			}
		}
	}
	return metricdata.Histogram[float64]{}, false
}

func TestHTTPMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("records the request counter with method, route and status", func(t *testing.T) {
		reader := installReader(t)
		router := gin.New()
		router.Use(HTTPMetrics())
		router.GET("/items/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/42", nil))

		counter, ok := findCounter(collectMetrics(t, reader), "http.server.request.total")
		require.True(t, ok, "http.server.request.total metric not found")
		require.Len(t, counter.DataPoints, 1)
		dp := counter.DataPoints[0]
		assert.EqualValues(t, 1, dp.Value)

		method, _ := dp.Attributes.Value(attribute.Key("http.request.method"))
		route, _ := dp.Attributes.Value(attribute.Key("http.route"))
		status, _ := dp.Attributes.Value(attribute.Key("http.response.status_code"))
		assert.Equal(t, "GET", method.AsString())
		assert.Equal(t, "/items/:id", route.AsString(), "the route pattern, not the path with the ID")
		assert.EqualValues(t, 200, status.AsInt64())
	})

	t.Run("one series per status code", func(t *testing.T) {
		reader := installReader(t)
		router := gin.New()
		router.Use(HTTPMetrics())
		router.GET("/fail", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
		router.GET("/gone", func(c *gin.Context) { c.Status(499) })

		for _, p := range []string{"/fail", "/fail", "/gone"} {
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
		}

		counter, ok := findCounter(collectMetrics(t, reader), "http.server.request.total")
		require.True(t, ok)
		byStatus := map[int64]int64{}
		for _, dp := range counter.DataPoints {
			s, _ := dp.Attributes.Value(attribute.Key("http.response.status_code"))
			byStatus[s.AsInt64()] += dp.Value
		}
		assert.Equal(t, map[int64]int64{500: 2, 499: 1}, byStatus)
	})

	t.Run("uses <unmatched> for unregistered routes", func(t *testing.T) {
		reader := installReader(t)
		router := gin.New()
		router.Use(HTTPMetrics())

		// A scanner can send any path: it must not become a label.
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/another/one", nil))

		counter, ok := findCounter(collectMetrics(t, reader), "http.server.request.total")
		require.True(t, ok)
		require.Len(t, counter.DataPoints, 1, "both unmatched paths share one series")
		route, _ := counter.DataPoints[0].Attributes.Value(attribute.Key("http.route"))
		assert.Equal(t, "<unmatched>", route.AsString())
		assert.EqualValues(t, 2, counter.DataPoints[0].Value)
	})

	t.Run("records the duration histogram in seconds", func(t *testing.T) {
		reader := installReader(t)
		router := gin.New()
		router.Use(HTTPMetrics())
		router.GET("/ping", func(c *gin.Context) { c.Status(http.StatusNoContent) })

		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ping", nil))

		hist, ok := findHistogram(collectMetrics(t, reader), "http.server.request.duration")
		require.True(t, ok, "http.server.request.duration metric not found")
		require.Len(t, hist.DataPoints, 1)
		assert.EqualValues(t, 1, hist.DataPoints[0].Count)
		// Second-based buckets: a fast request lands in the first bucket
		// (<= 5 ms), not in a 0-5 s bucket of the millisecond defaults.
		assert.Equal(t, []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}, hist.DataPoints[0].Bounds)
		assert.EqualValues(t, 1, hist.DataPoints[0].BucketCounts[0], "a fast request is in the <= 5 ms bucket")
	})

	t.Run("is a pass-through that records nothing when the provider is the no-op one", func(t *testing.T) {
		otel.SetMeterProvider(noopmetric.NewMeterProvider())
		router := gin.New()
		router.Use(HTTPMetrics())
		ran := false
		router.GET("/ok", func(c *gin.Context) { ran = true; c.Status(http.StatusOK) })

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok", nil))
		assert.True(t, ran)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// TestHTTPMetricsDisabledAddsNoAllocations: with the no-op provider the
// middleware is a bare c.Next(): a request allocates (almost) as much as one
// without it. The race detector adds an allocation of its own to a call
// through one more handler, so the bound is one; the enabled middleware, for
// comparison, allocates several (the attribute sets of its two instruments).
func TestHTTPMetricsDisabledAddsNoAllocations(t *testing.T) {
	gin.SetMode(gin.TestMode)

	build := func(withMetrics bool) *gin.Engine {
		r := gin.New()
		if withMetrics {
			r.Use(HTTPMetrics())
		}
		r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		return r
	}
	measure := func(r *gin.Engine) float64 {
		req := httptest.NewRequest(http.MethodGet, "/ping", nil)
		w := httptest.NewRecorder()
		return testing.AllocsPerRun(200, func() {
			w.Body.Reset()
			r.ServeHTTP(w, req)
		})
	}

	base := measure(build(false))

	otel.SetMeterProvider(noopmetric.NewMeterProvider())
	disabled := measure(build(true))
	assert.LessOrEqual(t, disabled-base, 1.0, "no-op provider: %v allocations against %v without the middleware", disabled, base)

	installReader(t)
	enabled := measure(build(true))
	assert.Greater(t, enabled-base, 2.0, "the enabled middleware does allocate, so the comparison above means something")
}

// TestHTTPMetricsMethodLabelIsBounded: net/http accepts any token as a
// method, so the label maps the nine standard methods to themselves and
// everything else to _OTHER. Otherwise a client could open a new series per
// junk method until the SDK's cardinality limit pushes the real routes into
// the overflow series.
func TestHTTPMetricsMethodLabelIsBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := installReader(t)
	router := gin.New()
	router.Use(HTTPMetrics())
	router.Any("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	standard := []string{"GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH"}
	for _, m := range standard {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(m, "/x", nil))
	}
	for i := 0; i < 50; i++ {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(fmt.Sprintf("JUNK%d", i), "/x", nil))
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("get", "/x", nil)) // methods are case-sensitive

	rm := collectMetrics(t, reader)
	counter, ok := findCounter(rm, "http.server.request.total")
	require.True(t, ok)
	byMethod := map[string]int64{}
	for _, dp := range counter.DataPoints {
		m, _ := dp.Attributes.Value(attribute.Key("http.request.method"))
		byMethod[m.AsString()] += dp.Value
	}
	want := map[string]int64{"_OTHER": 51}
	for _, m := range standard {
		want[m] = 1
	}
	assert.Equal(t, want, byMethod)
	assert.Len(t, counter.DataPoints, len(standard)+1, "one series per standard method and one for the rest")

	hist, ok := findHistogram(rm, "http.server.request.duration")
	require.True(t, ok)
	assert.Len(t, hist.DataPoints, len(standard)+1, "the duration histogram is bounded the same way")
}

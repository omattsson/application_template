package routes

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/telemetry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The tests of this file install process-wide OTel providers, so none of them
// runs in parallel. Parallel tests of the package only start after the
// sequential ones are done.

type memTelemetry struct {
	spans  *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
}

// installTelemetry makes in-memory providers the global ones, as
// telemetry.Init does with real exporters, and restores the no-op ones.
func installTelemetry(t *testing.T) *memTelemetry {
	t.Helper()
	m := &memTelemetry{spans: tracetest.NewInMemoryExporter(), reader: sdkmetric.NewManualReader()}
	tel, err := telemetry.InitWithBackends(config.OtelConfig{Enabled: true, ServiceName: "routes-test", SampleRate: 1}, telemetry.Backends{
		Spans:   sdktrace.NewSimpleSpanProcessor(m.spans),
		Metrics: m.reader,
		Logs:    sdklog.NewSimpleProcessor(noLogExporter{}),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, tel.Shutdown(context.Background()))
		_, err := telemetry.Init(config.OtelConfig{})
		require.NoError(t, err)
	})
	return m
}

type noLogExporter struct{}

func (noLogExporter) Export(context.Context, []sdklog.Record) error { return nil }
func (noLogExporter) Shutdown(context.Context) error                { return nil }
func (noLogExporter) ForceFlush(context.Context) error              { return nil }

func (m *memTelemetry) requestCounter(t *testing.T) []metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, m.reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name == "http.server.request.total" {
				return mt.Data.(metricdata.Sum[int64]).DataPoints
			}
		}
	}
	return nil
}

func otelConfig(enabled, metrics bool) *config.Config {
	cfg := testConfig(100, 10, nil)
	cfg.Otel = config.OtelConfig{Enabled: enabled, MetricsEnabled: metrics, ServiceName: "routes-test", SampleRate: 1}
	return cfg
}

func attrsOf(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// TestTelemetryEnabledRecordsOneSpanAndOneRequestCounter: GET /api/v1/items
// with OTEL_ENABLED=true produces exactly one server span and one count of
// the request counter, labelled with the route pattern and the status.
func TestTelemetryEnabledRecordsOneSpanAndOneRequestCounter(t *testing.T) {
	m := installTelemetry(t)
	router, _ := newTestRouter(t, otelConfig(true, false))

	w := doFrom(router, testPeer, http.MethodGet, "/api/v1/items", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	spans := m.spans.GetSpans()
	require.Len(t, spans, 1, "one server span per request")
	assert.Equal(t, "GET /api/v1/items", spans[0].Name)
	attrs := attrsOf(spans[0].Attributes)
	assert.Equal(t, "200", attrs["http.response.status_code"])

	points := m.requestCounter(t)
	require.Len(t, points, 1)
	assert.EqualValues(t, 1, points[0].Value)
	route, _ := points[0].Attributes.Value("http.route")
	status, _ := points[0].Attributes.Value("http.response.status_code")
	method, _ := points[0].Attributes.Value("http.request.method")
	assert.Equal(t, "/api/v1/items", route.AsString())
	assert.EqualValues(t, 200, status.AsInt64())
	assert.Equal(t, "GET", method.AsString())
}

// TestTelemetryErrorsAreCountedAndTheBodyStaysGeneric: telemetry changes no
// answer. A 404 is counted under its status and the unmatched route label.
func TestTelemetryErrorsAreCountedAndTheBodyStaysGeneric(t *testing.T) {
	m := installTelemetry(t)
	router, _ := newTestRouter(t, otelConfig(true, true))

	w := doFrom(router, testPeer, http.MethodGet, "/api/v1/items/9999", "", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = doFrom(router, testPeer, http.MethodGet, "/no/such/route", "", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)

	routes := map[string]int64{}
	for _, dp := range m.requestCounter(t) {
		r, _ := dp.Attributes.Value("http.route")
		routes[r.AsString()] += dp.Value
	}
	assert.Equal(t, map[string]int64{"/api/v1/items/:id": 1, "<unmatched>": 1}, routes)
}

// TestTelemetryMetricsOnlyHasNoSpans: METRICS_ENABLED without OTEL_ENABLED
// counts requests but starts no span.
func TestTelemetryMetricsOnlyHasNoSpans(t *testing.T) {
	m := installTelemetry(t)
	router, _ := newTestRouter(t, otelConfig(false, true))

	w := doFrom(router, testPeer, http.MethodGet, "/api/v1/ping", "", nil)
	require.Equal(t, http.StatusOK, w.Code)

	assert.Empty(t, m.spans.GetSpans(), "tracing needs OTEL_ENABLED")
	require.Len(t, m.requestCounter(t), 1, "METRICS_ENABLED registers the request counter")
}

// TestTelemetryDisabledRegistersNothing: with both switches off the router
// records nothing even when global providers exist, so existing requests are
// unchanged.
func TestTelemetryDisabledRegistersNothing(t *testing.T) {
	m := installTelemetry(t)
	router, _ := newTestRouter(t, otelConfig(false, false))

	w := doFrom(router, testPeer, http.MethodGet, "/api/v1/items", "", nil)
	require.Equal(t, http.StatusOK, w.Code)

	assert.Empty(t, m.spans.GetSpans())
	assert.Empty(t, m.requestCounter(t))
}

// TestTelemetryNeverRecordsTheWebSocketToken: RedactWSToken runs before the
// tracer, so the ?token= credential of a /ws request is in no span attribute
// and in no metric label.
func TestTelemetryNeverRecordsTheWebSocketToken(t *testing.T) {
	m := installTelemetry(t)
	router, _ := newTestRouter(t, otelConfig(true, true))

	const secret = "SECRET-JWT-VALUE"
	doFrom(router, testPeer, http.MethodGet, "/ws?token="+secret, "", nil)

	spans := m.spans.GetSpans()
	require.Len(t, spans, 1)
	for k, v := range attrsOf(spans[0].Attributes) {
		assert.NotContains(t, v, secret, "span attribute %s", k)
	}
	assert.NotContains(t, spans[0].Name, secret)
	for _, dp := range m.requestCounter(t) {
		for _, kv := range dp.Attributes.ToSlice() {
			assert.False(t, strings.Contains(kv.Value.Emit(), secret), "metric label %s", kv.Key)
		}
	}
}

// TestTelemetryAuthenticatedSpanCarriesTheUser: with auth on, the span of an
// authenticated request has the user attributes of the auth middleware.
func TestTelemetryAuthenticatedSpanCarriesTheUser(t *testing.T) {
	m := installTelemetry(t)
	cfg := authOn()
	cfg.Otel = config.OtelConfig{Enabled: true, ServiceName: "routes-test", SampleRate: 1}
	env := newAuthEnv(t, cfg)
	u := env.createUser(t, "alice", models.RoleUser)

	w := env.do(t, http.MethodGet, "/api/v1/items", "", bearer(env.token(t, u)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	spans := m.spans.GetSpans()
	require.Len(t, spans, 1)
	attrs := attrsOf(spans[0].Attributes)
	assert.Equal(t, u.ID, attrs["user.id"])
	assert.Equal(t, "alice", attrs["user.name"])
	assert.Equal(t, models.RoleUser, attrs["user.role"])

	// An unauthenticated request is refused and has no user attributes.
	m.spans.Reset()
	w = env.do(t, http.MethodGet, "/api/v1/items", "", nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	spans = m.spans.GetSpans()
	require.Len(t, spans, 1)
	assert.NotContains(t, attrsOf(spans[0].Attributes), "user.id")
}

// authCounts returns the series of an auth counter keyed by its attributes,
// as "method/status/reason" with the missing ones left out.
func (m *memTelemetry) authCounts(t *testing.T, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, m.reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				var parts []string
				for _, k := range []attribute.Key{"method", "status", "reason"} {
					if v, ok := dp.Attributes.Value(k); ok {
						parts = append(parts, v.AsString())
					}
				}
				out[strings.Join(parts, "/")] += dp.Value
			}
		}
	}
	return out
}

// TestTelemetryCountsAuthOutcomes: with telemetry on, the real router counts
// login, refresh, bearer-token and API key outcomes, and every answer is the
// one it was without telemetry.
func TestTelemetryCountsAuthOutcomes(t *testing.T) {
	m := installTelemetry(t)
	cfg := authOn()
	cfg.Otel = config.OtelConfig{MetricsEnabled: true, ServiceName: "routes-test", SampleRate: 1}
	env := newAuthEnv(t, cfg)
	u := env.createUser(t, "alice", models.RoleUser)
	json := map[string]string{"X-Requested-With": "XMLHttpRequest", "Content-Type": "application/json"}

	// Login: one failure (wrong password), one success.
	w := env.do(t, http.MethodPost, "/api/v1/auth/login", `{"username":"alice","password":"wrong-password-here"}`, json)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	tok, cookie := env.login(t, "alice")

	// Refresh: one success, one refusal (no cookie).
	h := map[string]string{"X-Requested-With": "x", "Cookie": cookie.Name + "=" + cookie.Value}
	require.Equal(t, http.StatusOK, env.do(t, http.MethodPost, "/api/v1/auth/refresh", "", h).Code)
	require.Equal(t, http.StatusUnauthorized, env.do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"X-Requested-With": "x"}).Code)

	// Bearer token: success, missing header.
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/items", "", bearer(tok)).Code)
	require.Equal(t, http.StatusUnauthorized, env.do(t, http.MethodGet, "/api/v1/items", "", nil).Code)

	// API key: success, unknown key.
	raw, _ := env.apiKey(t, u)
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/items", "", map[string]string{"X-API-Key": raw}).Code)
	require.Equal(t, http.StatusUnauthorized, env.do(t, http.MethodGet, "/api/v1/items", "", map[string]string{"X-API-Key": "sk_" + strings.Repeat("a", 64)}).Code)

	assert.Equal(t, map[string]int64{"local/failure/password_mismatch": 1, "local/success": 1}, m.authCounts(t, "auth.login.total"))
	assert.Equal(t, map[string]int64{"success": 1, "refused": 1}, m.authCounts(t, "auth.token.refresh.total"))
	assert.Equal(t, map[string]int64{"success": 1, "missing": 1}, m.authCounts(t, "auth.jwt.total"))
	assert.Equal(t, map[string]int64{"success": 1, "unknown": 1}, m.authCounts(t, "auth.apikey.total"))
}

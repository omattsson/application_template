package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/sessionstore"
	"backend/internal/telemetry"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"gorm.io/gorm"
)

// metricsTelemetry builds a Telemetry with the Prometheus handler
// (METRICS_ENABLED=true) and resets the global providers when the test ends.
func metricsTelemetry(t *testing.T) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.InitWithBackends(config.OtelConfig{MetricsEnabled: true, ServiceName: "servers-test", SampleRate: 1}, telemetry.Backends{})
	require.NoError(t, err)
	require.NotNil(t, tel.MetricsHandler)
	t.Cleanup(func() {
		require.NoError(t, tel.Shutdown(context.Background()))
		_, _ = telemetry.Init(config.OtelConfig{})
	})
	return tel
}

// notListening fails the test when something still accepts connections on addr.
func notListening(t *testing.T, addr string) {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", addr)
	if err == nil {
		_ = conn.Close()
	}
	assert.Error(t, err, "nothing may listen on %s", addr)
}

// captureLog sends the default logger to a buffer until the test ends.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	restoreLogging(t)
	buf := &lockedBuffer{}
	setupLoggingTo(buf, slog.LevelDebug)
	return buf
}

func TestStartMetricsServerDisabled(t *testing.T) {
	t.Parallel()

	t.Run("METRICS_ENABLED=false starts nothing", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapTestConfig()
		cfg.Otel.MetricsAddr = "127.0.0.1:0"
		assert.Nil(t, startMetricsServer(cfg, &telemetry.Telemetry{}))
	})

	t.Run("no handler starts nothing", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapTestConfig()
		cfg.Otel.MetricsEnabled = true
		cfg.Otel.MetricsAddr = "127.0.0.1:0"
		assert.Nil(t, startMetricsServer(cfg, &telemetry.Telemetry{}))
		assert.Nil(t, startMetricsServer(cfg, nil))
	})
}

func TestStartPprofServerDisabled(t *testing.T) {
	t.Parallel()
	cfg := bootstrapTestConfig()
	cfg.Server.PprofEnabled = false
	cfg.Server.PprofAddr = "127.0.0.1:0"
	assert.Nil(t, startPprofServer(cfg), "pprof is off unless PPROF_ENABLED=true")
}

func TestPprofDefaultAddressIsLoopback(t *testing.T) {
	t.Setenv("ENV_FILE", "/nonexistent/.env")
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Server.PprofEnabled)
	assert.True(t, config.IsLoopbackAddr(cfg.Server.PprofAddr), "default PPROF_ADDR %s is loopback", cfg.Server.PprofAddr)
	assert.Equal(t, ":9090", cfg.Otel.MetricsAddr)
}

func TestStartPprofServer(t *testing.T) {
	// Not parallel: it sets the process-wide profile rates.
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	})
	cfg := bootstrapTestConfig()
	cfg.Server.PprofEnabled = true
	cfg.Server.PprofAddr = "127.0.0.1:0"
	cfg.Server.PprofMutexFraction = 7
	cfg.Server.PprofBlockRate = 1000

	srv := startPprofServer(cfg)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })

	host, port, err := net.SplitHostPort(srv.Addr)
	require.NoError(t, err)
	assert.True(t, net.ParseIP(host).IsLoopback(), "bound to loopback, got %s", host)
	assert.NotEqual(t, "0", port, "Addr is the bound address")

	assert.Contains(t, httpGet(t, "http://"+srv.Addr+"/debug/pprof/", http.StatusOK), "goroutine")
	assert.Contains(t, httpGet(t, "http://"+srv.Addr+"/debug/pprof/cmdline", http.StatusOK), "")
	httpGet(t, "http://"+srv.Addr+"/debug/pprof/heap", http.StatusOK)
	httpGet(t, "http://"+srv.Addr+"/metrics", http.StatusNotFound)
	httpGet(t, "http://"+srv.Addr+"/", http.StatusNotFound)

	assert.Equal(t, 7, runtime.SetMutexProfileFraction(-1), "PPROF_MUTEX_FRACTION is applied")
}

func TestPprofAddressPolicy(t *testing.T) {
	// Not parallel: it captures the default logger.
	tests := []struct {
		name      string
		addr      string
		allow     bool
		wantErr   bool
		wantWarn  bool
		wantInLog string
	}{
		{name: "loopback ip", addr: "127.0.0.1:6060"},
		{name: "ipv6 loopback", addr: "[::1]:6060"},
		{name: "localhost", addr: "localhost:6060"},
		{name: "all interfaces refused", addr: ":6060", wantErr: true},
		{name: "public ip refused", addr: "10.1.2.3:6060", wantErr: true},
		{name: "all interfaces allowed with a warning", addr: "0.0.0.0:6060", allow: true, wantWarn: true, wantInLog: "pprof is reachable beyond loopback"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			buf := captureLog(t)
			cfg := bootstrapTestConfig()
			cfg.Server.PprofAddr = tt.addr
			cfg.Server.PprofAllowNonLoopback = tt.allow

			err := checkPprofAddr(cfg)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "PPROF_ALLOW_NON_LOOPBACK")
			} else {
				require.NoError(t, err)
			}
			if tt.wantWarn {
				assert.Contains(t, buf.String(), "level=WARN")
				assert.Contains(t, buf.String(), tt.wantInLog)
			} else {
				assert.NotContains(t, buf.String(), "level=WARN")
			}
		})
	}
}

func TestStartPprofServerRefusesNonLoopback(t *testing.T) {
	// Not parallel: it captures the default logger and sets the profile rates.
	buf := captureLog(t)
	cfg := bootstrapTestConfig()
	cfg.Server.PprofEnabled = true
	cfg.Server.PprofAddr = ":0" // a hand-built config that LoadConfig would have refused

	assert.Nil(t, startPprofServer(cfg), "defence in depth: no listener on every interface")
	assert.Contains(t, buf.String(), "level=ERROR")
	assert.Contains(t, buf.String(), "PPROF_ALLOW_NON_LOOPBACK")
}

func TestOptionalServersBindFailureIsLoggedNotFatal(t *testing.T) {
	// Not parallel: it captures the default logger.
	taken, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })

	buf := captureLog(t)
	cfg := bootstrapTestConfig()
	cfg.Otel.MetricsEnabled = true
	cfg.Otel.MetricsAddr = taken.Addr().String()
	cfg.Server.PprofEnabled = true
	cfg.Server.PprofAddr = taken.Addr().String()
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	})

	tel := metricsTelemetry(t)
	assert.Nil(t, startMetricsServer(cfg, tel))
	assert.Nil(t, startPprofServer(cfg))
	assert.Contains(t, buf.String(), "metrics server failed")
	assert.Contains(t, buf.String(), "pprof server failed")
}

// TestMetricsServerExposesRequestDuration sends one request through the
// router and reads the series from the separate metrics port.
func TestMetricsServerExposesRequestDuration(t *testing.T) {
	// Not parallel: it installs the global OTel providers and the Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)
	tel := metricsTelemetry(t)
	cfg := bootstrapTestConfig()
	cfg.Otel.MetricsEnabled = true
	cfg.Otel.MetricsAddr = "127.0.0.1:0"

	router, res, err := buildRouter(cfg, routerDeps{HealthChecker: health.New(), Hub: websocket.NewHub()})
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health/live", nil))
	require.Equal(t, http.StatusOK, w.Code)

	// /metrics is never on the API router.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, w.Code, "/metrics is served on its own port only")

	srv := startMetricsServer(cfg, tel)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })
	assert.False(t, strings.HasSuffix(srv.Addr, ":0"), "Addr is the bound address, got %s", srv.Addr)

	body := httpGet(t, "http://"+srv.Addr+"/metrics", http.StatusOK)
	assert.Contains(t, body, "http_server_request_duration_seconds_bucket", "request duration histogram")
	assert.Contains(t, body, `http_route="/health/live"`)
	assert.Contains(t, body, "http_server_request_total", "request counter")
	assert.Contains(t, body, "go_goroutines", "Go runtime collector")
	assert.Contains(t, body, "process_", "process collector")

	// Only /metrics is served.
	httpGet(t, "http://"+srv.Addr+"/", http.StatusNotFound)
	httpGet(t, "http://"+srv.Addr+"/debug/pprof/", http.StatusNotFound)
}

func TestShutdownClosesMetricsAndPprofServers(t *testing.T) {
	// Not parallel: it installs the global OTel providers and sets profile rates.
	tel := metricsTelemetry(t)
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	})
	cfg := bootstrapTestConfig()
	cfg.Otel.MetricsEnabled = true
	cfg.Otel.MetricsAddr = "127.0.0.1:0"
	cfg.Server.PprofEnabled = true
	cfg.Server.PprofAddr = "127.0.0.1:0"

	metricsSrv := startMetricsServer(cfg, tel)
	pprofSrv := startPprofServer(cfg)
	require.NotNil(t, metricsSrv)
	require.NotNil(t, pprofSrv)
	httpGet(t, "http://"+metricsSrv.Addr+"/metrics", http.StatusOK)
	httpGet(t, "http://"+pprofSrv.Addr+"/debug/pprof/", http.StatusOK)

	s := &services{}
	addAuxServers(s, metricsSrv, pprofSrv)
	require.Len(t, s.auxServers, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx, s))

	notListening(t, metricsSrv.Addr)
	notListening(t, pprofSrv.Addr)
}

func TestAddAuxServersSkipsNil(t *testing.T) {
	t.Parallel()
	s := &services{}
	var nilSrv *http.Server
	addAuxServers(s, nilSrv, nil)
	assert.Empty(t, s.auxServers, "a disabled server is not added (a typed nil would panic in shutdown)")
	assert.NoError(t, shutdown(context.Background(), s))
}

func TestShutdownStopsAuxServersFirst(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	s.auxServers = []httpServer{
		&fakeServer{rec: rec, name: "metrics"},
		&fakeServer{rec: rec, name: "pprof"},
	}

	require.NoError(t, shutdown(context.Background(), s))

	assert.Equal(t, []string{
		"metrics", "pprof", "server", "leader group", "in-flight", "hub", "telemetry", "rate limiters", "database",
	}, rec.list(), "the diagnostics servers stop before the API drain")
}

// A pprof profile request can run for 30 s: the aux step must not wait for
// it, so it cannot eat into the grace period of the other steps.
func TestShutdownAuxServersHaveASmallBoundedBudget(t *testing.T) {
	// Not parallel: it replaces the package-level budget.
	prev := auxShutdownTimeout
	auxShutdownTimeout = 100 * time.Millisecond
	t.Cleanup(func() { auxShutdownTimeout = prev })

	rec := &callRecorder{}
	s := fakeServices(rec)
	s.auxServers = []httpServer{
		&fakeServer{rec: rec, name: "pprof", hang: true},
		&fakeServer{rec: rec, name: "metrics", hang: true},
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := shutdown(ctx, s)

	assert.Less(t, time.Since(start), 5*time.Second, "two hung servers share one small budget")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, rec.list(), "pprof close", "a server that did not drain is closed")
	assert.Contains(t, rec.list(), "metrics close")
	assert.Contains(t, rec.list(), "database", "later steps still run")
}

func TestMetricsServerTimeoutsAreSet(t *testing.T) {
	// Not parallel: it installs the global OTel providers.
	tel := metricsTelemetry(t)
	cfg := bootstrapTestConfig()
	cfg.Otel.MetricsEnabled = true
	cfg.Otel.MetricsAddr = "127.0.0.1:0"

	srv := startMetricsServer(cfg, tel)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })

	assert.Positive(t, srv.ReadHeaderTimeout, "slowloris protection")
	assert.Positive(t, srv.ReadTimeout)
	assert.Positive(t, srv.WriteTimeout)
	assert.Positive(t, srv.IdleTimeout)
}

// deadlineServer records how much time its Shutdown context has left.
type deadlineServer struct {
	remaining time.Duration
}

func (d *deadlineServer) Shutdown(ctx context.Context) error {
	if dl, ok := ctx.Deadline(); ok {
		d.remaining = time.Until(dl)
	}
	return nil
}

func (d *deadlineServer) Close() error { return nil }

// A slow diagnostics step must not shorten the drain of the API: the HTTP
// step has a budget of its own, not what is left of ctx.
func TestShutdownHTTPStepKeepsItsFullBudget(t *testing.T) {
	// Not parallel: it replaces the package-level aux budget.
	prev := auxShutdownTimeout
	auxShutdownTimeout = 300 * time.Millisecond
	t.Cleanup(func() { auxShutdownTimeout = prev })

	rec := &callRecorder{}
	api := &deadlineServer{}
	s := &services{
		auxServers: []httpServer{&fakeServer{rec: rec, name: "pprof", hang: true}},
		servers:    []httpServer{api},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := shutdown(ctx, s)

	require.Error(t, err, "the hung diagnostics server is reported")
	assert.Greater(t, api.remaining, 900*time.Millisecond,
		"the HTTP drain still gets its whole budget after the 300 ms diagnostics step")
}

// closeFailServer is a server whose Shutdown and Close fail.
type closeFailServer struct {
	shutdownErr, closeErr error
}

func (c *closeFailServer) Shutdown(context.Context) error { return c.shutdownErr }
func (c *closeFailServer) Close() error                   { return c.closeErr }

func TestShutdownReportsServersThatCannotBeClosed(t *testing.T) {
	t.Parallel()
	shutdownErr := errors.New("drain failed")
	closeErr := errors.New("close failed")
	s := &services{
		auxServers: []httpServer{&closeFailServer{shutdownErr: shutdownErr, closeErr: closeErr}},
		servers:    []httpServer{&closeFailServer{shutdownErr: shutdownErr, closeErr: closeErr}},
	}

	err := shutdown(context.Background(), s)

	require.Error(t, err)
	assert.ErrorIs(t, err, shutdownErr)
	assert.ErrorIs(t, err, closeErr)
	assert.Contains(t, err.Error(), "close diagnostics server")
	assert.Contains(t, err.Error(), "close http server")
}

// failingListener accepts nothing: Serve returns its error at once.
type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept boom") }
func (failingListener) Close() error              { return nil }
func (failingListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestServeListenerLogsAServeError(t *testing.T) {
	// Not parallel: it captures the default logger.
	buf := captureLog(t)
	srv := &http.Server{Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second}

	done := serveListener(srv, failingListener{}, "test server")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the serve goroutine did not end")
	}
	assert.Contains(t, buf.String(), "level=ERROR")
	assert.Contains(t, buf.String(), "test server failed")
	assert.Contains(t, buf.String(), "accept boom")
}

func TestMetricsAddrReminder(t *testing.T) {
	// Not parallel: it captures the default logger.
	tests := []struct {
		name     string
		addr     string
		wantNote bool
	}{
		{name: "default, all interfaces", addr: ":9090", wantNote: true},
		{name: "any interface", addr: "0.0.0.0:9090", wantNote: true},
		{name: "a private address", addr: "10.0.0.5:9090", wantNote: true},
		{name: "loopback", addr: "127.0.0.1:9090"},
		{name: "localhost", addr: "localhost:9090"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			buf := captureLog(t)
			noteMetricsAddr(tt.addr)

			if !tt.wantNote {
				assert.NotContains(t, buf.String(), "unauthenticated")
				return
			}
			assert.Contains(t, buf.String(), "level=INFO", "an Info, not a Warn: the Compose default is :9090")
			assert.NotContains(t, buf.String(), "level=WARN")
			assert.Contains(t, buf.String(), "unauthenticated")
			assert.Contains(t, buf.String(), "private network")
			assert.Contains(t, buf.String(), tt.addr)
		})
	}
}

func TestStartMetricsServerLogsTheReminderOnlyBeyondLoopback(t *testing.T) {
	// Not parallel: it installs the global OTel providers and captures the logger.
	tel := metricsTelemetry(t)
	buf := captureLog(t)
	cfg := bootstrapTestConfig()
	cfg.Otel.MetricsEnabled = true
	cfg.Otel.MetricsAddr = "127.0.0.1:0"

	srv := startMetricsServer(cfg, tel)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })

	assert.NotContains(t, buf.String(), "unauthenticated")
}

// net/http/pprof registers its handlers on http.DefaultServeMux in init: a
// server with a nil Handler would serve them on its port. Every server of the
// process therefore needs an explicit handler, and pprof lives only on its
// own port.
func TestEveryServerHasAnExplicitHandler(t *testing.T) {
	// Not parallel: it installs the global OTel providers, sets the Gin mode and
	// the profile rates.
	t.Setenv("GIN_MODE", gin.TestMode)
	tel := metricsTelemetry(t)
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	})
	cfg := bootstrapTestConfig()
	cfg.Otel.MetricsEnabled = true
	cfg.Otel.MetricsAddr = "127.0.0.1:0"
	cfg.Server.PprofEnabled = true
	cfg.Server.PprofAddr = "127.0.0.1:0"

	router, res, err := buildRouter(cfg, routerDeps{HealthChecker: health.New(), Hub: websocket.NewHub()})
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)

	apiSrv := startHTTPServer(cfg, router)
	metricsSrv := startMetricsServer(cfg, tel)
	pprofSrv := startPprofServer(cfg)
	require.NotNil(t, metricsSrv)
	require.NotNil(t, pprofSrv)
	for _, srv := range []*http.Server{apiSrv, metricsSrv, pprofSrv} {
		srv := srv
		t.Cleanup(func() { _ = srv.Close() })
		assert.NotNil(t, srv.Handler, "%s must not fall back to http.DefaultServeMux", srv.Addr)
	}

	httpGet(t, "http://"+apiSrv.Addr+"/debug/pprof/", http.StatusNotFound)
	httpGet(t, "http://"+apiSrv.Addr+"/metrics", http.StatusNotFound)
}

// failingShutdownSpans is a span processor whose Shutdown fails.
type failingShutdownSpans struct{ sdktrace.SpanProcessor }

func (failingShutdownSpans) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (failingShutdownSpans) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (failingShutdownSpans) ForceFlush(context.Context) error                { return nil }
func (failingShutdownSpans) Shutdown(context.Context) error                  { return errors.New("export boom") }

func TestFlushTelemetry(t *testing.T) {
	// Not parallel: it captures the default logger and installs the global providers.
	t.Run("a flush that works logs nothing", func(t *testing.T) {
		buf := captureLog(t)
		flushTelemetry(&telemetry.Telemetry{})
		assert.NotContains(t, buf.String(), "Telemetry flush failed")
	})

	t.Run("a failed flush is logged and does not panic", func(t *testing.T) {
		buf := captureLog(t)
		tel, err := telemetry.InitWithBackends(config.OtelConfig{Enabled: true, ServiceName: "flush-test", SampleRate: 1},
			telemetry.Backends{Spans: failingShutdownSpans{}})
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = telemetry.Init(config.OtelConfig{}) })

		require.NotPanics(t, func() { flushTelemetry(tel) })

		assert.Contains(t, buf.String(), "level=WARN")
		assert.Contains(t, buf.String(), "Telemetry flush failed")
		assert.Contains(t, buf.String(), "export boom")
	})
}

func TestBuildRouterRefusesAuthWithoutStores(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)
	cfg := authModeConfig(true)

	router, res, err := buildRouter(cfg, routerDeps{HealthChecker: health.New(), Hub: websocket.NewHub()})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "build the router")
	assert.Nil(t, router)
	assert.Nil(t, res)
}

func TestOpenAuthStoresReturnsTheStoreError(t *testing.T) {
	// Not parallel: it replaces newSessionStore.
	prev := newSessionStore
	t.Cleanup(func() { newSessionStore = prev })
	storeErr := errors.New("store boom")
	newSessionStore = func(string, *gorm.DB) (sessionstore.SessionStore, error) { return nil, storeErr }

	repo, _ := openSQLiteRepo(t)
	deps, err := openAuthStores(authModeConfig(true), repo)

	require.ErrorIs(t, err, storeErr)
	assert.Nil(t, deps)
}

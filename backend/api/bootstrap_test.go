package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/leader"
	"backend/internal/models"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bootstrapTestConfig returns a valid configuration for the bootstrap tests:
// loopback host, port 0 (any free port), Swagger off, release defaults.
func bootstrapTestConfig() *config.Config {
	return &config.Config{
		App: config.AppConfig{Name: "testapp", Environment: "testing", Title: "Smoke Title"},
		Server: config.ServerConfig{
			Host:           "127.0.0.1",
			Port:           "0",
			ReadTimeout:    5 * time.Second,
			IdleTimeout:    5 * time.Second,
			RateLimit:      100,
			LoginRateLimit: 10,
		},
		CORS: config.CORSConfig{AllowedOrigins: "*"},
	}
}

// callRecorder records the order of the shutdown calls.
type callRecorder struct {
	calls []string
	mu    sync.Mutex
}

func (r *callRecorder) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *callRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type fakeServer struct {
	rec  *callRecorder
	err  error
	name string
}

func (f *fakeServer) Shutdown(context.Context) error { f.rec.add(f.name); return f.err }

type fakeLeader struct{ rec *callRecorder }

func (f *fakeLeader) Close(time.Duration) bool { f.rec.add("leader group"); return true }

type fakeWaiter struct{ rec *callRecorder }

func (f *fakeWaiter) Wait() { f.rec.add("in-flight") }

type fakeHub struct{ rec *callRecorder }

func (f *fakeHub) Shutdown() { f.rec.add("hub") }

type fakeStopper struct{ rec *callRecorder }

func (f *fakeStopper) Stop() { f.rec.add("rate limiters") }

type fakeCloser struct {
	rec *callRecorder
	err error
}

func (f *fakeCloser) Close() error { f.rec.add("database"); return f.err }

// fakeServices returns services whose every part records its call.
func fakeServices(rec *callRecorder) *services {
	return &services{
		servers:  []serverShutdowner{&fakeServer{rec: rec, name: "server"}},
		leader:   &fakeLeader{rec: rec},
		inFlight: &fakeWaiter{rec: rec},
		hub:      &fakeHub{rec: rec},
		telemetryShutdown: func(context.Context) error {
			rec.add("telemetry")
			return nil
		},
		rateLimiters: &fakeStopper{rec: rec},
		db:           &fakeCloser{rec: rec},
	}
}

func TestShutdownOrder(t *testing.T) {
	t.Parallel()

	t.Run("stops every service in order", func(t *testing.T) {
		t.Parallel()
		rec := &callRecorder{}

		err := shutdown(context.Background(), fakeServices(rec))

		require.NoError(t, err)
		assert.Equal(t, []string{
			"server", "leader group", "in-flight", "hub", "telemetry", "rate limiters", "database",
		}, rec.list())
	})

	t.Run("errors are reported and later steps still run", func(t *testing.T) {
		t.Parallel()
		rec := &callRecorder{}
		s := fakeServices(rec)
		serverErr := errors.New("server boom")
		dbErr := errors.New("db boom")
		telErr := errors.New("telemetry boom")
		s.servers = []serverShutdowner{
			&fakeServer{rec: rec, name: "server", err: serverErr},
			&fakeServer{rec: rec, name: "second server"},
		}
		s.telemetryShutdown = func(context.Context) error {
			rec.add("telemetry")
			return telErr
		}
		s.db = &fakeCloser{rec: rec, err: dbErr}

		err := shutdown(context.Background(), s)

		require.Error(t, err)
		assert.ErrorIs(t, err, serverErr)
		assert.ErrorIs(t, err, telErr)
		assert.ErrorIs(t, err, dbErr)
		assert.Equal(t, []string{
			"server", "second server", "leader group", "in-flight", "hub", "telemetry", "rate limiters", "database",
		}, rec.list())
	})

	t.Run("empty services is a no-op", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, shutdown(context.Background(), &services{}))
	})
}

func TestShutdownCompletesWithinTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 200 * time.Millisecond

	tests := []struct {
		name      string
		hangGroup bool
		hangWork  bool
		wantInErr string
	}{
		{name: "hung leader worker", hangGroup: true, wantInErr: "leader workers"},
		{name: "hung in-flight work", hangWork: true, wantInErr: "in-flight work"},
		{name: "both hung", hangGroup: true, hangWork: true, wantInErr: "leader workers"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &callRecorder{}
			s := fakeServices(rec)

			release := make(chan struct{})
			t.Cleanup(func() { close(release) })

			if tt.hangGroup {
				started := make(chan struct{})
				// The worker ignores ctx: it returns only on release.
				g := leader.NewGroup(time.Hour, leader.Worker{Name: "hung", Run: func(context.Context) {
					close(started)
					<-release
				}})
				g.Start(context.Background())
				<-started
				s.leader = g
			}
			if tt.hangWork {
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-release
				}()
				s.inFlight = &wg
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- shutdown(ctx, s) }()

			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not return: deadlock")
			}
			elapsed := time.Since(start)

			require.Error(t, err)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Contains(t, err.Error(), tt.wantInErr)
			assert.Less(t, elapsed, 2*time.Second, "shutdown must not wait past the timeout")
			assert.GreaterOrEqual(t, elapsed, timeout-20*time.Millisecond)
			// The steps after the hung one still ran.
			calls := rec.list()
			assert.Contains(t, calls, "hub")
			assert.Contains(t, calls, "telemetry")
			assert.Contains(t, calls, "rate limiters")
			assert.Contains(t, calls, "database")
		})
	}
}

func TestSwaggerRouteOnlyWhenEnabled(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode, and
	// t.Setenv modifies the process environment.
	t.Setenv("GIN_MODE", gin.TestMode)

	tests := []struct {
		name    string
		enabled bool
		want    int
	}{
		{name: "disabled returns 404", enabled: false, want: http.StatusNotFound},
		{name: "enabled returns 200", enabled: true, want: http.StatusOK},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cfg := bootstrapTestConfig()
			cfg.Server.EnableSwagger = tt.enabled

			router, rateLimiters := buildRouter(cfg, routerDeps{
				HealthChecker: health.New(),
				Hub:           websocket.NewHub(),
			})
			t.Cleanup(rateLimiters.Stop)

			for _, path := range []string{"/swagger/index.html", "/swagger/doc.json"} {
				w := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
				router.ServeHTTP(w, req)
				assert.Equal(t, tt.want, w.Code, path)
			}
		})
	}
}

func TestBuildRouterSetsGinMode(t *testing.T) {
	// Not parallel: it changes the process-wide Gin mode.
	t.Cleanup(func() { gin.SetMode(gin.TestMode) })

	tests := []struct {
		name    string
		ginMode string
		want    string
	}{
		{name: "APP_DEBUG false is release", want: gin.ReleaseMode},
		{name: "GIN_MODE wins", ginMode: gin.TestMode, want: gin.TestMode},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GIN_MODE", tt.ginMode)
			cfg := bootstrapTestConfig()
			cfg.App.Debug = false

			_, rateLimiters := buildRouter(cfg, routerDeps{HealthChecker: health.New(), Hub: websocket.NewHub()})
			t.Cleanup(rateLimiters.Stop)

			assert.Equal(t, tt.want, gin.Mode())
		})
	}
}

func TestStartHTTPServer(t *testing.T) {
	t.Parallel()
	cfg := bootstrapTestConfig()
	cfg.Server.WriteTimeout = 7 * time.Second

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })

	srv := startHTTPServer(cfg, mux)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	assert.False(t, strings.HasSuffix(srv.Addr, ":0"), "Addr is the bound address, got %s", srv.Addr)
	assert.Equal(t, cfg.Server.ReadTimeout, srv.ReadTimeout)
	assert.Equal(t, cfg.Server.WriteTimeout, srv.WriteTimeout)
	assert.Equal(t, cfg.Server.IdleTimeout, srv.IdleTimeout)

	body := httpGet(t, "http://"+srv.Addr+"/ping", http.StatusOK)
	assert.Equal(t, "pong", body)
}

func TestStartHTTPServerBindFailureExits(t *testing.T) {
	// Not parallel: it replaces the package-level exit function.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var code int
	prev := exit
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = prev })

	cfg := bootstrapTestConfig()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	cfg.Server.Port = port

	srv := startHTTPServer(cfg, http.NewServeMux())

	require.NotNil(t, srv)
	assert.Equal(t, 1, code)
}

func TestShutdownTimeout(t *testing.T) {
	t.Parallel()
	cfg := bootstrapTestConfig()
	assert.Equal(t, defaultShutdownTimeout, shutdownTimeout(cfg))
	cfg.Server.ShutdownTimeout = 12 * time.Second
	assert.Equal(t, 12*time.Second, shutdownTimeout(cfg))
}

// stubRepository is a models.Repository whose Ping and Close are recorded.
// The other methods are not used by the bootstrap and panic (nil embed).
type stubRepository struct {
	models.Repository
	pingErr error
	mu      sync.Mutex
	closed  bool
}

func (r *stubRepository) Ping(context.Context) error { return r.pingErr }

func (r *stubRepository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *stubRepository) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func TestInitDatabase(t *testing.T) {
	// Not parallel: it replaces the package-level repository factory.
	prev := openRepository
	t.Cleanup(func() { openRepository = prev })

	t.Run("returns the repository", func(t *testing.T) {
		want := &stubRepository{}
		openRepository = func(*config.Config) (models.Repository, error) { return want, nil }

		got, err := initDatabase(bootstrapTestConfig())

		require.NoError(t, err)
		assert.Same(t, want, got)
	})

	t.Run("returns the error", func(t *testing.T) {
		boom := errors.New("no database")
		openRepository = func(*config.Config) (models.Repository, error) { return nil, boom }

		got, err := initDatabase(bootstrapTestConfig())

		require.ErrorIs(t, err, boom)
		assert.Nil(t, got)
	})
}

// TestBootstrapPipeline builds the services, serves them on a free port,
// calls the public endpoints and shuts everything down.
func TestBootstrapPipeline(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)
	cfg := bootstrapTestConfig()
	repo := &stubRepository{}

	s, router := buildServices(cfg, repo)
	require.NotNil(t, s)
	require.NotNil(t, router)
	assert.NotNil(t, s.leader)
	assert.NotNil(t, s.inFlight)
	assert.NotNil(t, s.hub)
	assert.NotNil(t, s.telemetryShutdown)
	assert.NotNil(t, s.rateLimiters)
	assert.Same(t, repo, s.db)

	srv := startHTTPServer(cfg, router)
	s.servers = append(s.servers, srv)
	base := "http://" + srv.Addr

	assert.Contains(t, httpGet(t, base+"/health/live", http.StatusOK), "UP")
	assert.Contains(t, httpGet(t, base+"/health/ready", http.StatusOK), "UP")

	var ui struct {
		Title       string `json:"title"`
		AuthEnabled bool   `json:"auth_enabled"`
	}
	require.NoError(t, json.Unmarshal([]byte(httpGet(t, base+"/api/v1/ui-config", http.StatusOK)), &ui))
	assert.Equal(t, "Smoke Title", ui.Title)
	assert.False(t, ui.AuthEnabled)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx, s))
	assert.True(t, repo.isClosed())

	// The server no longer accepts connections.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/health/live", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	assert.Error(t, err)
}

// httpGet fetches url, checks the status code and returns the body.
func httpGet(t *testing.T, url string, wantCode int) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, wantCode, resp.StatusCode, "GET %s: %s", url, body)
	return string(body)
}

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
	"backend/internal/inflight"
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
	// hang makes Shutdown block until ctx ends, like a hung request.
	hang bool
}

func (f *fakeServer) Shutdown(ctx context.Context) error {
	f.rec.add(f.name)
	if f.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f *fakeServer) Close() error { f.rec.add(f.name + " close"); return nil }

// fakeLeader records the stop timeout it got.
type fakeLeader struct {
	rec     *callRecorder
	timeout time.Duration
}

func (f *fakeLeader) Close(timeout time.Duration) bool {
	f.timeout = timeout
	f.rec.add("leader group")
	return true
}

// ctxState is the state of a step context when the step started.
type ctxState struct {
	err  error
	left time.Duration // time until the deadline; 0 without one
}

func stateOf(ctx context.Context) ctxState {
	st := ctxState{err: ctx.Err()}
	if dl, ok := ctx.Deadline(); ok {
		st.left = time.Until(dl)
	}
	return st
}

// fakeWaiter records the context the in-flight step got.
type fakeWaiter struct {
	rec *callRecorder
	got ctxState
}

func (f *fakeWaiter) Wait(ctx context.Context) error {
	f.got = stateOf(ctx)
	f.rec.add("in-flight")
	return nil
}

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
		servers:  []httpServer{&fakeServer{rec: rec, name: "server"}},
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

	// A server whose Shutdown failed is closed, so its handlers do not run
	// on while the database closes.
	t.Run("errors are reported and later steps still run", func(t *testing.T) {
		t.Parallel()
		rec := &callRecorder{}
		s := fakeServices(rec)
		serverErr := errors.New("server boom")
		dbErr := errors.New("db boom")
		telErr := errors.New("telemetry boom")
		s.servers = []httpServer{
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
			"server", "server close", "second server", "leader group", "in-flight", "hub", "telemetry", "rate limiters", "database",
		}, rec.list())
	})

	t.Run("empty services is a no-op", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, shutdown(context.Background(), &services{}))
	})
}

// A typed nil in the inFlight interface is not == nil; shutdown must skip it
// like an untyped nil and still run every other step.
func TestShutdownSkipsTypedNilInFlight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		w    waiter
	}{
		{name: "untyped nil", w: nil},
		{name: "nil *inflight.Tracker", w: (*inflight.Tracker)(nil)},
		{name: "nil pointer of a waiter that dereferences", w: (*fakeWaiter)(nil)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &callRecorder{}
			s := fakeServices(rec)
			s.inFlight = tt.w

			var err error
			require.NotPanics(t, func() { err = shutdown(context.Background(), s) })

			require.NoError(t, err)
			assert.Equal(t, []string{"server", "leader group", "hub", "telemetry", "rate limiters", "database"}, rec.list())
		})
	}
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
		{name: "both hung", hangGroup: true, hangWork: true, wantInErr: "in-flight work"},
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
				tracker := inflight.New()
				require.True(t, tracker.Go(func() { <-release }))
				s.inFlight = tracker
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
			// Each hung step waits its own budget (see shutdown): at most
			// two budgets here.
			assert.Less(t, elapsed, 2*time.Second, "shutdown must not wait past the step budgets")
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

// TestShutdownBudgetsAreIndependent checks that a hung HTTP shutdown, which
// uses up the whole ctx, does not starve the later steps: the leader group,
// the in-flight work and the telemetry hook each get their own budget.
func TestShutdownBudgetsAreIndependent(t *testing.T) {
	t.Parallel()

	const budget = 300 * time.Millisecond
	rec := &callRecorder{}
	s := fakeServices(rec)
	s.servers = []httpServer{&fakeServer{rec: rec, name: "server", hang: true}}
	lead := &fakeLeader{rec: rec}
	s.leader = lead
	wait := &fakeWaiter{rec: rec}
	s.inFlight = wait
	var tel ctxState
	s.telemetryShutdown = func(ctx context.Context) error {
		tel = stateOf(ctx)
		rec.add("telemetry")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	err := shutdown(ctx, s)

	require.ErrorIs(t, err, context.DeadlineExceeded, "the hung HTTP shutdown is reported")
	assert.Equal(t, []string{
		"server", "server close", "leader group", "in-flight", "hub", "telemetry", "rate limiters", "database",
	}, rec.list())
	assert.Error(t, ctx.Err(), "the HTTP step used up the shared ctx")

	assert.GreaterOrEqual(t, lead.timeout, budget-50*time.Millisecond, "leader step got its own budget")
	assert.NoError(t, wait.got.err, "in-flight step got a live ctx")
	assert.Greater(t, wait.got.left, budget/2)
	assert.NoError(t, tel.err, "telemetry hook got a live ctx")
	assert.Greater(t, tel.left, budget/2)
}

func TestShutdownStepBudget(t *testing.T) {
	t.Parallel()

	t.Run("expired ctx gets the floor", func(t *testing.T) {
		t.Parallel()
		rec := &callRecorder{}
		s := fakeServices(rec)
		lead := &fakeLeader{rec: rec}
		s.leader = lead
		var tel ctxState
		s.telemetryShutdown = func(ctx context.Context) error { tel = stateOf(ctx); return nil }

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_ = shutdown(ctx, s)

		assert.Equal(t, minStepBudget, lead.timeout)
		assert.NoError(t, tel.err)
		assert.Greater(t, tel.left, time.Duration(0))
	})

	t.Run("ctx without deadline gets the default", func(t *testing.T) {
		t.Parallel()
		rec := &callRecorder{}
		s := fakeServices(rec)
		lead := &fakeLeader{rec: rec}
		s.leader = lead

		require.NoError(t, shutdown(context.Background(), s))

		assert.Equal(t, defaultShutdownTimeout, lead.timeout)
	})
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

			router, res, err := buildRouter(cfg, routerDeps{
				HealthChecker: health.New(),
				Hub:           websocket.NewHub(),
			})
			require.NoError(t, err)
			t.Cleanup(res.RateLimiters.Stop)

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

			_, res, err := buildRouter(cfg, routerDeps{HealthChecker: health.New(), Hub: websocket.NewHub()})
			require.NoError(t, err)
			t.Cleanup(res.RateLimiters.Stop)

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

	s, router, err := buildServices(cfg, repo)
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, router)
	assert.NotNil(t, s.leader)
	tracker, ok := s.inFlight.(*inflight.Tracker)
	require.True(t, ok, "in-flight work is an *inflight.Tracker")
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
	assert.Nil(t, s.auditRecorder, "no audit recorder with auth off")
	assert.Nil(t, s.sessionStore, "no session store with auth off")
	require.NoError(t, json.Unmarshal([]byte(httpGet(t, base+"/api/v1/ui-config", http.StatusOK)), &ui))
	assert.Equal(t, "Smoke Title", ui.Title)
	assert.False(t, ui.AuthEnabled)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx, s))
	assert.True(t, repo.isClosed())
	assert.False(t, tracker.Go(func() {}), "no new in-flight work after shutdown began")

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

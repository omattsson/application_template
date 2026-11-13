package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"time"

	_ "backend/docs" // registers the Swagger spec served by /swagger/*any
	"backend/internal/api/routes"
	"backend/internal/config"
	"backend/internal/ctxutil"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/inflight"
	"backend/internal/leader"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/telemetry"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"gorm.io/gorm"
)

// minStepBudget is the smallest budget of a shutdown step.
const minStepBudget = 100 * time.Millisecond

// defaultShutdownTimeout is the shutdown budget when SERVER_SHUTDOWN_TIMEOUT
// is 0.
const defaultShutdownTimeout = 5 * time.Second

// Intervals of the leader workers of the auth module.
const (
	// refreshTokenCleanupInterval is how often the leader deletes the refresh
	// tokens that expired (AuthHandler.CleanupExpiredTokens).
	refreshTokenCleanupInterval = time.Hour
	// sessionStoreCleanupInterval is how often the leader deletes the expired
	// session store entries. Each store also cleans up after itself every
	// minute (its janitor); the worker keeps the table small when that
	// janitor falls behind, from one replica only.
	sessionStoreCleanupInterval = 15 * time.Minute
	// ensureAdminTimeout bounds the admin bootstrap at start.
	ensureAdminTimeout = 30 * time.Second
)

// openRepository opens the database and runs the migrations. Tests replace it.
var openRepository = database.NewRepository

// exit ends the process. Tests replace it.
var exit = os.Exit

// startFailureHook runs before the process exits because the HTTP server
// could not start. main sets it to flush the telemetry, so the "Failed to
// start server" record reaches the collector. Tests replace it.
var startFailureHook = func() { /* nothing to flush until main sets it */ }

// The interfaces below are the parts of each service that shutdown uses.
// Tests pass fakes to check the order.

// httpServer is an HTTP server (*http.Server).
type httpServer interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// groupCloser is the leader worker group (*leader.Group).
type groupCloser interface {
	Close(timeout time.Duration) bool
}

// waiter is the in-flight work (*inflight.Tracker).
type waiter interface {
	Wait(ctx context.Context) error
}

// hubShutdowner is the WebSocket hub (*websocket.Hub).
type hubShutdowner interface {
	Shutdown()
}

// stopper is the rate limiters (*routes.RateLimiters) and the session store
// (sessionstore.SessionStore, whose Stop ends its cleanup goroutine).
type stopper interface {
	Stop()
}

// auditShutdowner is the audit recorder (*middleware.AuditRecorder).
type auditShutdowner interface {
	Shutdown(ctx context.Context) error
}

// closer is the data layer (models.Repository).
type closer interface {
	Close() error
}

// services holds what shutdown stops. A nil field is skipped.
type services struct {
	// auditRecorder writes the audit entries of the requests (auth on).
	auditRecorder auditShutdowner
	// sessionStore is the token blocklist and OIDC state store (auth on).
	sessionStore stopper
	// leader runs the leader-only background workers.
	leader groupCloser
	// inFlight is the work that outlives its request or worker run
	// (*inflight.Tracker; see routerDeps.InFlight).
	inFlight waiter
	hub      hubShutdowner
	// telemetryShutdown flushes the telemetry (attachTelemetry sets
	// Telemetry.Shutdown; a no-op until then).
	telemetryShutdown func(ctx context.Context) error
	rateLimiters      stopper
	db                closer
	servers           []httpServer
	// auxServers are the metrics and pprof servers (servers.go). shutdown
	// stops them first, within auxShutdownTimeout.
	auxServers []httpServer
}

// isNilInterface reports whether v is nil or holds a nil pointer. A typed nil
// (for example a nil *inflight.Tracker) in an interface is not == nil, so a
// plain nil check would call a method on it.
func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// setupLoggingTo makes a slog text handler on w, filtered at level, the
// default logger. It is a new text handler, not a wrapper of the previous
// default handler: that handler writes through the log package, and after
// slog.SetDefault the log package writes through the default logger again,
// which deadlocks.
func setupLoggingTo(w io.Writer, level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
}

// setupOTelLogging sends the logs also to the OTel log pipeline (OTLP to the
// collector) when OTEL_ENABLED=true, with LOG_LEVEL as the level of both
// outputs. Without a pipeline it changes nothing.
//
// The local output is a new slog text handler on stderr, never a wrapper of
// the current default handler: that one writes through the log package, and
// after slog.SetDefault the log package writes through the default logger
// again, which deadlocks (for example on a log.Print of a library). Because
// each call builds its own handler, calling it twice replaces the first setup
// instead of stacking on it. With OTEL_ENABLED the local lines use the slog
// text format (time=... level=...), as setupLoggingTo does.
func setupOTelLogging(tel *telemetry.Telemetry, level slog.Level) {
	setupOTelLoggingTo(tel, level, os.Stderr)
}

// setupOTelLoggingTo is setupOTelLogging with the local output as a parameter
// (tests).
func setupOTelLoggingTo(tel *telemetry.Telemetry, level slog.Level, local io.Writer) {
	if tel == nil || tel.LoggerProvider == nil {
		return
	}
	base := slog.NewTextHandler(local, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(tel.LogHandler(base, level)))
}

// initTelemetry starts OpenTelemetry (a no-op unless OTEL_ENABLED or
// METRICS_ENABLED is true) and routes the logs through it. Call it after the
// logger is set up and before anything that reads the global providers (the
// router, the hub, the repositories). The caller flushes it with
// attachTelemetry, or with Shutdown when the start fails.
func initTelemetry(cfg *config.Config) (*telemetry.Telemetry, error) {
	tel, err := telemetry.Init(cfg.Otel)
	if err != nil {
		return nil, err
	}
	setupOTelLogging(tel, cfg.LogLevel)
	return tel, nil
}

// registerDBMetrics exposes the connection pool of the database as metrics
// when telemetry is on. A failure is logged and the start goes on: the pool
// metrics are not worth a process that does not start. A repository without a
// database handle (a test double) is skipped.
func registerDBMetrics(cfg *config.Config, repo models.Repository) {
	if !cfg.Otel.Enabled && !cfg.Otel.MetricsEnabled {
		return
	}
	withDB, ok := repo.(interface{ DB() *gorm.DB })
	if !ok {
		slog.Debug("Repository has no database handle, pool metrics not registered")
		return
	}
	sqlDB, err := withDB.DB().DB()
	if err != nil {
		slog.Warn("Failed to read the database pool for metrics", "error", err)
		return
	}
	if err := telemetry.StartDBMetrics(sqlDB); err != nil {
		slog.Warn("Failed to register DB pool metrics", "error", err)
	}
}

// attachTelemetry makes tel.Shutdown the telemetry step of the shutdown (see
// shutdown: it runs after the services stopped, with its own budget, so the
// flush covers the shutdown itself). A nil tel keeps the no-op step.
func attachTelemetry(s *services, tel *telemetry.Telemetry) {
	if tel == nil {
		return
	}
	s.telemetryShutdown = tel.Shutdown
}

// startFailureFlushTimeout bounds the telemetry flush of a start that failed.
const startFailureFlushTimeout = 5 * time.Second

// flushTelemetry flushes tel for a process that ends because the start
// failed (the normal shutdown flushes it in its own step). The error is
// logged; the process exits either way.
func flushTelemetry(tel *telemetry.Telemetry) {
	ctx, cancel := context.WithTimeout(context.Background(), startFailureFlushTimeout)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		slog.Warn("Telemetry flush failed", "error", err)
	}
}

// routerDeps holds the dependencies of the routes.
type routerDeps struct {
	Repository    models.Repository
	HealthChecker *health.HealthChecker
	Hub           *websocket.Hub
	// InFlight tracks work that a handler starts and that outlives the
	// request (for example a detached write or a notification). buildRouter
	// passes it to the handlers that need it (none yet): such a handler
	// starts the work with InFlight.Go and does it inline when Go returns
	// false (shutdown began). Shutdown waits for it before the database
	// closes.
	InFlight *inflight.Tracker
	// Auth holds the auth stores; required with AUTH_ENABLED=true.
	Auth *routes.AuthDeps
}

// newSessionStore builds the session store of SESSION_STORE on db. Tests
// replace it.
var newSessionStore = defaultSessionStore

// defaultSessionStore returns the MySQL store (the default; it shares the
// database of the repositories) or the in-memory store. Both start a cleanup
// goroutine that Stop ends.
func defaultSessionStore(kind string, db *gorm.DB) (sessionstore.SessionStore, error) {
	switch kind {
	case "", config.SessionStoreMySQL:
		if db == nil {
			return nil, errors.New("SESSION_STORE=mysql needs the database")
		}
		return sessionstore.NewMySQL(db), nil
	case config.SessionStoreMemory:
		slog.Warn("SESSION_STORE=memory: revocations are kept in this process only; use it for one replica in development")
		return sessionstore.NewMemory(), nil
	default:
		return nil, fmt.Errorf("SESSION_STORE must be %s or %s", config.SessionStoreMySQL, config.SessionStoreMemory)
	}
}

// gormRepository is a repository that exposes its database handle
// (*models.GenericRepository).
type gormRepository interface {
	DB() *gorm.DB
}

// openAuthStores builds the repositories of the auth module and the session
// store on the database of repo (one connection pool, which repo.Close
// closes). The caller stops the session store.
func openAuthStores(cfg *config.Config, repo models.Repository) (*routes.AuthDeps, error) {
	g, ok := repo.(gormRepository)
	if !ok || g.DB() == nil {
		return nil, errors.New("auth needs the database repository")
	}
	db := g.DB()
	store, err := newSessionStore(cfg.Auth.SessionStore, db)
	if err != nil {
		return nil, err
	}
	return &routes.AuthDeps{
		Users:         database.NewGORMUserRepository(db),
		RefreshTokens: database.NewGORMRefreshTokenRepository(db),
		APIKeys:       database.NewGORMAPIKeyRepository(db),
		AuditLogs:     database.NewGORMAuditLogRepository(db),
		SessionStore:  store,
	}, nil
}

// periodicWorker returns a leader worker that runs fn at the start of each
// leadership term and then every interval, with the worker context (the end
// of the term stops a running call). An error is logged with the worker name;
// an error after the context ended (shutdown, lost leadership) is not.
func periodicWorker(name string, interval time.Duration, fn func(ctx context.Context) error) leader.Worker {
	run := func(ctx context.Context) {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			slog.Error("Leader worker run failed", "worker", name, "error", err)
		}
	}
	return leader.Worker{Name: name, Run: func(ctx context.Context) {
		run(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run(ctx)
			}
		}
	}}
}

// initDatabase opens the database, runs the migrations and returns the
// repository.
func initDatabase(cfg *config.Config) (models.Repository, error) {
	return openRepository(cfg)
}

// buildRouter sets the Gin mode (Config.GinMode), creates the Gin engine and
// registers the routes with routes.Setup (with AUTH_ENABLED=true it refuses
// to build without deps.Auth). The Swagger UI is registered only with
// ENABLE_SWAGGER=true. The caller stops the returned rate limiters and shuts
// the audit recorder down on shutdown.
func buildRouter(cfg *config.Config, deps routerDeps) (*gin.Engine, *routes.Result, error) {
	gin.SetMode(cfg.GinMode())
	// gin.New, not gin.Default: Setup registers its own Logger and Recovery
	// middleware.
	router := gin.New()
	res, err := routes.Setup(router, routes.Deps{
		Config:        cfg,
		Repository:    deps.Repository,
		HealthChecker: deps.HealthChecker,
		Hub:           deps.Hub,
		Auth:          deps.Auth,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build the router: %w", err)
	}
	if cfg.Server.EnableSwagger {
		router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
		slog.Info("Swagger UI enabled", "path", "/swagger/index.html")
	}
	return router, res, nil
}

// buildServices creates the health checker, the WebSocket hub, the router and
// the leader worker group, and starts the hub and the group. The returned
// services hold everything that shutdown stops except the HTTP server, which
// the caller adds after startHTTPServer.
//
// With AUTH_ENABLED=true it also builds the auth stores on the database of
// repo (SESSION_STORE), creates the admin user (EnsureAdminUser: an invalid
// ADMIN_PASSWORD or a failed lookup stops the start) and adds the
// refresh-token-cleanup and session-store-cleanup workers. On an error it
// stops what it started (not repo: the caller closes it) and returns it.
func buildServices(cfg *config.Config, repo models.Repository) (*services, *gin.Engine, error) {
	healthChecker := health.New()
	healthChecker.AddCheck("database", repo.Ping)
	healthChecker.SetReady(true)

	hub := websocket.NewHub()
	go hub.Run()

	// In-flight work: shared by the handlers (routerDeps.InFlight) and the
	// leader workers, which receive it when they are built below.
	tracker := inflight.New()

	var authDeps *routes.AuthDeps
	if cfg.Auth.Enabled {
		var err error
		if authDeps, err = openAuthStores(cfg, repo); err != nil {
			hub.Shutdown()
			return nil, nil, fmt.Errorf("open the auth stores: %w", err)
		}
	}
	// fail stops what was started before an error.
	fail := func(res *routes.Result, err error) (*services, *gin.Engine, error) {
		if res != nil {
			res.RateLimiters.Stop()
		}
		if authDeps != nil {
			authDeps.SessionStore.Stop()
		}
		hub.Shutdown()
		return nil, nil, err
	}

	router, res, err := buildRouter(cfg, routerDeps{
		Repository:    repo,
		HealthChecker: healthChecker,
		Hub:           hub,
		InFlight:      tracker,
		Auth:          authDeps,
	})
	if err != nil {
		return fail(nil, err)
	}

	s := &services{
		inFlight:          tracker,
		hub:               hub,
		telemetryShutdown: func(context.Context) error { return nil },
		rateLimiters:      res.RateLimiters,
		db:                repo,
	}

	// Leader-only background workers. There is no leader election yet, so
	// this process is the leader until shutdown. Pass every worker to
	// NewGroup here (give it tracker when it starts work that outlives its
	// Run), BEFORE workers.Start: Group.Add only takes effect at the next
	// term, and without an election there is no next term.
	var leaderWorkers []leader.Worker
	if cfg.Auth.Enabled {
		// Two replicas that start at once are safe: EnsureAdminUser never
		// changes an existing user and treats a duplicate key as success.
		ctx, cancel := context.WithTimeout(context.Background(), ensureAdminTimeout)
		err := res.AuthHandler.EnsureAdminUser(ctx)
		cancel()
		if err != nil {
			return fail(res, fmt.Errorf("admin user: %w", err))
		}
		leaderWorkers = append(leaderWorkers,
			periodicWorker("refresh-token-cleanup", refreshTokenCleanupInterval, res.AuthHandler.CleanupExpiredTokens),
			periodicWorker("session-store-cleanup", sessionStoreCleanupInterval, authDeps.SessionStore.Cleanup),
		)
		s.auditRecorder = res.AuditRecorder
		s.sessionStore = authDeps.SessionStore
	}
	workers := leader.NewGroup(leader.DefaultStopTimeout, leaderWorkers...)
	workers.Start(context.Background())
	s.leader = workers

	return s, router, nil
}

// startHTTPServer binds the address of cfg and serves handler in a goroutine.
// It binds before it returns, so the Addr of the returned server is the bound
// address (port 0 gets the real port). A bind or serve failure ends the
// process with exit code 1.
func startHTTPServer(cfg *config.Config, handler http.Handler) *http.Server {
	srv := &http.Server{
		Addr:         net.JoinHostPort(cfg.Server.Host, cfg.Server.Port),
		Handler:      handler,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", srv.Addr)
	if err != nil {
		slog.Error("Failed to start server", "error", err)
		startFailureHook()
		exit(1)
		return srv
	}
	// addr is the configured address (as before); listen is the bound one
	// (0.0.0.0 can show as [::], port 0 shows the real port).
	slog.Info("Server starting", "addr", srv.Addr, "listen", ln.Addr().String())
	srv.Addr = ln.Addr().String()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Failed to start server", "error", err)
			startFailureHook()
			exit(1)
		}
	}()
	return srv
}

// shutdownTimeout returns SERVER_SHUTDOWN_TIMEOUT, or defaultShutdownTimeout
// when it is 0.
func shutdownTimeout(cfg *config.Config) time.Duration {
	if cfg.Server.ShutdownTimeout > 0 {
		return cfg.Server.ShutdownTimeout
	}
	return defaultShutdownTimeout
}

// stepBudget is the budget of each shutdown step after the HTTP step: the
// time left on ctx when shutdown starts (SERVER_SHUTDOWN_TIMEOUT from main),
// at least minStepBudget. Without a deadline it is defaultShutdownTimeout.
func stepBudget(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return defaultShutdownTimeout
	}
	if d := time.Until(deadline); d > minStepBudget {
		return d
	}
	return minStepBudget
}

// shutdown stops the services in this order:
//
//  0. Metrics and pprof servers (auxServers): first, together within
//     auxShutdownTimeout (2 s), and closed when they are not done by then.
//     A pprof profile request can run for 30 s and must not take time from
//     the steps below.
//  1. HTTP servers: no new requests; wait for the open requests. A server
//     whose Shutdown fails (deadline) is closed, so its handlers do not run
//     on while the database closes.
//  2. Audit recorder (auth on): no new entries; wait for the writes of the
//     finished requests, so a graceful stop loses no entry.
//  3. Leader worker group: no new background work.
//  4. In-flight work that outlives its request (inflight.Tracker).
//  5. WebSocket hub: close the client connections; this also ends the
//     revalidation loop of the open sockets.
//  6. Telemetry: flush the traces, metrics and logs after the services
//     stopped, so it covers the shutdown.
//  7. Rate limiters: stop their cleanup goroutines.
//  8. Session store (auth on): stop its cleanup goroutine, after everything
//     that reads it (requests, workers, the hub revalidation).
//  9. Database: close the connection pool last.
//
// Budgets: the diagnostics step gets auxShutdownTimeout. The HTTP step, the
// audit step, the leader step, the in-flight step and the telemetry step each
// get their own budget (stepBudget: the time left on ctx when shutdown
// starts, at least minStepBudget), detached from ctx with ctxutil.Detach. A
// hung request that uses up its budget therefore does not starve the later
// steps, and the diagnostics step does not shorten the HTTP drain: the leader
// workers still get time to stop and the telemetry flush still gets a live
// context. The worst case is auxShutdownTimeout plus five budgets (each of
// the five steps hangs), 152 s with the default 30 s; keep the grace period
// of the orchestrator above that, or SERVER_SHUTDOWN_TIMEOUT below a fifth of
// it.
//
// Every step runs even when an earlier step failed. shutdown returns the
// errors of all steps; a step that ran out of time wraps
// context.DeadlineExceeded.
func shutdown(ctx context.Context, s *services) error {
	budget := stepBudget(ctx) // measured before any step uses ctx up
	var errs []error

	if len(s.auxServers) > 0 {
		auxCtx, cancel := ctxutil.Detach(ctx, auxShutdownTimeout)
		for _, srv := range s.auxServers {
			if err := srv.Shutdown(auxCtx); err != nil {
				errs = append(errs, fmt.Errorf("diagnostics server: %w", err))
				if cerr := srv.Close(); cerr != nil {
					errs = append(errs, fmt.Errorf("close diagnostics server: %w", cerr))
				}
			}
		}
		cancel()
	}

	// The HTTP step has a budget of its own too: the diagnostics step above
	// used up to auxShutdownTimeout of the wall clock, which ctx (a deadline)
	// would have lost the drain.
	httpCtx, cancelHTTP := ctxutil.Detach(ctx, budget)
	for _, srv := range s.servers {
		if err := srv.Shutdown(httpCtx); err != nil {
			errs = append(errs, fmt.Errorf("http server: %w", err))
			if cerr := srv.Close(); cerr != nil {
				errs = append(errs, fmt.Errorf("close http server: %w", cerr))
			}
		}
	}
	cancelHTTP()

	if !isNilInterface(s.auditRecorder) {
		auditCtx, cancel := ctxutil.Detach(ctx, budget)
		err := s.auditRecorder.Shutdown(auditCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("audit log writes did not finish: %w", err))
		}
	}

	if s.leader != nil && !s.leader.Close(budget) {
		errs = append(errs, fmt.Errorf("leader workers did not stop: %w", context.DeadlineExceeded))
	}

	if !isNilInterface(s.inFlight) {
		waitCtx, cancel := ctxutil.Detach(ctx, budget)
		err := s.inFlight.Wait(waitCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("in-flight work did not finish: %w", err))
		}
	}

	if s.hub != nil {
		s.hub.Shutdown()
	}

	if s.telemetryShutdown != nil {
		telCtx, cancel := ctxutil.Detach(ctx, budget)
		err := s.telemetryShutdown(telCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("telemetry: %w", err))
		}
	}

	if s.rateLimiters != nil {
		s.rateLimiters.Stop()
	}

	if !isNilInterface(s.sessionStore) {
		s.sessionStore.Stop()
	}

	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close repository: %w", err))
		}
	}

	return errors.Join(errs...)
}

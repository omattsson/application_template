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
	"sync"
	"time"

	_ "backend/docs" // registers the Swagger spec served by /swagger/*any
	"backend/internal/api/routes"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/leader"
	"backend/internal/models"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// defaultShutdownTimeout is the shutdown budget when SERVER_SHUTDOWN_TIMEOUT
// is 0.
const defaultShutdownTimeout = 5 * time.Second

// openRepository opens the database and runs the migrations. Tests replace it.
var openRepository = database.NewRepository

// exit ends the process. Tests replace it.
var exit = os.Exit

// The interfaces below are the parts of each service that shutdown uses.
// Tests pass fakes to check the order.

// serverShutdowner is an HTTP server (*http.Server).
type serverShutdowner interface {
	Shutdown(ctx context.Context) error
}

// groupCloser is the leader worker group (*leader.Group).
type groupCloser interface {
	Close(timeout time.Duration) bool
}

// waiter is the in-flight work (*sync.WaitGroup).
type waiter interface {
	Wait()
}

// hubShutdowner is the WebSocket hub (*websocket.Hub).
type hubShutdowner interface {
	Shutdown()
}

// stopper is the rate limiters (*routes.RateLimiters).
type stopper interface {
	Stop()
}

// closer is the data layer (models.Repository).
type closer interface {
	Close() error
}

// services holds what shutdown stops. A nil field is skipped.
type services struct {
	// leader runs the leader-only background workers.
	leader groupCloser
	// inFlight is the work that outlives its request (later tasks add to it:
	// Add before the work starts, Done when it ends).
	inFlight waiter
	hub      hubShutdowner
	// telemetryShutdown flushes the telemetry. A no-op until telemetry
	// exists.
	telemetryShutdown func(ctx context.Context) error
	rateLimiters      stopper
	db                closer
	servers           []serverShutdowner
}

// setupLoggingTo makes a slog text handler on w, filtered at level, the
// default logger. It is a new text handler, not a wrapper of the previous
// default handler: that handler writes through the log package, and after
// slog.SetDefault the log package writes through the default logger again,
// which deadlocks.
func setupLoggingTo(w io.Writer, level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
}

// routerDeps holds the dependencies of the routes.
type routerDeps struct {
	Repository    models.Repository
	HealthChecker *health.HealthChecker
	Hub           *websocket.Hub
}

// initDatabase opens the database, runs the migrations and returns the
// repository.
func initDatabase(cfg *config.Config) (models.Repository, error) {
	return openRepository(cfg)
}

// buildRouter sets the Gin mode (Config.GinMode), creates the Gin engine and
// registers the routes. The Swagger UI is registered only with
// ENABLE_SWAGGER=true. The caller stops the returned rate limiters on
// shutdown.
func buildRouter(cfg *config.Config, deps routerDeps) (*gin.Engine, *routes.RateLimiters) {
	gin.SetMode(cfg.GinMode())
	// gin.New, not gin.Default: SetupRoutes registers its own Logger and
	// Recovery middleware.
	router := gin.New()
	rateLimiters := routes.SetupRoutes(router, deps.Repository, deps.HealthChecker, cfg, deps.Hub)
	if cfg.Server.EnableSwagger {
		router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
		slog.Info("Swagger UI enabled", "path", "/swagger/index.html")
	}
	return router, rateLimiters
}

// buildServices creates the health checker, the WebSocket hub, the router and
// the leader worker group, and starts the hub and the group. The returned
// services hold everything that shutdown stops except the HTTP server, which
// the caller adds after startHTTPServer.
func buildServices(cfg *config.Config, repo models.Repository) (*services, *gin.Engine) {
	healthChecker := health.New()
	healthChecker.AddCheck("database", repo.Ping)
	healthChecker.SetReady(true)

	hub := websocket.NewHub()
	go hub.Run()

	router, rateLimiters := buildRouter(cfg, routerDeps{
		Repository:    repo,
		HealthChecker: healthChecker,
		Hub:           hub,
	})

	// Leader-only background workers. There is no leader election yet, so
	// this process is the leader until shutdown. The group has no workers
	// yet; later tasks add them before Start.
	workers := leader.NewGroup(leader.DefaultStopTimeout)
	workers.Start(context.Background())

	return &services{
		leader:            workers,
		inFlight:          &sync.WaitGroup{},
		hub:               hub,
		telemetryShutdown: func(context.Context) error { return nil },
		rateLimiters:      rateLimiters,
		db:                repo,
	}, router
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
		exit(1)
		return srv
	}
	srv.Addr = ln.Addr().String()

	slog.Info("Server starting", "addr", srv.Addr)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Failed to start server", "error", err)
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

// shutdown stops the services in this order:
//
//  1. HTTP servers: no new requests; wait for the open requests.
//  2. Leader worker group: no new background work.
//  3. In-flight work that outlives its request.
//  4. WebSocket hub: close the client connections.
//  5. Telemetry: flush after the services stopped, so it covers the shutdown.
//  6. Rate limiters: stop their cleanup goroutines.
//  7. Database: close the connection pool last.
//
// Every step runs even when an earlier step failed or ctx expired: a hung
// step waits at most until the deadline of ctx. shutdown returns the errors
// of all steps; an expired deadline wraps context.DeadlineExceeded.
func shutdown(ctx context.Context, s *services) error {
	var errs []error

	for _, srv := range s.servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("http server: %w", err))
		}
	}

	if s.leader != nil && !s.leader.Close(remaining(ctx)) {
		errs = append(errs, fmt.Errorf("leader workers did not stop: %w", context.DeadlineExceeded))
	}

	if s.inFlight != nil {
		if err := waitContext(ctx, s.inFlight); err != nil {
			errs = append(errs, fmt.Errorf("in-flight work did not finish: %w", err))
		}
	}

	if s.hub != nil {
		s.hub.Shutdown()
	}

	if s.telemetryShutdown != nil {
		if err := s.telemetryShutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("telemetry: %w", err))
		}
	}

	if s.rateLimiters != nil {
		s.rateLimiters.Stop()
	}

	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close repository: %w", err))
		}
	}

	return errors.Join(errs...)
}

// remaining returns the time until the deadline of ctx: the stop timeout of
// the leader group. Without a deadline it is leader.DefaultStopTimeout. It is
// never <= 0, because Group.Close treats that as "use the default".
func remaining(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return leader.DefaultStopTimeout
	}
	if d := time.Until(deadline); d > 0 {
		return d
	}
	return time.Nanosecond
}

// waitContext waits for w until ctx ends. On timeout the waiting goroutine
// stays until w is done; the process is exiting anyway.
func waitContext(ctx context.Context, w waiter) error {
	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Work that finished at the deadline is not a timeout.
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

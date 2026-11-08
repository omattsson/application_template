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
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// minStepBudget is the smallest budget of a shutdown step.
const minStepBudget = 100 * time.Millisecond

// defaultShutdownTimeout is the shutdown budget when SERVER_SHUTDOWN_TIMEOUT
// is 0.
const defaultShutdownTimeout = 5 * time.Second

// openRepository opens the database and runs the migrations. Tests replace it.
var openRepository = database.NewRepository

// exit ends the process. Tests replace it.
var exit = os.Exit

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
	// inFlight is the work that outlives its request or worker run
	// (*inflight.Tracker; see routerDeps.InFlight).
	inFlight waiter
	hub      hubShutdowner
	// telemetryShutdown flushes the telemetry. A no-op until telemetry
	// exists.
	telemetryShutdown func(ctx context.Context) error
	rateLimiters      stopper
	db                closer
	servers           []httpServer
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
	// InFlight tracks work that a handler starts and that outlives the
	// request (for example a detached write or a notification). buildRouter
	// passes it to the handlers that need it (none yet): such a handler
	// starts the work with InFlight.Go and does it inline when Go returns
	// false (shutdown began). Shutdown waits for it before the database
	// closes.
	InFlight *inflight.Tracker
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

	// In-flight work: shared by the handlers (routerDeps.InFlight) and the
	// leader workers, which receive it when they are built below.
	tracker := inflight.New()

	router, rateLimiters := buildRouter(cfg, routerDeps{
		Repository:    repo,
		HealthChecker: healthChecker,
		Hub:           hub,
		InFlight:      tracker,
	})

	// Leader-only background workers. There is no leader election yet, so
	// this process is the leader until shutdown. Pass every worker to
	// NewGroup here (give it tracker when it starts work that outlives its
	// Run), BEFORE workers.Start: Group.Add only takes effect at the next
	// term, and without an election there is no next term.
	workers := leader.NewGroup(leader.DefaultStopTimeout)
	workers.Start(context.Background())

	return &services{
		leader:            workers,
		inFlight:          tracker,
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
	// addr is the configured address (as before); listen is the bound one
	// (0.0.0.0 can show as [::], port 0 shows the real port).
	slog.Info("Server starting", "addr", srv.Addr, "listen", ln.Addr().String())
	srv.Addr = ln.Addr().String()

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
//  1. HTTP servers: no new requests; wait for the open requests. A server
//     whose Shutdown fails (deadline) is closed, so its handlers do not run
//     on while the database closes.
//  2. Leader worker group: no new background work.
//  3. In-flight work that outlives its request (inflight.Tracker).
//  4. WebSocket hub: close the client connections.
//  5. Telemetry: flush after the services stopped, so it covers the shutdown.
//  6. Rate limiters: stop their cleanup goroutines.
//  7. Database: close the connection pool last.
//
// Budgets: the HTTP step uses ctx. The leader step, the in-flight step and
// the telemetry step each get their own budget (stepBudget: the time left on
// ctx when shutdown starts, at least minStepBudget), detached from ctx with
// ctxutil.Detach. A hung request that uses up ctx therefore does not starve
// them: the leader workers still get time to stop and the telemetry flush
// still gets a live context. The worst case is about four budgets (each of
// the four steps hangs); keep the grace period of the orchestrator above
// that, or SERVER_SHUTDOWN_TIMEOUT below a quarter of it.
//
// Every step runs even when an earlier step failed. shutdown returns the
// errors of all steps; a step that ran out of time wraps
// context.DeadlineExceeded.
func shutdown(ctx context.Context, s *services) error {
	budget := stepBudget(ctx) // measured before the HTTP step uses ctx up
	var errs []error

	for _, srv := range s.servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("http server: %w", err))
			if cerr := srv.Close(); cerr != nil {
				errs = append(errs, fmt.Errorf("close http server: %w", cerr))
			}
		}
	}

	if s.leader != nil && !s.leader.Close(budget) {
		errs = append(errs, fmt.Errorf("leader workers did not stop: %w", context.DeadlineExceeded))
	}

	if s.inFlight != nil {
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

	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close repository: %w", err))
		}
	}

	return errors.Join(errs...)
}

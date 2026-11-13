package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	// Importing net/http/pprof registers its handlers on http.DefaultServeMux
	// (in init). A server of this process must therefore NEVER be started with
	// a nil Handler: it would serve pprof on its own port. Every server here
	// has an explicit mux; TestEveryServerHasAnExplicitHandler checks it.
	"net/http/pprof"
	"runtime"
	"time"

	"backend/internal/config"
	"backend/internal/telemetry"
)

// auxShutdownTimeout is the budget of the metrics and pprof servers in
// shutdown, all of them together. They stop before the API drains: a pprof
// profile request can run for 30 s and must not take time from the steps
// that matter, so a server that is still busy after this long is closed.
// Tests replace it.
var auxShutdownTimeout = 2 * time.Second

// Timeouts of the diagnostics servers.
const (
	auxReadHeaderTimeout = 5 * time.Second
	auxReadTimeout       = 10 * time.Second
	auxIdleTimeout       = 30 * time.Second
	// metricsWriteTimeout bounds one scrape.
	metricsWriteTimeout = 10 * time.Second
	// pprofWriteTimeout is above the 30 s of a default CPU profile and
	// trace, so a profile of a few minutes still completes.
	pprofWriteTimeout = 5 * time.Minute
)

// startMetricsServer serves the Prometheus metrics (GET /metrics) on
// METRICS_ADDR, a port of its own: the API router never serves them. It
// returns nil, and starts nothing, unless METRICS_ENABLED=true and tel has a
// handler. The endpoint has no authentication: keep the port on a private
// network. A bind failure is logged and returns nil; the API runs on without
// metrics.
func startMetricsServer(cfg *config.Config, tel *telemetry.Telemetry) *http.Server {
	if !cfg.Otel.MetricsEnabled || tel == nil || tel.MetricsHandler == nil {
		return nil
	}

	noteMetricsAddr(cfg.Otel.MetricsAddr)

	mux := http.NewServeMux()
	mux.Handle("/metrics", tel.MetricsHandler)

	return serveOptional(&http.Server{
		Addr:              cfg.Otel.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: auxReadHeaderTimeout,
		ReadTimeout:       auxReadTimeout,
		WriteTimeout:      metricsWriteTimeout,
		IdleTimeout:       auxIdleTimeout,
	}, "metrics server")
}

// noteMetricsAddr reminds the operator, at Info, that /metrics has no
// authentication when its address is not loopback. It is an Info and not a
// Warn on purpose: the default :9090 is not loopback either, and the Compose
// setup uses it on its private network, so a Warn would fire on every start.
func noteMetricsAddr(addr string) {
	if config.IsLoopbackAddr(addr) {
		return
	}
	slog.Info("metrics endpoint is unauthenticated: keep this port on a private network and never publish it to the internet",
		"addr", addr)
}

// checkPprofAddr enforces the pprof address policy again at start (LoadConfig
// has checked it; a hand-built config skips LoadConfig): a heap or goroutine
// dump exposes secrets, so an address that is not loopback is refused unless
// PPROF_ALLOW_NON_LOOPBACK=true, and then it is a prominent warning.
func checkPprofAddr(cfg *config.Config) error {
	if config.IsLoopbackAddr(cfg.Server.PprofAddr) {
		return nil
	}
	if !cfg.Server.PprofAllowNonLoopback {
		return fmt.Errorf("PPROF_ADDR %q is not loopback and PPROF_ALLOW_NON_LOOPBACK is not true", cfg.Server.PprofAddr)
	}
	slog.Warn("pprof is reachable beyond loopback (PPROF_ALLOW_NON_LOOPBACK=true): heap and goroutine dumps expose secrets; keep this port on a private network",
		"addr", cfg.Server.PprofAddr)

	return nil
}

// startPprofServer serves net/http/pprof on PPROF_ADDR (loopback by default),
// a port of its own, never the API port. It returns nil, and starts nothing,
// unless PPROF_ENABLED=true. It sets the mutex and block profile rates
// (PPROF_MUTEX_FRACTION and PPROF_BLOCK_RATE, 0 is off). A refused address or
// a bind failure is logged and returns nil.
func startPprofServer(cfg *config.Config) *http.Server {
	if !cfg.Server.PprofEnabled {
		return nil
	}
	if err := checkPprofAddr(cfg); err != nil {
		slog.Error("pprof server not started", "error", err)
		return nil
	}

	runtime.SetMutexProfileFraction(cfg.Server.PprofMutexFraction)
	runtime.SetBlockProfileRate(cfg.Server.PprofBlockRate)
	slog.Info("pprof profiles configured",
		"mutex_fraction", cfg.Server.PprofMutexFraction, "block_rate_ns", cfg.Server.PprofBlockRate)

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index) // also the named profiles (heap, goroutine, ...)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return serveOptional(&http.Server{
		Addr:              cfg.Server.PprofAddr,
		Handler:           mux,
		ReadHeaderTimeout: auxReadHeaderTimeout,
		ReadTimeout:       auxReadTimeout,
		WriteTimeout:      pprofWriteTimeout,
		IdleTimeout:       auxIdleTimeout,
	}, "pprof server")
}

// serveOptional binds srv.Addr and serves it in a goroutine that ends when
// the server is closed. It binds before it returns, so srv.Addr is the bound
// address (port 0 gets the real port). A bind failure is logged at Error and
// returns nil: a diagnostics server is not worth a process that does not
// start. A later serve error is logged.
func serveOptional(srv *http.Server, name string) *http.Server {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", srv.Addr)
	if err != nil {
		slog.Error(name+" failed", "addr", srv.Addr, "error", err)
		return nil
	}
	srv.Addr = ln.Addr().String()
	slog.Info(name+" starting", "addr", srv.Addr)
	serveListener(srv, ln, name)

	return srv
}

// serveListener serves srv on ln in a goroutine that ends when the server is
// closed or Serve fails (logged at Error). The returned channel is closed
// when the goroutine ends.
func serveListener(srv *http.Server, ln net.Listener, name string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error(name+" failed", "error", err)
		}
	}()

	return done
}

// addAuxServers registers the diagnostics servers for shutdown. A nil server
// (disabled, or not started) is skipped.
func addAuxServers(s *services, srvs ...*http.Server) {
	for _, srv := range srvs {
		if srv != nil {
			s.auxServers = append(s.auxServers, srv)
		}
	}
}

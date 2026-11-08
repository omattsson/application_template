// Package main provides the entry point for the backend API service.
// It loads the configuration, builds the services (bootstrap.go), serves HTTP
// and shuts the services down in order on SIGINT or SIGTERM.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"backend/internal/config"
)

// @title           Backend API
// @version         1.0
// @description     This is the API documentation for the backend service
// @host            localhost:8081
// @BasePath        /
// @schemes         http https
// @produce         json
// @consumes        json
func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}
	// LOG_LEVEL (LoadConfig already rejected an invalid value).
	setupLoggingTo(os.Stderr, cfg.LogLevel)

	repo, err := initDatabase(cfg)
	if err != nil {
		slog.Error("Failed to initialize repository", "error", err)
		os.Exit(1)
	}

	s, router := buildServices(cfg, repo)
	s.servers = append(s.servers, startHTTPServer(cfg, router))

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout(cfg))
	defer cancel()
	if err := shutdown(ctx, s); err != nil {
		slog.Error("Server forced to shutdown", "error", err)
		return
	}
	slog.Info("Server exited gracefully")
}

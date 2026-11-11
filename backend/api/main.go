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
//
// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                With AUTH_ENABLED=true: "Bearer <access token>" from POST /api/v1/auth/login or /api/v1/auth/refresh. Not used with AUTH_ENABLED=false.
//
// @securityDefinitions.apikey ApiKeyAuth
// @in                         header
// @name                       X-API-Key
// @description                With AUTH_ENABLED=true: an API key (sk_ and 64 hex characters) from POST /api/v1/api-keys. Not accepted on the key, user and audit log management routes or on logout-all.
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

	s, router, err := buildServices(cfg, repo)
	if err != nil {
		// No secret in err: the config and auth errors name the variable only.
		slog.Error("Failed to start the services", "error", err)
		_ = repo.Close()
		os.Exit(1)
	}
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

// Package main provides the entry point for the backend API service.
// It handles configuration loading, database setup, and HTTP server initialization.
package main

import (
	_ "backend/docs"
	"backend/internal/api/routes"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

const (
	gracefulShutdownTimeout = 5 * time.Second
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
	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Initialize repository using the factory (selects MySQL or Azure Table based on config)
	repo, err := database.NewRepository(cfg)
	if err != nil {
		slog.Error("Failed to initialize repository", "error", err)
		os.Exit(1)
	}

	// Initialize health checker with actual database dependency
	healthChecker := health.New()
	healthChecker.AddCheck("database", func(ctx context.Context) error {
		return repo.Ping(ctx)
	})
	healthChecker.SetReady(true)

	// Setup router
	router := gin.Default()
	routes.SetupRoutes(router, repo, healthChecker, cfg)
	router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Create server with timeouts
	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", cfg.Server.Host, cfg.Server.Port),
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Start server in a goroutine
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Failed to start server", "error", err)
			os.Exit(1)
		}
	}()

	slog.Info("Server started", "addr", srv.Addr)

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down server...")

	// Give outstanding requests time to complete
	ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancel()

	err = srv.Shutdown(ctx)
	cancel()

	if err != nil {
		slog.Error("Server forced to shutdown", "error", err)
		return
	}

	slog.Info("Server exited gracefully")
}

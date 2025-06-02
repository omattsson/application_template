package main

import (
	_ "backend/docs" // This will be generated
	"backend/internal/api/routes"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// @title           Backend API
// @version         1.0
// @description     This is the API documentation for the backend service
// @host            localhost:8081
// @BasePath        /

func main() {
	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize database
	db, err := database.NewFromAppConfig(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Run database migrations
	if err := db.AutoMigrate(); err != nil {
		log.Fatalf("Failed to run database migrations: %v", err)
	}

	// Create repository based on configuration
	repository, err := database.NewRepository(cfg)
	if err != nil {
		log.Fatalf("Failed to create repository: %v", err)
	}

	r := gin.Default()

	// Initialize health checker
	healthChecker := health.NewHealthChecker()

	// Add database health check
	if cfg.AzureTable.UseAzureTable {
		healthChecker.AddCheck("database", func() error {
			// For Azure Table Storage, we'll check if we can list tables
			return repository.Ping()
		})
	} else {
		healthChecker.AddCheck("database", func() error {
			sqlDB, err := db.DB.DB()
			if err != nil {
				return err
			}
			return sqlDB.Ping()
		})
	}

	// Mark the service as ready after initialization
	healthChecker.SetReady(true)

	// Swagger documentation endpoint
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Set up routes with repository
	routes.SetupRoutes(r, repository)

	// Configure server address
	addr := fmt.Sprintf("%s:%s", cfg.Server.Host, cfg.Server.Port)

	// Configure server timeouts
	server := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Start server
	log.Printf("Starting server on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server failed to start: %v", err)
	}
}

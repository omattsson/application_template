package main

import (
	_ "backend/docs" // This will be generated
	"backend/internal/api/routes"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/models"
	"log"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// @title           Backend API
// @version         1.0
// @description     This is the API documentation for the backend service
// @host            localhost:8080
// @BasePath        /api/v1

func main() {
	// Load configuration
	cfg := config.LoadConfig()

	// Initialize database
	dbConfig := database.NewConfig()
	db, err := database.NewDatabase(dbConfig)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Run database migrations
	if err := db.AutoMigrate(); err != nil {
		log.Fatalf("Failed to run database migrations: %v", err)
	}

	// Create repository
	repository := models.NewRepository(db.DB)

	r := gin.Default()

	// Initialize health checker
	healthChecker := health.NewHealthChecker()

	// Add database health check
	healthChecker.AddCheck("database", func() error {
		sqlDB, err := db.DB.DB()
		if err != nil {
			return err
		}
		return sqlDB.Ping()
	})

	// Mark the service as ready after initialization
	healthChecker.SetReady(true)

	// Swagger documentation endpoint
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Set up routes with repository
	routes.SetupRoutes(r, repository)

	// Start the server
	log.Printf("Starting server on port %s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

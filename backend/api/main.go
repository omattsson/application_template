package main

import (
	_ "backend/docs" // This will be generated
	"backend/internal/api/routes"
	"backend/internal/config"
	"backend/internal/health"

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
	r := gin.Default()

	// Initialize health checker
	healthChecker := health.NewHealthChecker()

	// Add example checks if needed
	healthChecker.AddCheck("database", func() error {
		// Add your database check here
		return nil
	})

	// Mark the service as ready after initialization
	healthChecker.SetReady(true)

	// Swagger documentation endpoint
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Set up routes
	routes.SetupRoutes(r)

	// Load configuration
	cfg := config.LoadConfig()

	// Start the server
	if err := r.Run(":" + cfg.Port); err != nil {
		panic(err)
	}
}

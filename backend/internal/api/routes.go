package api

import (
	"backend/internal/api/handlers"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// SetupRouter initializes the router and configures all routes
func SetupRouter() *gin.Engine {
	r := gin.Default()

	// Health check endpoints
	r.GET("/health/live", handlers.LivenessCheck)
	r.GET("/health/ready", handlers.ReadinessCheck)

	// Swagger documentation
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// API v1 routes
	{
		// Add your API routes here
	}

	return r
}

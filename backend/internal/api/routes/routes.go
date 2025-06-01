package routes

import (
	"backend/internal/api/handlers"
	"backend/internal/api/middleware"

	"github.com/gin-gonic/gin"
)

// SetupRoutes configures all the routes for our application
func SetupRoutes(router *gin.Engine) {
	// Add middleware
	router.Use(middleware.Logger())
	router.Use(middleware.Recovery())
	router.Use(middleware.CORS())

	// Health check endpoints
	health := router.Group("/health")
	{
		health.GET("/live", handlers.LivenessCheck)
		health.GET("/ready", handlers.ReadinessCheck)
		health.GET("", handlers.HealthCheck) // Keep the original health check for backward compatibility
	}

	// API v1 routes
	v1 := router.Group("/api/v1")
	{
		// Ping endpoint
		v1.GET("/ping", handlers.Ping)

		// Items endpoints
		items := v1.Group("/items")
		{
			items.GET("", handlers.GetItems)
			items.POST("", handlers.CreateItem)
		}
	}
}

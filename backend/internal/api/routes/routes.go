package routes

import (
	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// SetupRoutes configures all the routes for our application
func SetupRoutes(router *gin.Engine, repository models.Repository) {
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
		itemsHandler := handlers.NewHandler(repository)
		items := v1.Group("/items")
		{
			items.GET("", itemsHandler.GetItems)
			items.GET("/:id", itemsHandler.GetItem)
			items.POST("", itemsHandler.CreateItem)
			items.PUT("/:id", itemsHandler.UpdateItem)
			items.DELETE("/:id", itemsHandler.DeleteItem)
		}
	}
}

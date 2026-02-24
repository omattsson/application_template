package routes

import (
	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// SetupRoutes configures all the routes for our application.
// healthChecker is injected from main so the readiness endpoint reflects real dependency health.
func SetupRoutes(router *gin.Engine, repository models.Repository, healthChecker *health.HealthChecker, cfg *config.Config) {
	// Add middleware
	router.Use(middleware.RequestID())
	router.Use(middleware.Logger())
	router.Use(middleware.Recovery())
	router.Use(middleware.CORS(cfg.CORS.AllowedOrigins))
	router.Use(middleware.MaxBodySize(1 << 20)) // 1 MB default

	// Health check endpoints
	healthGroup := router.Group("/health")
	{
		healthGroup.GET("/live", handlers.LivenessHandler(healthChecker))
		healthGroup.GET("/ready", handlers.ReadinessHandler(healthChecker))
		healthGroup.GET("", handlers.HealthCheck) // Keep the original health check for backward compatibility
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

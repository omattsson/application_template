package routes

import (
	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/websocket"
	"time"

	"github.com/gin-gonic/gin"
)

// SetupRoutes configures all the routes for our application.
// healthChecker is injected from main so the readiness endpoint reflects real dependency health.
// Returns the rate limiter so the caller can stop it during shutdown.
func SetupRoutes(router *gin.Engine, repository models.Repository, healthChecker *health.HealthChecker, cfg *config.Config, hub *websocket.Hub) *handlers.RateLimiter {
	// Add middleware
	router.Use(middleware.RequestID())
	router.Use(middleware.Logger())
	router.Use(middleware.Recovery())
	router.Use(middleware.CORS(cfg.CORS.AllowedOrigins))
	router.Use(middleware.MaxBodySize(1 << 20)) // 1 MB default

	// WebSocket endpoint (top-level, outside rate limiter — connections are long-lived)
	wsHandler := handlers.NewWebSocketHandler(hub, cfg.CORS.AllowedOrigins)
	router.GET("/ws", wsHandler.HandleWebSocket)

	// Health check endpoints
	healthGroup := router.Group("/health")
	{
		healthGroup.GET("/live", handlers.LivenessHandler(healthChecker))
		healthGroup.GET("/ready", handlers.ReadinessHandler(healthChecker))
		healthGroup.GET("", handlers.HealthCheck) // Keep the original health check for backward compatibility
	}

	// Rate limiter for API routes
	rateLimiter := handlers.NewRateLimiter(100, time.Minute)

	// API v1 routes
	v1 := router.Group("/api/v1")
	v1.Use(rateLimiter.RateLimit())
	{
		// Ping endpoint
		v1.GET("/ping", handlers.Ping)

		// Items endpoints
		itemsHandler := handlers.NewHandlerWithHub(repository, hub)
		items := v1.Group("/items")
		{
			items.GET("", itemsHandler.GetItems)
			items.GET("/:id", itemsHandler.GetItem)
			items.POST("", itemsHandler.CreateItem)
			items.PUT("/:id", itemsHandler.UpdateItem)
			items.DELETE("/:id", itemsHandler.DeleteItem)
		}
	}

	return rateLimiter
}

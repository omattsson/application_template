package routes

import (
	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/websocket"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimiters groups the rate limiters created by SetupRoutes.
type RateLimiters struct {
	API   *handlers.RateLimiter
	Login *handlers.RateLimiter
}

// Stop terminates the background cleanup goroutines.
func (r *RateLimiters) Stop() {
	if r == nil {
		return
	}
	if r.API != nil {
		r.API.Stop()
	}
	if r.Login != nil {
		r.Login.Stop()
	}
}

// SetupRoutes configures all the routes for our application.
// healthChecker is injected from main so the readiness endpoint reflects real dependency health.
// Returns the rate limiters so the caller can stop them during shutdown.
func SetupRoutes(router *gin.Engine, repository models.Repository, healthChecker *health.HealthChecker, cfg *config.Config, hub *websocket.Hub) *RateLimiters {
	// Client IP: only the proxies of TRUSTED_PROXIES may set it with
	// X-Forwarded-For or X-Real-IP. Gin trusts every peer by default, so a
	// client could pick a new IP for each request and pass the per-IP rate
	// limits. A nil list trusts no proxy: the client IP is the TCP peer.
	// config.ParseTrustedProxies validated the list; on an error, trust no
	// proxy.
	trustedProxies := cfg.Server.TrustedProxies
	if len(trustedProxies) == 0 {
		trustedProxies = nil
	}
	if err := router.SetTrustedProxies(trustedProxies); err != nil {
		slog.Error("Invalid TRUSTED_PROXIES; no proxy is trusted", "error", err)
		_ = router.SetTrustedProxies(nil)
	}

	// Add middleware. RedactWSToken runs before Logger (and any tracer) so
	// the WebSocket token in the query string is never recorded.
	router.Use(middleware.RequestID())
	router.Use(middleware.RedactWSToken())
	router.Use(middleware.Logger())
	router.Use(middleware.Recovery())
	router.Use(middleware.SecurityHeaders())
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

	// Rate limiter for API routes: RATE_LIMIT requests per minute per client IP.
	rateLimiter := handlers.NewRateLimiter(int(cfg.Server.RateLimit), time.Minute)
	// Login limiter: LOGIN_RATE_LIMIT attempts per minute per client IP. The
	// login route attaches it with Login.RateLimit().
	loginRateLimiter := handlers.NewRateLimiter(int(cfg.Server.LoginRateLimit), time.Minute)

	// API v1 routes
	v1 := router.Group("/api/v1")
	v1.Use(rateLimiter.RateLimit())
	{
		// Ping endpoint
		v1.GET("/ping", handlers.Ping)

		// Public UI config (no auth, so the login page can read it)
		v1.GET("/ui-config", handlers.NewUIConfigHandler(cfg.App.Title, cfg.Auth.Enabled))

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

	return &RateLimiters{API: rateLimiter, Login: loginRateLimiter}
}

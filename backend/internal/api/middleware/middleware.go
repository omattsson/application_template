package middleware

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// CORS middleware with configurable allowed origins.
// Pass "*" or "" to allow all origins (development only).
// For production, pass a comma-separated list of allowed origins.
func CORS(allowedOrigins string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if allowedOrigins == "" || allowedOrigins == "*" {
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			requestOrigin := c.Request.Header.Get("Origin")
			allowed := false
			for _, origin := range strings.Split(allowedOrigins, ",") {
				if strings.TrimSpace(origin) == requestOrigin {
					c.Writer.Header().Set("Access-Control-Allow-Origin", requestOrigin)
					c.Writer.Header().Set("Vary", "Origin")
					allowed = true
					break
				}
			}
			if !allowed {
				// Block requests from non-whitelisted origins as defense-in-depth;
				// browsers enforce CORS client-side, but we also enforce server-side.
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
		}
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, Authorization, X-Request-ID")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// Logger is a middleware that logs incoming requests using structured logging.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		slog.Info("incoming request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
		)
		c.Next()
	}
}

// Recovery is a middleware that recovers from any panics and writes a 500 if there was one.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("recovered from panic", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal Server Error"})
				c.Abort()
			}
		}()
		c.Next()
	}
}

// RequestID adds a unique request ID to each request.
// If the client sends an X-Request-ID header, it is reused; otherwise a new one is generated.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}
		c.Set("request_id", requestID)
		c.Writer.Header().Set("X-Request-ID", requestID)
		c.Next()
	}
}

// MaxBodySize limits the size of the request body to prevent memory exhaustion.
func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

func generateRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use timestamp-based ID if crypto/rand fails
		slog.Warn("failed to generate random request ID, using fallback", "error", err)
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

package handlers

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimiter implements a simple rate limiting middleware
type RateLimiter struct {
	sync.RWMutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

func (rl *RateLimiter) RateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()

		rl.Lock()
		defer rl.Unlock()

		now := time.Now()
		windowStart := now.Add(-rl.window)

		// Remove old requests
		if _, exists := rl.requests[ip]; exists {
			var valid []time.Time
			for _, t := range rl.requests[ip] {
				if t.After(windowStart) {
					valid = append(valid, t)
				}
			}
			rl.requests[ip] = valid
		}

		// Check if limit exceeded
		if len(rl.requests[ip]) >= rl.limit {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			c.Abort()
			return
		}

		// Add current request
		rl.requests[ip] = append(rl.requests[ip], now)
		c.Next()
	}
}

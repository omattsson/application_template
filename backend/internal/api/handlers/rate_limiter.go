package handlers

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimiter implements a simple rate limiting middleware
type RateLimiter struct {
	sync.RWMutex                        // size: 8
	window       time.Duration          // size: 8
	requests     map[string][]time.Time // size: 8 (pointer)
	done         chan struct{}          // size: 8
	limit        int                    // size: 4
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
		done:     make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

// Stop terminates the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.done)
}

func (rl *RateLimiter) RateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()

		rl.Lock()
		defer rl.Unlock()

		now := time.Now()
		windowStart := now.Add(-rl.window)

		// Initialize requests map for this IP if needed
		if _, exists := rl.requests[ip]; !exists {
			rl.requests[ip] = make([]time.Time, 0)
		}

		// Remove old requests (outside our time window)
		var valid []time.Time
		for _, t := range rl.requests[ip] {
			if t.After(windowStart) {
				valid = append(valid, t)
			}
		}
		rl.requests[ip] = valid

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

// cleanup periodically removes expired entries to prevent memory leaks.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.window * 2)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.cleanupExpired()
		case <-rl.done:
			return
		}
	}
}

func (rl *RateLimiter) cleanupExpired() {
	rl.Lock()
	defer rl.Unlock()
	now := time.Now()
	for ip, times := range rl.requests {
		var valid []time.Time
		for _, t := range times {
			if now.Sub(t) < rl.window {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(rl.requests, ip)
		} else {
			rl.requests[ip] = valid
		}
	}
}

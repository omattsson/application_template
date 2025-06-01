package handlers

import (
	"backend/internal/health"
	"net/http"

	"github.com/gin-gonic/gin"
)

var healthChecker *health.HealthChecker

func init() {
	healthChecker = health.NewHealthChecker()
	// Set the service as ready after initialization
	healthChecker.SetReady(true)
}

// @Summary     Health Check
// @Description Get API health status
// @Tags        health
// @Produce     json
// @Success     200 {object} map[string]string
// @Router      /health [get]
func HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// @Summary     Ping test
// @Description Ping test endpoint
// @Tags        ping
// @Produce     json
// @Success     200 {object} map[string]string
// @Router      /api/v1/ping [get]
func Ping(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "pong"})
}

// @Summary     Get items
// @Description Get all items
// @Tags        items
// @Produce     json
// @Success     200 {object} map[string]string
// @Router      /api/v1/items [get]
func GetItems(c *gin.Context) {
	// Logic to retrieve items goes here
	c.JSON(http.StatusOK, gin.H{"message": "GetItems called"})
}

// @Summary     Create item
// @Description Create a new item
// @Tags        items
// @Accept      json
// @Produce     json
// @Success     201 {object} map[string]string
// @Router      /api/v1/items [post]
func CreateItem(c *gin.Context) {
	// Logic to create an item goes here
	c.JSON(http.StatusCreated, gin.H{"message": "CreateItem called"})
}

// @Summary     Liveness Check
// @Description Get API liveness status
// @Tags        health
// @Produce     json
// @Success     200 {object} health.HealthStatus
// @Router      /health/live [get]
func LivenessCheck(c *gin.Context) {
	status := healthChecker.CheckLiveness()
	c.JSON(http.StatusOK, status)
}

// @Summary     Readiness Check
// @Description Get API readiness status
// @Tags        health
// @Produce     json
// @Success     200 {object} health.HealthStatus
// @Failure     503 {object} health.HealthStatus
// @Router      /health/ready [get]
func ReadinessCheck(c *gin.Context) {
	status := healthChecker.CheckReadiness()
	if status.Status == "DOWN" {
		c.JSON(http.StatusServiceUnavailable, status)
		return
	}
	c.JSON(http.StatusOK, status)
}

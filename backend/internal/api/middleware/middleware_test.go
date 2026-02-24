package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestLoggerMiddleware(t *testing.T) {
	// Not parallel: this test mutates the global slog default logger.
	// Set Gin to Test Mode
	gin.SetMode(gin.TestMode)

	// Create a buffer to capture slog output
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	// Setup router with middleware
	r := gin.New()
	r.Use(Logger())
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Create mock request
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.0.2.1:1234"

	// Serve request
	r.ServeHTTP(w, req)

	// Assert response status
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify that something was logged
	logOutput := buf.String()
	assert.Contains(t, logOutput, "GET")
	assert.Contains(t, logOutput, "/test")
}

func TestRecoveryMiddleware(t *testing.T) {
	t.Parallel()
	// Set Gin to Test Mode
	gin.SetMode(gin.TestMode)

	// Setup router with middleware
	r := gin.New()
	r.Use(Recovery())
	r.GET("/panic", func(c *gin.Context) {
		panic("test panic")
	})

	// Create mock request
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/panic", nil)

	// Serve request
	r.ServeHTTP(w, req)

	// Assert that the recovery middleware caught the panic
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response map[string]string
	err := json.NewDecoder(w.Body).Decode(&response)
	assert.Nil(t, err)
	assert.Equal(t, "Internal Server Error", response["error"])
}

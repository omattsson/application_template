package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestCORSMiddleware(t *testing.T) {
	// Set Gin to Test Mode
	gin.SetMode(gin.TestMode)

	// Setup router with middleware
	r := gin.New()
	r.Use(CORS())
	r.Any("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	t.Run("Regular GET request", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		r.ServeHTTP(w, req)

		// Check CORS headers
		assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "GET, POST, PUT, DELETE, OPTIONS", w.Header().Get("Access-Control-Allow-Methods"))
		assert.Equal(t, "Content-Type, Content-Length, Accept-Encoding, Authorization", w.Header().Get("Access-Control-Allow-Headers"))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("OPTIONS preflight request", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("OPTIONS", "/test", nil)
		r.ServeHTTP(w, req)

		// Check CORS headers
		assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "GET, POST, PUT, DELETE, OPTIONS", w.Header().Get("Access-Control-Allow-Methods"))
		assert.Equal(t, "Content-Type, Content-Length, Accept-Encoding, Authorization", w.Header().Get("Access-Control-Allow-Headers"))
		assert.Equal(t, http.StatusNoContent, w.Code) // OPTIONS request should return 204 No Content
	})
}

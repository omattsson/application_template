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
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Test cases
	tests := []struct {
		name           string
		origin         string
		expectedOrigin string
	}{
		{
			name:           "With origin header",
			origin:         "http://localhost:3000",
			expectedOrigin: "http://localhost:3000",
		},
		{
			name:           "Without origin header",
			origin:         "",
			expectedOrigin: "*",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/test", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			// Test preflight request
			preflightReq, _ := http.NewRequest("OPTIONS", "/test", nil)
			if tt.origin != "" {
				preflightReq.Header.Set("Origin", tt.origin)
			}
			preflightW := httptest.NewRecorder()

			r.ServeHTTP(preflightW, preflightReq)
			r.ServeHTTP(w, req)

			// Assert response headers for regular request
			assert.Equal(t, tt.expectedOrigin, w.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))

			// Assert response headers for preflight request
			assert.Equal(t, tt.expectedOrigin, preflightW.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "true", preflightW.Header().Get("Access-Control-Allow-Credentials"))
			assert.Equal(t, "GET,POST,PUT,PATCH,DELETE,HEAD,OPTIONS", preflightW.Header().Get("Access-Control-Allow-Methods"))
		})
	}
}

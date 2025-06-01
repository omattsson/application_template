package test

import (
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
)

// TestServer represents a test server instance
type TestServer struct {
	Engine *gin.Engine
}

// NewTestServer creates a new test server instance
func NewTestServer() *TestServer {
	gin.SetMode(gin.TestMode)
	return &TestServer{
		Engine: gin.New(),
	}
}

// ExecuteRequest executes a test request and returns the response
func (ts *TestServer) ExecuteRequest(method, path string, body interface{}) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(method, path, nil)
	ts.Engine.ServeHTTP(w, req)
	return w
}

// SetupTestEnvironment sets up the test environment with common middleware and configurations
func SetupTestEnvironment() *TestServer {
	server := NewTestServer()
	// Add common middleware for testing
	server.Engine.Use(gin.Recovery())
	return server
}

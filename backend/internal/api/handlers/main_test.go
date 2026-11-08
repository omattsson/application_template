package handlers

import (
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestMain sets Gin's mode once, before any test goroutine starts. gin.SetMode
// writes a package-level variable that gin.IsDebugging reads on every route
// registration, so calling it from parallel tests is a data race.
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

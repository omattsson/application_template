package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoGinSetModeOutsideTestMain guards against reintroducing the data race
// between parallel tests calling gin.SetMode and gin route registration
// reading the mode. Gin's mode is set once in TestMain (main_test.go).
func TestNoGinSetModeOutsideTestMain(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, f := range files {
		f := f
		if f == "main_test.go" || f == "ginmode_guard_test.go" {
			continue
		}
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.False(t, strings.Contains(string(data), "gin.SetMode("),
			"%s calls gin.SetMode; set the mode in TestMain instead (parallel tests race on it)", f)
	}
}

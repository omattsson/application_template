package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadConfig(t *testing.T) {
	// Test with environment variables set
	t.Run("With environment variables", func(t *testing.T) {
		// Set test environment variables
		os.Setenv("PORT", "3000")
		os.Setenv("ENV", "test")
		defer func() {
			os.Unsetenv("PORT")
			os.Unsetenv("ENV")
		}()

		config := LoadConfig()

		assert.Equal(t, "3000", config.Port)
		assert.Equal(t, "test", config.Env)
	})

	// Test with default values
	t.Run("With default values", func(t *testing.T) {
		// Clear environment variables
		os.Unsetenv("PORT")
		os.Unsetenv("ENV")

		config := LoadConfig()

		assert.Equal(t, "8080", config.Port)
		assert.Equal(t, "development", config.Env)
	})
}

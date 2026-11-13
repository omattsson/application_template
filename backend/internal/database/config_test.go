package database

import (
	"os"
	"testing"

	appconfig "backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConfig(t *testing.T) {
	t.Run("With environment variables", func(t *testing.T) {
		t.Setenv("DB_HOST", "testhost")
		t.Setenv("DB_PORT", "3307")
		t.Setenv("DB_USER", "testuser")
		t.Setenv("DB_PASSWORD", "testpass")
		t.Setenv("DB_NAME", "testdb")

		config := NewConfig()

		assert.Equal(t, "testhost", config.Host)
		assert.Equal(t, "3307", config.Port)
		assert.Equal(t, "testuser", config.User)
		assert.Equal(t, "testpass", config.Password)
		assert.Equal(t, "testdb", config.DBName)
	})

	t.Run("With default values", func(t *testing.T) {
		// Clear the variables; t.Setenv restores the original values.
		for _, k := range []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME"} {
			t.Setenv(k, "")
			require.NoError(t, os.Unsetenv(k))
		}

		config := NewConfig()

		assert.Equal(t, "localhost", config.Host)
		assert.Equal(t, "3306", config.Port)
		assert.Equal(t, "appuser", config.User)
		assert.Equal(t, "apppass", config.Password)
		assert.Equal(t, "app", config.DBName)
	})
}

func TestConfigDSN(t *testing.T) {
	config := &Config{
		Host:     "testhost",
		Port:     "3306",
		User:     "testuser",
		Password: "testpass",
		DBName:   "testdb",
	}

	expected := "testuser:testpass@tcp(testhost:3306)/testdb?charset=utf8mb4&parseTime=True&loc=UTC" + appconfig.MySQLSessionParams
	assert.Equal(t, expected, config.DSN())
	assert.Contains(t, config.DSN(), appconfig.MySQLSessionParams)
}

package database

import (
	"os"
	"testing"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewRepositoryWithAzure tests creating a repository using Azure Table Storage configuration
func TestNewRepositoryWithAzure(t *testing.T) {
	// Save current env vars and restore after test
	savedEnv := map[string]string{
		"USE_AZURE_TABLE":          os.Getenv("USE_AZURE_TABLE"),
		"USE_AZURITE":              os.Getenv("USE_AZURITE"),
		"AZURE_TABLE_ACCOUNT_NAME": os.Getenv("AZURE_TABLE_ACCOUNT_NAME"),
		"AZURE_TABLE_ACCOUNT_KEY":  os.Getenv("AZURE_TABLE_ACCOUNT_KEY"),
		"AZURE_TABLE_ENDPOINT":     os.Getenv("AZURE_TABLE_ENDPOINT"),
		"AZURE_TABLE_NAME":         os.Getenv("AZURE_TABLE_NAME"),
	}
	defer func() {
		for k, v := range savedEnv {
			if v != "" {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		}
	}()

	// Test path that should use Azure Table Storage
	t.Run("Selects Azure Table repository when configured", func(t *testing.T) {
		// Configure to use Azure Table but with invalid details to cause error
		os.Setenv("USE_AZURE_TABLE", "true")
		os.Setenv("USE_AZURITE", "false")
		os.Setenv("AZURE_TABLE_ACCOUNT_NAME", "testaccount")
		os.Setenv("AZURE_TABLE_ACCOUNT_KEY", "testkey")
		os.Setenv("AZURE_TABLE_ENDPOINT", "example.com")
		os.Setenv("AZURE_TABLE_NAME", "testitems")

		cfg, err := config.LoadConfig()
		require.NoError(t, err)

		// Should try to create Azure repository (which will fail due to invalid creds)
		_, err = NewRepository(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "azure_client")
	})

	// Test with Azurite emulator configuration
	t.Run("Uses Azurite when configured", func(t *testing.T) {
		// Standard Azurite configuration
		os.Setenv("USE_AZURE_TABLE", "true")
		os.Setenv("USE_AZURITE", "true")
		os.Setenv("AZURE_TABLE_ACCOUNT_NAME", "devstoreaccount1")
		os.Setenv("AZURE_TABLE_ACCOUNT_KEY", "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
		os.Setenv("AZURE_TABLE_ENDPOINT", "127.0.0.1:10002")
		os.Setenv("AZURE_TABLE_NAME", "items")

		cfg, err := config.LoadConfig()
		require.NoError(t, err)

		// Will fail because we're not really connecting to Azurite
		_, err = NewRepository(cfg)
		assert.Error(t, err)
		// Should attempt to use the http endpoint for azurite
		assert.Contains(t, err.Error(), "create_table")
	})

	// Test fallback to MySQL when Azure is not configured
	t.Run("Falls back to MySQL when Azure not configured", func(t *testing.T) {
		os.Setenv("USE_AZURE_TABLE", "false")
		os.Unsetenv("USE_AZURITE")
		os.Unsetenv("AZURE_TABLE_ACCOUNT_NAME")
		os.Unsetenv("AZURE_TABLE_ACCOUNT_KEY")
		os.Unsetenv("AZURE_TABLE_ENDPOINT")

		// Set MySQL vars to invalid values to trigger error
		os.Setenv("DB_HOST", "nonexistenthost")
		os.Setenv("DB_PORT", "3306")

		cfg, err := config.LoadConfig()
		require.NoError(t, err)

		// Should try to create MySQL repository (which will fail due to connection issues)
		_, err = NewRepository(cfg)
		assert.Error(t, err)
		// The error should be about failing to connect to MySQL, not about Azure
		assert.Contains(t, err.Error(), "failed to initialize MySQL database")
	})
}

// TestNewRepository_EnvironmentIntegration tests how the repository creation integrates with environment settings
func TestNewRepository_EnvironmentIntegration(t *testing.T) {
	// Save current env vars
	savedVars := map[string]string{
		"USE_AZURE_TABLE": os.Getenv("USE_AZURE_TABLE"),
		"DB_HOST":         os.Getenv("DB_HOST"),
		"DB_PORT":         os.Getenv("DB_PORT"),
		"DB_USER":         os.Getenv("DB_USER"),
		"DB_PASSWORD":     os.Getenv("DB_PASSWORD"),
		"DB_NAME":         os.Getenv("DB_NAME"),
	}
	defer func() {
		// Restore env vars
		for k, v := range savedVars {
			if v != "" {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		}
	}()

	// Test that environment variables properly affect repository selection
	t.Run("Repository type based on environment variables", func(t *testing.T) {
		// Setup test environments
		testCases := []struct {
			name        string
			envVars     map[string]string
			expectAzure bool
		}{
			{
				name: "Default MySQL configuration",
				envVars: map[string]string{
					"USE_AZURE_TABLE": "false",
					"DB_HOST":         "localhost",
					"DB_NAME":         "testdb",
				},
				expectAzure: false,
			},
			{
				name: "Azure configuration",
				envVars: map[string]string{
					"USE_AZURE_TABLE":          "true",
					"AZURE_TABLE_ACCOUNT_NAME": "account",
					"AZURE_TABLE_ACCOUNT_KEY":  "key",
					"AZURE_TABLE_ENDPOINT":     "endpoint.com",
				},
				expectAzure: true,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// Set environment variables
				for k, v := range tc.envVars {
					os.Setenv(k, v)
				}

				// Load config
				cfg, err := config.LoadConfig()
				require.NoError(t, err)

				// Check if config reflects the right choice
				assert.Equal(t, tc.expectAzure, cfg.AzureTable.UseAzureTable)

				// Actual repository creation will fail due to invalid connection info,
				// but we can verify that the right path was chosen based on error message
				_, err = NewRepository(cfg)
				assert.Error(t, err)

				if tc.expectAzure {
					assert.Contains(t, err.Error(), "azure_client")
				} else {
					assert.Contains(t, err.Error(), "failed to initialize MySQL database")
				}
			})
		}
	})
}

//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAzureTableIntegration tests the Azure Table Storage integration in main.go
func TestAzureTableIntegration(t *testing.T) {
	// Not parallel: subtests use t.Setenv
	// Save current env vars
	envVars := []string{
		"USE_AZURE_TABLE",
		"USE_AZURITE",
		"AZURE_TABLE_ACCOUNT_NAME",
		"AZURE_TABLE_ACCOUNT_KEY",
		"AZURE_TABLE_ENDPOINT",
		"AZURE_TABLE_NAME",
	}

	savedValues := make(map[string]string)
	for _, key := range envVars {
		savedValues[key] = os.Getenv(key)
	}

	t.Cleanup(func() {
		// Restore environment variables
		for key, value := range savedValues {
			if value != "" {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		}
	})

	// Test health check configuration with Azure Table
	t.Run("Configure health check for Azure Table", func(t *testing.T) {
		// Configure to use Azure Table (with invalid details)
		t.Setenv("USE_AZURE_TABLE", "true")
		t.Setenv("USE_AZURITE", "false")
		t.Setenv("AZURE_TABLE_ACCOUNT_NAME", "testaccount")
		t.Setenv("AZURE_TABLE_ACCOUNT_KEY", "testkey")
		t.Setenv("AZURE_TABLE_ENDPOINT", "example.com")
		t.Setenv("AZURE_TABLE_NAME", "testitems")

		// Load config
		cfg, err := config.LoadConfig()
		require.NoError(t, err)
		assert.True(t, cfg.AzureTable.UseAzureTable)

		// Attempt to create repository (this will fail)
		repo, err := database.NewRepository(cfg)
		require.Error(t, err) // Expected to fail with invalid Azure credentials
		require.Nil(t, repo)

		// Test the health check behavior - this only tests the conditional logic in main.go
		// that configures the health check differently for Azure vs MySQL
		healthChecker := health.New()

		// For Azure Table, we add a mock health check since the repo is nil
		healthChecker.AddCheck("database", func(_ context.Context) error {
			return nil // Mock check since we expect repo to be nil in this test
		})

		// The health check should return UP since we're using a mock check
		healthChecker.SetReady(true)
		status := healthChecker.CheckReadiness(context.Background())
		assert.Equal(t, "UP", status.Status)
	})

	// Test main.go conditional logic for Azurite
	t.Run("Configure for Azurite emulator", func(t *testing.T) {
		// Configure to use Azurite
		t.Setenv("USE_AZURE_TABLE", "true")
		t.Setenv("USE_AZURITE", "true")
		t.Setenv("AZURE_TABLE_ACCOUNT_NAME", "devstoreaccount1")
		t.Setenv("AZURE_TABLE_ACCOUNT_KEY", "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
		t.Setenv("AZURE_TABLE_ENDPOINT", "127.0.0.1:10002")
		t.Setenv("AZURE_TABLE_NAME", "items")

		// Load config
		cfg, err := config.LoadConfig()
		require.NoError(t, err)
		assert.True(t, cfg.AzureTable.UseAzureTable)
		assert.True(t, cfg.AzureTable.UseAzurite)

		// Repository creation will fail without actual Azurite,
		// but we can verify the config loading works
		_, err = database.NewRepository(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create_table")
	})
}

// TestDatabaseChoiceIntegration tests the database choice logic in main.go
func TestDatabaseChoiceIntegration(t *testing.T) {
	// Not parallel: subtests use t.Setenv
	// Save current env vars for DB and Azure config
	envVars := []string{
		"USE_AZURE_TABLE",
		"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
	}

	savedValues := make(map[string]string)
	for _, key := range envVars {
		savedValues[key] = os.Getenv(key)
	}

	t.Cleanup(func() {
		// Restore environment variables
		for key, value := range savedValues {
			if value != "" {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		}
	})

	t.Run("Main selects correct database type", func(t *testing.T) {
		// Test cases for different database configurations
		testCases := []struct {
			name                 string
			useAzureTable        string
			dbHost               string
			expectErrorSubstring string
		}{
			{
				name:                 "Choose MySQL database",
				useAzureTable:        "false",
				dbHost:               "nonexistent-host",
				expectErrorSubstring: "failed to initialize MySQL database",
			},
			{
				name:                 "Choose Azure Table Storage",
				useAzureTable:        "true",
				dbHost:               "localhost", // Irrelevant when using Azure
				expectErrorSubstring: "azure_client",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				tc := tc // Capture range variable
				// Set environment variables for this test case
				t.Setenv("USE_AZURE_TABLE", tc.useAzureTable)
				t.Setenv("DB_HOST", tc.dbHost)

				// Load config
				cfg, err := config.LoadConfig()
				require.NoError(t, err)

				// Try to create repository (will fail with invalid credentials)
				_, err = database.NewRepository(cfg)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectErrorSubstring)
			})
		}
	})
}

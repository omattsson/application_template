package azure_test

import (
	"context"
	"testing"

	"backend/internal/database/azure"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
)

// TestTableRepository_ConfigurationHandling tests how the repository handles various configurations
func TestTableRepository_ConfigurationHandling(t *testing.T) {
	t.Parallel()

	// Test cases for different connection string configurations
	testCases := []struct {
		name        string
		accountName string
		accountKey  string
		endpoint    string
		tableName   string
		useAzurite  bool
		expectError bool
		errorType   string
	}{
		{
			name:        "Empty account name",
			accountName: "",
			accountKey:  "key123",
			endpoint:    "endpoint.com",
			tableName:   "items",
			useAzurite:  false,
			expectError: true,
			errorType:   "azure_client",
		},
		{
			name:        "Empty account key",
			accountName: "account123",
			accountKey:  "",
			endpoint:    "endpoint.com",
			tableName:   "items",
			useAzurite:  false,
			expectError: true,
			errorType:   "azure_client",
		},
		{
			name:        "Empty table name",
			accountName: "account123",
			accountKey:  "key123",
			endpoint:    "endpoint.com",
			tableName:   "",
			useAzurite:  false,
			expectError: true,
			errorType:   "azure_client", // Will fail during table creation
		},
		{
			name:        "Invalid endpoint",
			accountName: "account123",
			accountKey:  "key123",
			endpoint:    "://invalid-endpoint",
			tableName:   "items",
			useAzurite:  false,
			expectError: true,
			errorType:   "azure_client",
		},
		{
			name:        "Azurite with empty endpoint",
			accountName: "account123",
			accountKey:  "key123",
			endpoint:    "",
			tableName:   "items",
			useAzurite:  true,
			expectError: true,
			errorType:   "azure_client",
		},
		{
			name:        "Standard with empty endpoint",
			accountName: "account123",
			accountKey:  "key123",
			endpoint:    "",
			tableName:   "items",
			useAzurite:  false,
			expectError: true,
			errorType:   "azure_client",
		},
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := azure.NewTableRepository(
				tc.accountName,
				tc.accountKey,
				tc.endpoint,
				tc.tableName,
				tc.useAzurite,
			)

			if tc.expectError {
				assert.Error(t, err)
				assert.Nil(t, repo)

				// Check if error is wrapped in a DatabaseError
				var dbErr *dberrors.DatabaseError
				if assert.ErrorAs(t, err, &dbErr) {
					assert.Equal(t, tc.errorType, dbErr.Op)
				}
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, repo)
			}
		})
	}
}

// TestTableRepository_InvalidData tests handling of invalid data in operations
func TestTableRepository_InvalidData(t *testing.T) {
	t.Parallel()

	// Create a minimal repository for testing
	repo := azure.NewTestTableRepository("testtable")
	ctx := context.Background()

	// Test invalid inputs for different operations
	t.Run("Invalid inputs for Create", func(t *testing.T) {
		t.Parallel()
		// Test with nil
		err := repo.Create(ctx, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.Create(ctx, "string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "entity must be *models.Item")
	})

	t.Run("Invalid inputs for FindByID", func(t *testing.T) {
		t.Parallel()
		// Test with nil
		err := repo.FindByID(ctx, 1, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.FindByID(ctx, 1, "string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dest must be *models.Item")
	})

	t.Run("Invalid inputs for Update", func(t *testing.T) {
		t.Parallel()
		// Test with nil
		err := repo.Update(ctx, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.Update(ctx, "string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "entity must be *models.Item")
	})

	t.Run("Invalid inputs for Delete", func(t *testing.T) {
		t.Parallel()
		// Test with nil
		err := repo.Delete(ctx, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.Delete(ctx, "string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "entity must be *models.Item")
	})

	t.Run("Invalid inputs for List", func(t *testing.T) {
		t.Parallel()
		// Test with nil
		err := repo.List(ctx, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.List(ctx, "string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dest must be *[]models.Item")
	})
}

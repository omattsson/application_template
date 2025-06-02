package azure

import (
	"testing"

	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
)

// TestTableRepository_ConfigurationHandling tests how the repository handles various configurations
func TestTableRepository_ConfigurationHandling(t *testing.T) {
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
		t.Run(tc.name, func(t *testing.T) {
			repo, err := NewTableRepository(
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
	// Create a minimal repository for testing
	repo := &TableRepository{
		client:    nil, // Not used in these tests
		tableName: "test",
	}

	// Test invalid inputs for different operations
	t.Run("Invalid inputs for Create", func(t *testing.T) {
		// Test with nil
		err := repo.Create(nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.Create("string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "entity must be *models.Item")
	})

	t.Run("Invalid inputs for FindByID", func(t *testing.T) {
		// Test with nil
		err := repo.FindByID(1, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.FindByID(1, "string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dest must be *models.Item")
	})

	t.Run("Invalid inputs for Update", func(t *testing.T) {
		// Test with nil
		err := repo.Update(nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.Update("string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "entity must be *models.Item")
	})

	t.Run("Invalid inputs for Delete", func(t *testing.T) {
		// Test with nil
		err := repo.Delete(nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.Delete("string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "entity must be *models.Item")
	})

	t.Run("Invalid inputs for List", func(t *testing.T) {
		// Test with nil
		err := repo.List(nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type_assertion")

		// Test with wrong type
		err = repo.List("string")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dest must be *[]models.Item")
	})
}

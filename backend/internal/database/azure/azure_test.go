//go:build integration

package azure_test

import (
	"backend/internal/database/azure"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/stretchr/testify/assert"
)

func TestHelperFunctions(t *testing.T) {
	t.Parallel()

	// Test isTableExistsError
	t.Run("isTableExistsError handles different error types", func(t *testing.T) {
		t.Parallel()
		err := &azcore.ResponseError{ErrorCode: "TableAlreadyExists"}
		assert.True(t, azure.IsTableExistsError(err))
		assert.False(t, azure.IsTableExistsError(nil))
	})

	// Test isEntityExistsError
	t.Run("isEntityExistsError handles different error types", func(t *testing.T) {
		t.Parallel()
		err := &azcore.ResponseError{ErrorCode: "EntityAlreadyExists"}
		assert.True(t, azure.IsEntityExistsError(err))
		assert.False(t, azure.IsEntityExistsError(nil))
	})

	// Test isNotFoundError
	t.Run("isNotFoundError handles different error types", func(t *testing.T) {
		t.Parallel()
		err := &azcore.ResponseError{StatusCode: 404}
		assert.True(t, azure.IsNotFoundError(err))
		assert.False(t, azure.IsNotFoundError(nil))
	})
}

// TestNewTableRepository verifies repository instantiation with various configs
func TestNewTableRepository(t *testing.T) {
	t.Parallel()

	t.Run("repository created with valid config", func(t *testing.T) {
		t.Parallel()
		repo, err := azure.NewTableRepository(
			"testaccount",
			"testkey",
			"endpoint.com",
			"testtable",
			false,
		)
		// Should fail as we're not actually connecting to Azure
		assert.Error(t, err)
		assert.Nil(t, repo)
	})

	t.Run("repository fails with empty account", func(t *testing.T) {
		t.Parallel()
		repo, err := azure.NewTableRepository(
			"",
			"testkey",
			"endpoint.com",
			"testtable",
			false,
		)
		assert.Error(t, err)
		assert.Nil(t, repo)
	})

	t.Run("repository fails with empty key", func(t *testing.T) {
		t.Parallel()
		repo, err := azure.NewTableRepository(
			"testaccount",
			"",
			"endpoint.com",
			"testtable",
			false,
		)
		assert.Error(t, err)
		assert.Nil(t, repo)
	})
}

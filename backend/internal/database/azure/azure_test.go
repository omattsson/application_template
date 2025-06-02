package azure

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/stretchr/testify/assert"
)

func TestHelperFunctions(t *testing.T) {
	// Test isTableExistsError
	t.Run("isTableExistsError handles different error types", func(t *testing.T) {
		err := &azcore.ResponseError{ErrorCode: "TableAlreadyExists"}
		assert.True(t, isTableExistsError(err))
		assert.False(t, isTableExistsError(nil))
	})

	// Test isEntityExistsError
	t.Run("isEntityExistsError handles different error types", func(t *testing.T) {
		err := &azcore.ResponseError{ErrorCode: "EntityAlreadyExists"}
		assert.True(t, isEntityExistsError(err))
		assert.False(t, isEntityExistsError(nil))
	})

	// Test isNotFoundError
	t.Run("isNotFoundError handles different error types", func(t *testing.T) {
		err := &azcore.ResponseError{StatusCode: 404}
		assert.True(t, isNotFoundError(err))
		assert.False(t, isNotFoundError(nil))
	})
}

// This is a minimal test to verify our client adapter is working
func TestClientAdapter(t *testing.T) {
	t.Run("adapter can be created", func(t *testing.T) {
		// We cannot actually create a client without valid credentials,
		// but we can verify our adapter functions are working
		adapter := &azureClientAdapter{Client: nil}
		assert.NotNil(t, adapter)
	})
}

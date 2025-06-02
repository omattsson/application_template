package azure

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewTableRepository doesn't actually use mocks - it tests the real function
// with integration-like tests, but without making real network calls
func TestNewTableRepository_Validation(t *testing.T) {
	// Test with empty parameters - these should fail validation in a real scenario
	// but we're just ensuring our function handles input appropriately
	testCases := []struct {
		name       string
		accName    string
		accKey     string
		endpoint   string
		tableName  string
		useAzurite bool
		expectErr  bool
		errSubstr  string
	}{
		{
			name:      "Empty account name",
			accName:   "",
			accKey:    "key",
			endpoint:  "endpoint",
			tableName: "table",
			expectErr: true,
			errSubstr: "connection string",
		},
		{
			name:      "Empty account key",
			accName:   "name",
			accKey:    "",
			endpoint:  "endpoint",
			tableName: "table",
			expectErr: true,
			errSubstr: "connection string",
		},
		{
			name:       "Invalid parameters but azurite enabled",
			accName:    "devstoreaccount1",
			accKey:     "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==",
			endpoint:   "localhost:10002",
			tableName:  "items",
			useAzurite: true,
			expectErr:  true, // Will fail because we're not actually connecting to azurite
			errSubstr:  "create_table: 400 Bad Request",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repo, err := NewTableRepository(tc.accName, tc.accKey, tc.endpoint, tc.tableName, tc.useAzurite)

			if tc.expectErr {
				assert.Error(t, err)
				if tc.errSubstr != "" {
					assert.Contains(t, err.Error(), tc.errSubstr)
				}
				assert.Nil(t, repo)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, repo)
				assert.Equal(t, tc.tableName, repo.tableName)
			}
		})
	}
}

// TestErrorHelperFunctions tests the utility error handling functions
func TestErrorHelperFunctions(t *testing.T) {
	// Test isTableExistsError
	t.Run("isTableExistsError identifies correct errors", func(t *testing.T) {
		err := &azcore.ResponseError{ErrorCode: "TableAlreadyExists"}
		assert.True(t, isTableExistsError(err))
		assert.False(t, isTableExistsError(errors.New("other error")))
		assert.False(t, isTableExistsError(&azcore.ResponseError{ErrorCode: "OtherError"}))
		assert.False(t, isTableExistsError(nil))
	})

	// Test isEntityExistsError
	t.Run("isEntityExistsError identifies correct errors", func(t *testing.T) {
		err := &azcore.ResponseError{ErrorCode: "EntityAlreadyExists"}
		assert.True(t, isEntityExistsError(err))
		assert.False(t, isEntityExistsError(errors.New("other error")))
		assert.False(t, isEntityExistsError(&azcore.ResponseError{ErrorCode: "OtherError"}))
		assert.False(t, isEntityExistsError(nil))
	})

	// Test isNotFoundError
	t.Run("isNotFoundError identifies correct errors", func(t *testing.T) {
		err := &azcore.ResponseError{StatusCode: 404}
		assert.True(t, isNotFoundError(err))
		assert.False(t, isNotFoundError(errors.New("other error")))
		assert.False(t, isNotFoundError(&azcore.ResponseError{StatusCode: 500}))
		assert.False(t, isNotFoundError(nil))
	})
}

// TestTableRepository_URLConstruction tests the URL construction logic
func TestTableRepository_URLConstruction(t *testing.T) {
	t.Run("URL construction handles azurite", func(t *testing.T) {
		// We can't test the internals directly, but we can make sure the code path
		// for both azurite and non-azurite is exercised (even if connection fails)

		// Standard URL construction
		_, err := NewTableRepository("account", "key", "endpoint.com", "table", false)
		require.Error(t, err) // Will fail because we're not actually connecting
		assert.Contains(t, err.Error(), "azure_client")

		// Azurite URL construction should be different
		_, err = NewTableRepository("account", "key", "127.0.0.1:10002", "table", true)
		require.Error(t, err) // Will fail because we're not actually connecting
		assert.Contains(t, err.Error(), "azure_client")
	})
}

// TestTableRepository_DatabaseErrors tests the error wrapping functionality
// mockClient implements the minimal interface needed for TableRepository tests
type mockClient struct {
	addEntityFn            func(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error)
	getEntityFn            func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error)
	updateEntityFn         func(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error)
	newListEntitiesPagerFn func(options *aztables.ListEntitiesOptions) ListEntitiesPager
	deleteEntityFn         func(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error)
}

func (m *mockClient) AddEntity(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
	return m.addEntityFn(ctx, entity, options)
}
func (m *mockClient) GetEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
	return m.getEntityFn(ctx, partitionKey, rowKey, options)
}
func (m *mockClient) UpdateEntity(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
	return m.updateEntityFn(ctx, entity, options)
}
func (m *mockClient) NewListEntitiesPager(options *aztables.ListEntitiesOptions) ListEntitiesPager {
	return m.newListEntitiesPagerFn(options)
}
func (m *mockClient) DeleteEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
	return m.deleteEntityFn(ctx, partitionKey, rowKey, options)
}

// mockPager implements ListEntitiesPager for testing
type mockPager struct {
	err error
}

func (m *mockPager) More() bool { return false }
func (m *mockPager) NextPage(ctx context.Context) ([]byte, error) {
	return nil, m.err
}
func TestTableRepository_DatabaseErrors(t *testing.T) {
	// Create a mock that returns errors for all operations
	mockClient := &mockClient{
		addEntityFn: func(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
			return aztables.AddEntityResponse{}, fmt.Errorf("mock error")
		},
		getEntityFn: func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
			return aztables.GetEntityResponse{}, fmt.Errorf("mock error")
		},
		updateEntityFn: func(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
			return aztables.UpdateEntityResponse{}, fmt.Errorf("mock error")
		},
		newListEntitiesPagerFn: func(options *aztables.ListEntitiesOptions) ListEntitiesPager {
			return &mockPager{err: fmt.Errorf("mock error")}
		},
	}

	badRepo := &TableRepository{
		client:    mockClient,
		tableName: "test",
	}

	t.Run("Operations propagate proper database errors", func(t *testing.T) {
		// Test Create with bad client
		err := badRepo.Create(&models.Item{})
		assert.Error(t, err)
		var dbErr *dberrors.DatabaseError
		assert.True(t, errors.As(err, &dbErr))

		// Test FindByID with bad client
		item := &models.Item{}
		err = badRepo.FindByID(1, item)
		assert.Error(t, err)
		assert.True(t, errors.As(err, &dbErr))

		// Test Update with bad client
		err = badRepo.Update(&models.Item{})
		assert.Error(t, err)
		assert.True(t, errors.As(err, &dbErr))

		// Test Delete with bad client
		err = badRepo.Delete(&models.Item{})
		assert.Error(t, err)
		assert.True(t, errors.As(err, &dbErr))

		// Test List with bad client
		items := []models.Item{}
		err = badRepo.List(&items)
		assert.Error(t, err)
		assert.True(t, errors.As(err, &dbErr))

		// Test Ping with bad client
		err = badRepo.Ping()
		assert.Error(t, err)
		assert.True(t, errors.As(err, &dbErr))
	})
}

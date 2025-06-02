package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/stretchr/testify/assert"
)

// --- Mocks ---

// mockPager implements ListEntitiesPager for testing
type mockPager struct {
	pages [][]byte
	index int
	err   error
}

func (p *mockPager) More() bool {
	return p.index < len(p.pages)
}

func (p *mockPager) NextPage(ctx context.Context) (aztables.ListEntitiesResponse, error) {
	if p.err != nil {
		return aztables.ListEntitiesResponse{}, p.err
	}
	if !p.More() {
		return aztables.ListEntitiesResponse{}, nil
	}
	resp := aztables.ListEntitiesResponse{
		Entities: [][]byte{p.pages[p.index]},
	}
	p.index++
	return resp, nil
}

// mockClient implements AzureTableClient for testing
type mockClient struct {
	addEntityFn            func(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error)
	getEntityFn            func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error)
	updateEntityFn         func(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error)
	deleteEntityFn         func(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error)
	newListEntitiesPagerFn func(options *aztables.ListEntitiesOptions) ListEntitiesPager
}

func (m *mockClient) AddEntity(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
	if m.addEntityFn != nil {
		return m.addEntityFn(ctx, entity, options)
	}
	return aztables.AddEntityResponse{}, fmt.Errorf("mock not implemented")
}

func (m *mockClient) GetEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
	if m.getEntityFn != nil {
		return m.getEntityFn(ctx, partitionKey, rowKey, options)
	}
	return aztables.GetEntityResponse{}, fmt.Errorf("mock not implemented")
}

func (m *mockClient) UpdateEntity(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
	if m.updateEntityFn != nil {
		return m.updateEntityFn(ctx, entity, options)
	}
	return aztables.UpdateEntityResponse{}, fmt.Errorf("mock not implemented")
}

func (m *mockClient) DeleteEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
	if m.deleteEntityFn != nil {
		return m.deleteEntityFn(ctx, partitionKey, rowKey, options)
	}
	return aztables.DeleteEntityResponse{}, fmt.Errorf("mock not implemented")
}
func (m *mockClient) NewListEntitiesPager(options *aztables.ListEntitiesOptions) ListEntitiesPager {
	if m.newListEntitiesPagerFn != nil {
		return m.newListEntitiesPagerFn(options)
	}
	return &mockPager{}
}

// --- Helper ---

func newTestTableRepository(mock *mockClient) *TableRepository {
	return &TableRepository{
		client:    mock,
		tableName: "test",
	}
}

// --- Tests ---

func TestCreate_Success(t *testing.T) {
	mock := &mockClient{
		addEntityFn: func(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
			return aztables.AddEntityResponse{}, nil
		},
	}
	repo := newTestTableRepository(mock)
	item := &models.Item{Base: models.Base{ID: 1}, Name: "foo", Price: 42.0}
	err := repo.Create(item)
	assert.NoError(t, err)
	assert.NotZero(t, item.CreatedAt)
	assert.NotZero(t, item.UpdatedAt)
}

func TestCreate_Duplicate(t *testing.T) {
	mock := &mockClient{
		addEntityFn: func(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
			return aztables.AddEntityResponse{}, &azcore.ResponseError{ErrorCode: "EntityAlreadyExists"}
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	item := &models.Item{Base: models.Base{ID: 1}, Name: "foo", Price: 42.0}
	err := repo.Create(item)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, dberrors.ErrDuplicateKey))
}

func TestCreate_TypeAssertion(t *testing.T) {
	repo := &TableRepository{}
	err := repo.Create("not an item")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "entity must be *models.Item")
}

func TestFindByID_Success(t *testing.T) {
	now := time.Now().UTC()
	entity := map[string]interface{}{
		"Name":      "foo",
		"Price":     42.0,
		"CreatedAt": now.Format(time.RFC3339),
		"UpdatedAt": now.Format(time.RFC3339),
	}
	val, _ := json.Marshal(entity)
	mock := &mockClient{
		getEntityFn: func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
			return aztables.GetEntityResponse{Value: val}, nil
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	item := &models.Item{}
	err := repo.FindByID(1, item)
	assert.NoError(t, err)
	assert.Equal(t, uint(1), item.ID)
	assert.Equal(t, "foo", item.Name)
	assert.Equal(t, 42.0, item.Price)
	assert.WithinDuration(t, now, item.CreatedAt, time.Second)
	assert.WithinDuration(t, now, item.UpdatedAt, time.Second)
}

func TestFindByID_NotFound(t *testing.T) {
	mock := &mockClient{
		getEntityFn: func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
			return aztables.GetEntityResponse{}, &azcore.ResponseError{StatusCode: 404}
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	item := &models.Item{}
	err := repo.FindByID(1, item)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, dberrors.ErrNotFound))
}

func TestFindByID_TypeAssertion(t *testing.T) {
	repo := &TableRepository{}
	err := repo.FindByID(1, "not an item")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "dest must be *models.Item")
}

func TestUpdate_Success(t *testing.T) {
	now := time.Now().UTC()
	item := &models.Item{Base: models.Base{ID: 1, CreatedAt: now}, Name: "foo", Price: 42.0}
	mock := &mockClient{
		getEntityFn: func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
			return aztables.GetEntityResponse{}, nil
		},
		updateEntityFn: func(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
			return aztables.UpdateEntityResponse{}, nil
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	err := repo.Update(item)
	assert.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC(), item.UpdatedAt, time.Second)
}

func TestUpdate_NotFound(t *testing.T) {
	item := &models.Item{Base: models.Base{ID: 1}, Name: "foo", Price: 42.0}
	mock := &mockClient{
		getEntityFn: func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
			return aztables.GetEntityResponse{}, &azcore.ResponseError{StatusCode: 404}
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	err := repo.Update(item)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, dberrors.ErrNotFound))
}

func TestUpdate_TypeAssertion(t *testing.T) {
	repo := &TableRepository{}
	err := repo.Update("not an item")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "entity must be *models.Item")
}

func TestDelete_Success(t *testing.T) {
	item := &models.Item{Base: models.Base{ID: 1}}
	mock := &mockClient{
		deleteEntityFn: func(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
			return aztables.DeleteEntityResponse{}, nil
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	err := repo.Delete(item)
	assert.NoError(t, err)
}

func TestDelete_NotFound(t *testing.T) {
	item := &models.Item{Base: models.Base{ID: 1}}
	mock := &mockClient{
		deleteEntityFn: func(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
			return aztables.DeleteEntityResponse{}, &azcore.ResponseError{StatusCode: 404}
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	err := repo.Delete(item)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, dberrors.ErrNotFound))
}

func TestDelete_TypeAssertion(t *testing.T) {
	repo := &TableRepository{}
	err := repo.Delete("not an item")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "entity must be *models.Item")
}

func TestList_Success(t *testing.T) {
	now := time.Now().UTC()
	entity := map[string]interface{}{
		"RowKey":    strconv.FormatUint(1, 10),
		"Name":      "foo",
		"Price":     42.0,
		"CreatedAt": now.Format(time.RFC3339),
		"UpdatedAt": now.Format(time.RFC3339),
	}
	val, _ := json.Marshal(entity)
	mockPager := &mockPager{pages: [][]byte{val}}
	mock := &mockClient{
		newListEntitiesPagerFn: func(options *aztables.ListEntitiesOptions) ListEntitiesPager {
			return mockPager
		},
	}
	repo := &TableRepository{client: mock, tableName: "test"}
	var items []models.Item
	err := repo.List(&items)
	assert.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "foo", items[0].Name)
	assert.Equal(t, 42.0, items[0].Price)
}

func TestList_MultiplePages(t *testing.T) {
	now := time.Now().UTC()
	entities := []map[string]interface{}{
		{
			"RowKey":    "1",
			"Name":      "foo",
			"Price":     42.0,
			"CreatedAt": now.Format(time.RFC3339),
			"UpdatedAt": now.Format(time.RFC3339),
		},
		{
			"RowKey":    "2",
			"Name":      "bar",
			"Price":     43.0,
			"CreatedAt": now.Format(time.RFC3339),
			"UpdatedAt": now.Format(time.RFC3339),
		},
	}

	pages := make([][]byte, len(entities))
	for i, entity := range entities {
		val, _ := json.Marshal(entity)
		pages[i] = val
	}

	mockPager := &mockPager{pages: pages}
	mock := &mockClient{
		newListEntitiesPagerFn: func(options *aztables.ListEntitiesOptions) ListEntitiesPager {
			return mockPager
		},
	}
	repo := newTestTableRepository(mock)
	var items []models.Item
	err := repo.List(&items)
	assert.NoError(t, err)
	assert.Len(t, items, 2)
	assert.Equal(t, "foo", items[0].Name)
	assert.Equal(t, "bar", items[1].Name)
}

func TestList_EmptyResults(t *testing.T) {
	mockPager := &mockPager{pages: [][]byte{}}
	mock := &mockClient{
		newListEntitiesPagerFn: func(options *aztables.ListEntitiesOptions) ListEntitiesPager {
			return mockPager
		},
	}
	repo := newTestTableRepository(mock)
	var items []models.Item
	err := repo.List(&items)
	assert.NoError(t, err)
	assert.Len(t, items, 0)
}

func TestList_PaginationError(t *testing.T) {
	mock := &mockClient{
		newListEntitiesPagerFn: func(options *aztables.ListEntitiesOptions) ListEntitiesPager {
			return &mockPager{
				pages: [][]byte{},
				err:   fmt.Errorf("mock error"),
			}
		},
	}

	repo := newTestTableRepository(mock)
	var items []models.Item
	err := repo.List(&items)
	assert.Error(t, err)
	var dbErr *dberrors.DatabaseError
	if assert.ErrorAs(t, err, &dbErr) {
		assert.Equal(t, "list", dbErr.Op)
		assert.Contains(t, dbErr.Error(), "mock error")
	}
}

func TestList_InvalidEntityData(t *testing.T) {
	mockPager := &mockPager{pages: [][]byte{[]byte("invalid json")}}
	mock := &mockClient{
		newListEntitiesPagerFn: func(options *aztables.ListEntitiesOptions) ListEntitiesPager {
			return mockPager
		},
	}
	repo := newTestTableRepository(mock)
	var items []models.Item
	err := repo.List(&items)
	assert.Error(t, err)
	var dbErr *dberrors.DatabaseError
	if assert.ErrorAs(t, err, &dbErr) {
		assert.Equal(t, "unmarshal", dbErr.Op)
	}
}

func TestList_FilteringAndPagination(t *testing.T) {
	now := time.Now().UTC()
	entities := []map[string]interface{}{
		{
			"RowKey":    "1",
			"Name":      "Phone",
			"Price":     999.99,
			"CreatedAt": now.Format(time.RFC3339),
			"UpdatedAt": now.Format(time.RFC3339),
		},
		{
			"RowKey":    "2",
			"Name":      "Laptop",
			"Price":     1999.99,
			"CreatedAt": now.Format(time.RFC3339),
			"UpdatedAt": now.Format(time.RFC3339),
		},
		{
			"RowKey":    "3",
			"Name":      "Phone Case",
			"Price":     29.99,
			"CreatedAt": now.Format(time.RFC3339),
			"UpdatedAt": now.Format(time.RFC3339),
		},
	}

	pages := make([][]byte, len(entities))
	for i, entity := range entities {
		val, _ := json.Marshal(entity)
		pages[i] = val
	}

	tests := []struct {
		name       string
		conditions []interface{}
		wantNames  []string
		wantCount  int
	}{
		{
			name:       "no filters",
			conditions: nil,
			wantCount:  3,
			wantNames:  []string{"Phone", "Laptop", "Phone Case"},
		},
		{
			name: "filter by name exact",
			conditions: []interface{}{
				models.Filter{Field: "name", Op: "exact", Value: "Phone"},
			},
			wantCount: 1,
			wantNames: []string{"Phone"},
		},
		{
			name: "filter by name contains",
			conditions: []interface{}{
				models.Filter{Field: "name", Value: "Phone"},
			},
			wantCount: 2,
			wantNames: []string{"Phone", "Phone Case"},
		},
		{
			name: "filter by min price",
			conditions: []interface{}{
				models.Filter{Field: "price", Op: ">=", Value: 1000.0},
			},
			wantCount: 1,
			wantNames: []string{"Laptop"},
		},
		{
			name: "filter by max price",
			conditions: []interface{}{
				models.Filter{Field: "price", Op: "<=", Value: 50.0},
			},
			wantCount: 1,
			wantNames: []string{"Phone Case"},
		},
		{
			name: "pagination limit only",
			conditions: []interface{}{
				models.Pagination{Limit: 2, Offset: 0},
			},
			wantCount: 2,
			wantNames: []string{"Phone", "Laptop"},
		},
		{
			name: "pagination with offset",
			conditions: []interface{}{
				models.Pagination{Limit: 2, Offset: 1},
			},
			wantCount: 2,
			wantNames: []string{"Laptop", "Phone Case"},
		},
		{
			name: "combined filters and pagination",
			conditions: []interface{}{
				models.Filter{Field: "name", Value: "Phone"},
				models.Pagination{Limit: 1, Offset: 1},
			},
			wantCount: 1,
			wantNames: []string{"Phone Case"},
		},
	}

	mockPager := &mockPager{pages: pages}
	mock := &mockClient{
		newListEntitiesPagerFn: func(options *aztables.ListEntitiesOptions) ListEntitiesPager {
			return mockPager
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newTestTableRepository(mock)
			var items []models.Item
			err := repo.List(&items, tt.conditions...)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantCount, len(items))

			names := make([]string, len(items))
			for i, item := range items {
				names[i] = item.Name
			}
			assert.Equal(t, tt.wantNames, names)
		})
	}
}

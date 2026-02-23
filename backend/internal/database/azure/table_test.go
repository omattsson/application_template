package azure_test

import (
	"context"
	"testing"

	"backend/internal/database/azure"
	"backend/internal/models"

	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/stretchr/testify/assert"
)

type testPager struct {
	pages   [][]byte
	current int
	err     error
}

func (p *testPager) More() bool {
	return p.current < len(p.pages)
}

func (p *testPager) NextPage(ctx context.Context) (aztables.ListEntitiesResponse, error) {
	if p.err != nil {
		return aztables.ListEntitiesResponse{}, p.err
	}

	if !p.More() {
		return aztables.ListEntitiesResponse{}, nil
	}

	var entityBytes [][]byte
	for i := 0; i < 2 && p.More(); i++ {
		entityBytes = append(entityBytes, p.pages[p.current])
		p.current++
	}

	return aztables.ListEntitiesResponse{Entities: entityBytes}, nil
}

type mockClient struct {
	azure.AzureTableClient
	addEntity    func(context.Context, []byte, *aztables.AddEntityOptions) (aztables.AddEntityResponse, error)
	getEntity    func(context.Context, string, string, *aztables.GetEntityOptions) (aztables.GetEntityResponse, error)
	updateEntity func(context.Context, []byte, *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error)
	deleteEntity func(context.Context, string, string, *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error)
	pager        *testPager
}

func (m *mockClient) AddEntity(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
	return m.addEntity(ctx, entity, options)
}

func (m *mockClient) GetEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
	return m.getEntity(ctx, partitionKey, rowKey, options)
}

func (m *mockClient) UpdateEntity(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
	return m.updateEntity(ctx, entity, options)
}

func (m *mockClient) DeleteEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
	return m.deleteEntity(ctx, partitionKey, rowKey, options)
}

func (m *mockClient) NewListEntitiesPager(options *aztables.ListEntitiesOptions) azure.ListEntitiesPager {
	return m.pager
}

func TestTableClientOperations(t *testing.T) {
	t.Parallel()

	t.Run("can create and retrieve an entity", func(t *testing.T) {
		t.Parallel()

		mockClient := &mockClient{
			addEntity: func(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
				return aztables.AddEntityResponse{}, nil
			},
			getEntity: func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
				return aztables.GetEntityResponse{
					Value: []byte(`{"Name":"test","Price":10.5,"CreatedAt":"2021-01-01T00:00:00Z","UpdatedAt":"2021-01-01T00:00:00Z"}`),
				}, nil
			},
		}

		repo := azure.NewTestTableRepository("testtable")
		repo.SetTestClient(mockClient)

		item := &models.Item{
			Name:  "test",
			Price: 10.5,
		}
		err := repo.Create(item)
		assert.NoError(t, err)

		var retrieved models.Item
		err = repo.FindByID(1, &retrieved)
		assert.NoError(t, err)
		assert.Equal(t, item.Name, retrieved.Name)
		assert.InDelta(t, item.Price, retrieved.Price, 0.001)
	})

	t.Run("can list entities", func(t *testing.T) {
		t.Parallel()

		testData := [][]byte{
			[]byte(`{"PartitionKey":"items","RowKey":"1","Name":"item1","Price":10.5,"CreatedAt":"2021-01-01T00:00:00Z","UpdatedAt":"2021-01-01T00:00:00Z"}`),
			[]byte(`{"PartitionKey":"items","RowKey":"2","Name":"item2","Price":20.5,"CreatedAt":"2021-01-01T00:00:00Z","UpdatedAt":"2021-01-01T00:00:00Z"}`),
			[]byte(`{"PartitionKey":"items","RowKey":"3","Name":"item3","Price":30.5,"CreatedAt":"2021-01-01T00:00:00Z","UpdatedAt":"2021-01-01T00:00:00Z"}`),
		}

		mockClient := &mockClient{
			pager: &testPager{
				pages: testData,
			},
		}

		repo := azure.NewTestTableRepository("testtable")
		repo.SetTestClient(mockClient)

		var items []models.Item
		err := repo.List(&items)
		assert.NoError(t, err)
		assert.Len(t, items, 3)
		assert.Equal(t, "item1", items[0].Name)
		assert.Equal(t, "item2", items[1].Name)
		assert.Equal(t, "item3", items[2].Name)
	})

	t.Run("can update an entity", func(t *testing.T) {
		t.Parallel()

		mockClient := &mockClient{
			getEntity: func(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
				return aztables.GetEntityResponse{
					Value: []byte(`{"Name":"test","Price":10.5}`),
				}, nil
			},
			updateEntity: func(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
				return aztables.UpdateEntityResponse{}, nil
			},
		}

		repo := azure.NewTestTableRepository("testtable")
		repo.SetTestClient(mockClient)

		item := &models.Item{
			Name:  "test",
			Price: 10.5,
		}
		err := repo.Update(item)
		assert.NoError(t, err)
	})

	t.Run("can delete an entity", func(t *testing.T) {
		t.Parallel()

		mockClient := &mockClient{
			deleteEntity: func(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
				return aztables.DeleteEntityResponse{}, nil
			},
		}

		repo := azure.NewTestTableRepository("testtable")
		repo.SetTestClient(mockClient)

		item := &models.Item{
			Name:  "test",
			Price: 10.5,
		}
		err := repo.Delete(item)
		assert.NoError(t, err)
	})
}

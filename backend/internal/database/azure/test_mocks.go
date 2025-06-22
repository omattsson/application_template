package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)

// mockTablePager is a mock implementation for testing
type mockTablePager struct {
	nextPage int
	pages    [][]byte
}

func (p *mockTablePager) More() bool {
	return p.nextPage < len(p.pages)
}

func (p *mockTablePager) NextPage(ctx context.Context) (aztables.ListEntitiesResponse, error) {
	if p.nextPage >= len(p.pages) {
		return aztables.ListEntitiesResponse{}, nil
	}

	response := aztables.ListEntitiesResponse{
		Entities: [][]byte{p.pages[p.nextPage]},
	}
	p.nextPage++
	return response, nil
}

// mockTableClient is a mock implementation for testing
type mockTableClient struct {
	addEntityFn    func(context.Context, []byte, *aztables.AddEntityOptions) (aztables.AddEntityResponse, error)
	getEntityFn    func(context.Context, string, string, *aztables.GetEntityOptions) (aztables.GetEntityResponse, error)
	updateEntityFn func(context.Context, []byte, *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error)
	deleteEntityFn func(context.Context, string, string, *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error)
	pager          *mockTablePager
}

func newMockTableClient() *mockTableClient {
	return &mockTableClient{}
}

func (m *mockTableClient) AddEntity(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
	if m.addEntityFn != nil {
		return m.addEntityFn(ctx, entity, options)
	}
	return aztables.AddEntityResponse{}, nil
}

func (m *mockTableClient) GetEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
	if m.getEntityFn != nil {
		return m.getEntityFn(ctx, partitionKey, rowKey, options)
	}
	return aztables.GetEntityResponse{}, nil
}

func (m *mockTableClient) UpdateEntity(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
	if m.updateEntityFn != nil {
		return m.updateEntityFn(ctx, entity, options)
	}
	return aztables.UpdateEntityResponse{}, nil
}

func (m *mockTableClient) DeleteEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
	if m.deleteEntityFn != nil {
		return m.deleteEntityFn(ctx, partitionKey, rowKey, options)
	}
	return aztables.DeleteEntityResponse{}, nil
}

func (m *mockTableClient) NewListEntitiesPager(options *aztables.ListEntitiesOptions) ListEntitiesPager {
	return m.pager
}

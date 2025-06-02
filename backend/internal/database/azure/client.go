package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)

// ListEntitiesPager defines the interface for Azure Table pagination
// This allows us to mock pagination in tests
// ListEntitiesPager matches the interface of azcore.Pager[aztables.ListEntitiesResponse]
type ListEntitiesPager interface {
	More() bool
	NextPage(ctx context.Context) (aztables.ListEntitiesResponse, error)
}

// AzureTableClient defines the interface for Azure Table operations
// This allows us to easily mock the client in tests
type AzureTableClient interface {
	AddEntity(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error)
	GetEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error)
	UpdateEntity(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error)
	DeleteEntity(ctx context.Context, partitionKey, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error)
	NewListEntitiesPager(options *aztables.ListEntitiesOptions) ListEntitiesPager
}

// Ensure the built-in client satisfies our interface
// We need to adapt it since our interface is slightly different
type azureClientAdapter struct {
	*aztables.Client
}

func (a *azureClientAdapter) NewListEntitiesPager(options *aztables.ListEntitiesOptions) ListEntitiesPager {
	return &azurePagerAdapter{
		pager: a.Client.NewListEntitiesPager(options),
	}
}

// azurePagerAdapter wraps the Azure Tables client and implements ListEntitiesPager
type azurePagerAdapter struct {
	pager *runtime.Pager[aztables.ListEntitiesResponse]
}

func (p *azurePagerAdapter) More() bool {
	return p.pager.More()
}

func (p *azurePagerAdapter) NextPage(ctx context.Context) (aztables.ListEntitiesResponse, error) {
	return p.pager.NextPage(ctx)
}

func newAzureClientAdapter(client *aztables.Client) AzureTableClient {
	return &azureClientAdapter{Client: client}
}

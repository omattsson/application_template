// Package azure provides implementations for Azure Table Storage operations
// and the necessary interfaces and types for working with Azure Tables.
package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)



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

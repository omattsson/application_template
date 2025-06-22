package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)

// TableClient is an interface that matches the Azure Tables SDK client
type TableClient interface {
	AddEntity(context.Context, []byte, *aztables.AddEntityOptions) (aztables.AddEntityResponse, error)
	GetEntity(context.Context, string, string, *aztables.GetEntityOptions) (aztables.GetEntityResponse, error)
	UpdateEntity(context.Context, []byte, *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error)
	DeleteEntity(context.Context, string, string, *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error)
	NewListEntitiesPager(*aztables.ListEntitiesOptions) ListEntitiesPager
}

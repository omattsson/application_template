package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)

// TableRepository implements the Repository interface for Azure Table Storage
type TableRepository struct {
	client    AzureTableClient
	tableName string
}

// NewTableRepository creates a new Azure Table Storage repository
func NewTableRepository(accountName, accountKey, endpoint, tableName string, useAzurite bool) (*TableRepository, error) {
	var serviceURL string
	if useAzurite {
		serviceURL = fmt.Sprintf("http://%s", endpoint) // Azurite uses http
	} else {
		serviceURL = fmt.Sprintf("https://%s.table.%s", accountName, endpoint)
	}

	if accountName == "" || accountKey == "" {
		return nil, dberrors.NewDatabaseError("azure_client", fmt.Errorf("invalid connection string: missing account name or key"))
	}

	// Create service client
	serviceClient, err := aztables.NewServiceClientFromConnectionString(
		fmt.Sprintf("DefaultEndpointsProtocol=https;AccountName=%s;AccountKey=%s;TableEndpoint=%s",
			accountName,
			accountKey,
			serviceURL),
		nil,
	)
	if err != nil {
		return nil, dberrors.NewDatabaseError("azure_client", err)
	}

	// Get table client
	tableClient := serviceClient.NewClient(tableName)

	// Create table if it doesn't exist
	_, err = serviceClient.CreateTable(context.Background(), tableName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) {
			if respErr.ErrorCode == "TableAlreadyExists" {
				// Table already exists, which is fine
				return &TableRepository{
					client:    newAzureClientAdapter(tableClient),
					tableName: tableName,
				}, nil
			}
			// Return the underlying status code error
			return nil, fmt.Errorf("create_table: %v", respErr.RawResponse.Status)
		}
		// Return other errors as-is
		return nil, dberrors.NewDatabaseError("create_table", err)
	}

	return &TableRepository{
		client:    newAzureClientAdapter(tableClient),
		tableName: tableName,
	}, nil
}

// Create implements the Repository interface
func (r *TableRepository) Create(entity interface{}) error {
	item, ok := entity.(*models.Item)
	if !ok {
		return dberrors.NewDatabaseError("type_assertion", fmt.Errorf("entity must be *models.Item"))
	}

	// Create Azure Table entity
	now := time.Now().UTC()
	entityJson := map[string]interface{}{
		"PartitionKey": "items",
		"RowKey":       strconv.FormatUint(uint64(item.ID), 10),
		"Name":         item.Name,
		"Price":        item.Price,
		"CreatedAt":    now.Format(time.RFC3339),
		"UpdatedAt":    now.Format(time.RFC3339),
	}

	entityBytes, err := json.Marshal(entityJson)
	if err != nil {
		return dberrors.NewDatabaseError("marshal", err)
	}

	// Create the entity
	_, err = r.client.AddEntity(context.Background(), entityBytes, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.ErrorCode == "EntityAlreadyExists" {
			return dberrors.NewDatabaseError("create", dberrors.ErrDuplicateKey)
		}
		return dberrors.NewDatabaseError("create", err)
	}

	item.CreatedAt = now
	item.UpdatedAt = now
	return nil
}

// FindByID implements the Repository interface
func (r *TableRepository) FindByID(id uint, dest interface{}) error {
	item, ok := dest.(*models.Item)
	if !ok {
		return dberrors.NewDatabaseError("type_assertion", fmt.Errorf("dest must be *models.Item"))
	}

	// Get the entity
	result, err := r.client.GetEntity(context.Background(), "items", strconv.FormatUint(uint64(id), 10), nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return dberrors.NewDatabaseError("find", dberrors.ErrNotFound)
		}
		return dberrors.NewDatabaseError("find", err)
	}

	// Parse the entity
	var entityData map[string]interface{}
	if err := json.Unmarshal(result.Value, &entityData); err != nil {
		return dberrors.NewDatabaseError("unmarshal", err)
	}

	// Map entity to item
	item.ID = id
	item.Name = entityData["Name"].(string)
	item.Price = entityData["Price"].(float64)

	createdAt, err := time.Parse(time.RFC3339, entityData["CreatedAt"].(string))
	if err != nil {
		return dberrors.NewDatabaseError("parse_time", err)
	}
	item.CreatedAt = createdAt

	updatedAt, err := time.Parse(time.RFC3339, entityData["UpdatedAt"].(string))
	if err != nil {
		return dberrors.NewDatabaseError("parse_time", err)
	}
	item.UpdatedAt = updatedAt

	return nil
}

// Update implements the Repository interface
func (r *TableRepository) Update(entity interface{}) error {
	item, ok := entity.(*models.Item)
	if !ok {
		return dberrors.NewDatabaseError("type_assertion", fmt.Errorf("entity must be *models.Item"))
	}

	// Check if entity exists first
	_, err := r.client.GetEntity(context.Background(), "items", strconv.FormatUint(uint64(item.ID), 10), nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return dberrors.NewDatabaseError("update", dberrors.ErrNotFound)
		}
		return dberrors.NewDatabaseError("find", err)
	}

	// Create Azure Table entity
	now := time.Now().UTC()
	entityJson := map[string]interface{}{
		"PartitionKey": "items",
		"RowKey":       strconv.FormatUint(uint64(item.ID), 10),
		"Name":         item.Name,
		"Price":        item.Price,
		"CreatedAt":    item.CreatedAt.Format(time.RFC3339),
		"UpdatedAt":    now.Format(time.RFC3339),
	}

	entityBytes, err := json.Marshal(entityJson)
	if err != nil {
		return dberrors.NewDatabaseError("marshal", err)
	}

	// Update the entity
	_, err = r.client.UpdateEntity(context.Background(), entityBytes, nil)
	if err != nil {
		return dberrors.NewDatabaseError("update", err)
	}

	item.UpdatedAt = now
	return nil
}

// Delete implements the Repository interface
func (r *TableRepository) Delete(entity interface{}) error {
	item, ok := entity.(*models.Item)
	if !ok {
		return dberrors.NewDatabaseError("type_assertion", fmt.Errorf("entity must be *models.Item"))
	}

	_, err := r.client.DeleteEntity(context.Background(), "items", strconv.FormatUint(uint64(item.ID), 10), nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return dberrors.NewDatabaseError("delete", dberrors.ErrNotFound)
		}
		return dberrors.NewDatabaseError("delete", err)
	}

	return nil
}

// List implements the Repository interface
func (r *TableRepository) List(dest interface{}, conditions ...interface{}) error {
	items, ok := dest.(*[]models.Item)
	if !ok {
		return dberrors.NewDatabaseError("type_assertion", fmt.Errorf("dest must be *[]models.Item"))
	}

	// Process conditions
	var (
		result             []models.Item
		pagination         *models.Pagination
		nameContainsFilter string
	)

	// Base filter for partition key
	filterParts := []string{"PartitionKey eq 'items'"}

	// Build filters from conditions
	for _, condition := range conditions {
		switch cond := condition.(type) {
		case models.Filter:
			switch cond.Field {
			case "name":
				name := cond.Value.(string)
				if cond.Op == "exact" {
					filterParts = append(filterParts, fmt.Sprintf("Name eq '%s'", name))
				} else {
					nameContainsFilter = strings.ToLower(name)
				}
			case "price":
				price := cond.Value.(float64)
				if cond.Op == ">=" {
					filterParts = append(filterParts, fmt.Sprintf("Price ge %f", price))
				} else if cond.Op == "<=" {
					filterParts = append(filterParts, fmt.Sprintf("Price le %f", price))
				}
			}
		case models.Pagination:
			pagination = &cond
		}
	}

	// Combine all filter parts with AND
	filter := strings.Join(filterParts, " and ")

	// Get pager for table query
	pager := r.client.NewListEntitiesPager(&aztables.ListEntitiesOptions{
		Filter: &filter,
	})

	// Fetch and process all entities
	for pager.More() {
		response, err := pager.NextPage(context.Background())
		if err != nil {
			return dberrors.NewDatabaseError("list", err)
		}

		for _, entityBytes := range response.Entities {
			var entityData map[string]interface{}
			if err := json.Unmarshal(entityBytes, &entityData); err != nil {
				return dberrors.NewDatabaseError("unmarshal", err)
			}

			id, _ := strconv.ParseUint(entityData["RowKey"].(string), 10, 32)
			createdAt, _ := time.Parse(time.RFC3339, entityData["CreatedAt"].(string))
			updatedAt, _ := time.Parse(time.RFC3339, entityData["UpdatedAt"].(string))

			item := models.Item{
				Base: models.Base{
					ID:        uint(id),
					CreatedAt: createdAt,
					UpdatedAt: updatedAt,
				},
				Name:  entityData["Name"].(string),
				Price: entityData["Price"].(float64),
			}

			// Apply name contains filter if specified
			if nameContainsFilter != "" {
				if !strings.Contains(strings.ToLower(item.Name), nameContainsFilter) {
					continue
				}
			}

			result = append(result, item)
		}
	}

	// Apply pagination after all filtering
	if pagination != nil {
		start := pagination.Offset
		if start >= len(result) {
			*items = []models.Item{}
			return nil
		}

		end := start + pagination.Limit
		if end > len(result) {
			end = len(result)
		}
		result = result[start:end]
	}

	*items = result
	return nil
}

// Ping implements the Repository interface
func (r *TableRepository) Ping() error {
	// List tables to check connectivity
	pager := r.client.NewListEntitiesPager(nil)
	_, err := pager.NextPage(context.Background())
	if err != nil {
		return dberrors.NewDatabaseError("ping", err)
	}
	return nil
}

// Helper functions for error handling

func isTableExistsError(err error) bool {
	var respErr *azcore.ResponseError
	return err != nil && errors.As(err, &respErr) && respErr.ErrorCode == "TableAlreadyExists"
}

func isEntityExistsError(err error) bool {
	var respErr *azcore.ResponseError
	return err != nil && errors.As(err, &respErr) && respErr.ErrorCode == "EntityAlreadyExists"
}

func isNotFoundError(err error) bool {
	var respErr *azcore.ResponseError
	return err != nil && errors.As(err, &respErr) && respErr.StatusCode == 404
}

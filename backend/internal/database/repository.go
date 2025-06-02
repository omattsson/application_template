package database

import (
	"fmt"
	"log"

	"backend/internal/config"
	"backend/internal/database/azure"
	"backend/internal/models"
)

// NewRepository creates a new repository based on the configuration
func NewRepository(cfg *config.Config) (models.Repository, error) {
	if cfg.AzureTable.UseAzureTable {
		log.Println("Using Azure Table Storage as repository")
		return azure.NewTableRepository(
			cfg.AzureTable.AccountName,
			cfg.AzureTable.AccountKey,
			cfg.AzureTable.Endpoint,
			cfg.AzureTable.TableName,
			cfg.AzureTable.UseAzurite,
		)
	}

	log.Println("Using MySQL as repository")
	db, err := NewFromAppConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize MySQL database: %w", err)
	}

	return models.NewRepository(db.DB), nil
}

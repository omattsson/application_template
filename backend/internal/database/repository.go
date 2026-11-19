package database

import (
	"fmt"
	"log/slog"

	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/outbox"
)

// repositoryOptions returns the options of the Items repository for cfg. With
// OUTBOX_ENABLED=true the repository writes the Items events to outbox_events
// in the transaction of each change (outbox.ItemHook); with false it has no
// hook and touches no outbox table.
func repositoryOptions(cfg *config.Config) []models.RepositoryOption {
	if cfg == nil || !cfg.Outbox.Enabled {
		return nil
	}
	return []models.RepositoryOption{models.WithChangeHook(outbox.ItemHook(InsertOutboxEvent))}
}

// NewRepository creates a new repository based on the configuration.
// DB_TYPE=sqlite is accepted by config validation for unit-test configs only;
// this production factory always builds MySQL.
func NewRepository(cfg *config.Config) (models.Repository, error) {
	slog.Info("Using MySQL as repository")
	db, err := NewFromAppConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize MySQL database: %w", err)
	}

	// Run database migrations (migrator tracks applied versions; safe on every startup)
	if err := db.AutoMigrate(); err != nil {
		// Clean up the database connection to avoid resource leaks
		if sqlDB, dbErr := db.DB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		return nil, fmt.Errorf("failed to run database migrations: %w", err)
	}

	return models.NewRepository(db.DB, repositoryOptions(cfg)...), nil
}

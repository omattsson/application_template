package schema

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Migration represents a database migration
type Migration struct {
	Version     string
	Name        string
	Up          func(*gorm.DB) error
	Down        func(*gorm.DB) error
	Description string
}

// Migrator handles database schema migrations
type Migrator struct {
	db         *gorm.DB
	migrations []Migration
}

// NewMigrator creates a new migrator instance
func NewMigrator(db *gorm.DB) *Migrator {
	return &Migrator{
		db:         db,
		migrations: make([]Migration, 0),
	}
}

// AddMigration adds a new migration to the migrator
func (m *Migrator) AddMigration(migration Migration) {
	m.migrations = append(m.migrations, migration)
}

// MigrateUp runs all pending migrations
func (m *Migrator) MigrateUp() error {
	// Ensure schema version table exists
	if err := m.db.AutoMigrate(&SchemaVersion{}); err != nil {
		return fmt.Errorf("failed to create schema version table: %v", err)
	}

	for _, migration := range m.migrations {
		var version SchemaVersion
		result := m.db.Where("version = ?", migration.Version).First(&version)

		if result.Error == gorm.ErrRecordNotFound {
			// Run migration in transaction
			err := m.db.Transaction(func(tx *gorm.DB) error {
				if err := migration.Up(tx); err != nil {
					return err
				}

				// Record migration
				version := SchemaVersion{
					Version:   migration.Version,
					Name:      migration.Name,
					AppliedAt: time.Now(),
				}
				return tx.Create(&version).Error
			})

			if err != nil {
				return fmt.Errorf("failed to apply migration %s: %v", migration.Version, err)
			}
		}
	}
	return nil
}

// MigrateDown rolls back the last migration
func (m *Migrator) MigrateDown() error {
	var lastVersion SchemaVersion
	if err := m.db.Order("applied_at desc").First(&lastVersion).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil // No migrations to roll back
		}
		return fmt.Errorf("failed to get last migration: %v", err)
	}

	for i := len(m.migrations) - 1; i >= 0; i-- {
		migration := m.migrations[i]
		if migration.Version == lastVersion.Version {
			// Run rollback in transaction
			err := m.db.Transaction(func(tx *gorm.DB) error {
				if err := migration.Down(tx); err != nil {
					return err
				}
				return tx.Delete(&lastVersion).Error
			})

			if err != nil {
				return fmt.Errorf("failed to roll back migration %s: %v", migration.Version, err)
			}
			return nil
		}
	}
	return nil
}

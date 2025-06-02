package database

import (
	"log"

	"backend/internal/database/schema"
	"backend/internal/models"

	"gorm.io/gorm"
)

// AutoMigrate runs database migrations for all models
func (d *Database) AutoMigrate() error {
	log.Println("Running database migrations...")

	// Initialize migrator
	migrator := schema.NewMigrator(d.DB)

	// Add migrations
	migrator.AddMigration(schema.Migration{
		Version:     "20231201000001",
		Name:        "create_base_tables",
		Description: "Create initial user and item tables",
		Up: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&models.User{}, &models.Item{})
		},
		Down: func(tx *gorm.DB) error {
			return tx.Migrator().DropTable(&models.Item{}, &models.User{})
		},
	})

	// Example of adding indexes and constraints in a separate migration
	migrator.AddMigration(schema.Migration{
		Version:     "20231201000002",
		Name:        "add_indexes",
		Description: "Add indexes for performance optimization",
		Up: func(tx *gorm.DB) error {
			// Add composite index on items
			err := tx.Exec("CREATE INDEX idx_items_name_price ON items(name, price)").Error
			if err != nil {
				return err
			}

			// Add unique index on email (uniqueness already enforced by schema)
			return tx.Exec("CREATE INDEX idx_users_email ON users(email)").Error
		},
		Down: func(tx *gorm.DB) error {
			if err := tx.Exec("DROP INDEX idx_items_name_price ON items").Error; err != nil {
				return err
			}
			return tx.Exec("DROP INDEX idx_users_email ON users").Error
		},
	})

	// Run migrations
	if err := migrator.MigrateUp(); err != nil {
		return err
	}

	log.Println("Database migrations completed successfully")
	return nil
}

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
			var count int64
			tx.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'items' AND index_name = 'idx_items_name_price'").Scan(&count)
			if count == 0 {
				if err := tx.Exec("CREATE INDEX idx_items_name_price ON items(name, price)").Error; err != nil {
					return err
				}
			}

			// Add unique index on email (uniqueness already enforced by schema)
			tx.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'users' AND index_name = 'idx_users_email'").Scan(&count)
			if count == 0 {
				if err := tx.Exec("CREATE INDEX idx_users_email ON users(email)").Error; err != nil {
					return err
				}
			}
			return nil
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

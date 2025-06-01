package database

import (
	"log"

	"backend/internal/models"
)

// AutoMigrate runs database migrations for all models
func (d *Database) AutoMigrate() error {
	log.Println("Running database migrations...")

	if err := d.DB.AutoMigrate(
		&models.User{},
		&models.Item{},
	); err != nil {
		return err
	}

	log.Println("Database migrations completed successfully")
	return nil
}

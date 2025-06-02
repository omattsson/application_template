package database

import (
	"context"
	"fmt"
	"log"
	"time"

	"backend/internal/config"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	maxRetries = 5
	retryDelay = 2 * time.Second
)

// NewFromAppConfig creates a new database instance from application config
func NewFromAppConfig(cfg *config.Config) (*Database, error) {
	// Set up logging based on environment
	logLevel := logger.Info
	if !cfg.App.Debug {
		logLevel = logger.Error
	}

	// Initialize database with retries
	var db *gorm.DB
	var err error
	var retryCount int

	for retryCount < maxRetries {
		db, err = gorm.Open(mysql.Open(cfg.Database.DSN()), &gorm.Config{
			Logger: logger.Default.LogMode(logLevel),
		})

		if err == nil {
			break
		}

		retryCount++
		if retryCount < maxRetries {
			log.Printf("Failed to connect to database (attempt %d/%d): %v. Retrying in %v...",
				retryCount, maxRetries, err, retryDelay)
			time.Sleep(retryDelay)
		}
	}

	if err != nil {
		return nil, NewDatabaseError("connect", fmt.Errorf("failed after %d attempts: %v", retryCount, err))
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, NewDatabaseError("configure", err)
	}

	// Set connection pool settings from config
	sqlDB.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, NewDatabaseError("ping", err)
	}

	database := &Database{DB: db}

	// Enable foreign key checks and set other important MySQL settings
	if err := database.configure(); err != nil {
		return nil, err
	}

	return database, nil
}

// configure sets up important database settings
func (d *Database) configure() error {
	// Enable foreign key checks
	if err := d.Exec("SET FOREIGN_KEY_CHECKS = 1").Error; err != nil {
		return NewDatabaseError("configure", err)
	}

	// Set explicit default timezone to UTC
	if err := d.Exec("SET time_zone = '+00:00'").Error; err != nil {
		return NewDatabaseError("configure", err)
	}

	// Set SQL mode to strict
	if err := d.Exec("SET SESSION sql_mode = 'STRICT_TRANS_TABLES,NO_AUTO_VALUE_ON_ZERO,NO_ENGINE_SUBSTITUTION'").Error; err != nil {
		return NewDatabaseError("configure", err)
	}

	return nil
}

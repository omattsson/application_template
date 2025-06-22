package database

import (
	"log"
	"strings"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Database struct {
	*gorm.DB
}

func NewDatabase(dsn string, logger logger.Interface) (*Database, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger,
	})
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, NewDatabaseError("connect", ErrConnectionFailed)
		}
		return nil, NewDatabaseError("connect", err)
	}

	// Configure connection pool
	sqlDB, err := db.DB()
	if err != nil {
		return nil, NewDatabaseError("configure", err)
	}

	// Set reasonable defaults for the connection pool
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetMaxOpenConns(20)

	log.Println("Connected to database successfully")
	return &Database{DB: db}, nil
}

// Transaction executes operations within a database transaction
func (d *Database) Transaction(fn func(tx *gorm.DB) error) error {
	err := d.DB.Transaction(fn)
	if err != nil {
		return NewDatabaseError("transaction", err)
	}
	return nil
}

// HandleError translates GORM/MySQL errors into our custom error types
func (d *Database) HandleError(op string, err error) error {
	if err == nil {
		return nil
	}

	if err == gorm.ErrRecordNotFound {
		return NewDatabaseError(op, ErrNotFound)
	}

	// Check for duplicate key violations
	if strings.Contains(err.Error(), "Duplicate entry") {
		return NewDatabaseError(op, ErrDuplicateKey)
	}

	// Handle validation errors
	if strings.Contains(err.Error(), "validation failed") {
		return NewDatabaseError(op, ErrValidation)
	}

	return NewDatabaseError(op, err)
}

// Create implements models.Repository
func (d *Database) Create(value interface{}) error {
	result := d.DB.Create(value)
	if result.Error != nil {
		return d.HandleError("create", result.Error)
	}
	return nil
}

// FindByID implements models.Repository
func (d *Database) FindByID(id uint, dest interface{}) error {
	result := d.DB.First(dest, id)
	if result.Error != nil {
		return d.HandleError("find", result.Error)
	}
	return nil
}

// Update implements models.Repository
func (d *Database) Update(value interface{}) error {
	result := d.DB.Save(value)
	if result.Error != nil {
		return d.HandleError("update", result.Error)
	}
	return nil
}

// Delete implements models.Repository
func (d *Database) Delete(value interface{}) error {
	result := d.DB.Delete(value)
	if result.Error != nil {
		return d.HandleError("delete", result.Error)
	}
	return nil
}

// List implements models.Repository
func (d *Database) List(dest interface{}, conditions ...interface{}) error {
	result := d.DB.Find(dest)
	if result.Error != nil {
		return d.HandleError("list", result.Error)
	}
	return nil
}

// Ping implements models.Repository
func (d *Database) Ping() error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return d.HandleError("ping", err)
	}
	return sqlDB.Ping()
}

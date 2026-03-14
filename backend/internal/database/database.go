package database

import (
	"log/slog"
	"strings"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Database wraps a gorm.DB instance with additional utilities.
type Database struct {
	*gorm.DB
}

// NewDatabase creates a new database connection. Callers should configure
// connection pool settings via the returned *sql.DB if needed.
func NewDatabase(dsn string, logCfg logger.Interface) (*Database, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logCfg,
	})
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, NewDatabaseError("connect", ErrConnectionFailed)
		}
		return nil, NewDatabaseError("connect", err)
	}

	slog.Info("Connected to database successfully")
	return &Database{DB: db}, nil
}

// Transaction executes operations within a database transaction.
func (d *Database) Transaction(fn func(tx *gorm.DB) error) error {
	err := d.DB.Transaction(fn)
	if err != nil {
		return NewDatabaseError("transaction", err)
	}
	return nil
}

// Ping checks if the database connection is alive.
func (d *Database) Ping() error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}

package database

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// SchemaManager handles database schema operations
type SchemaManager interface {
	// CreateTable creates a new table for the given model if it doesn't exist
	CreateTable(model interface{}) error
	// DropTable drops the table for the given model if it exists
	DropTable(model interface{}) error
	// HasTable checks if a table exists for the given model
	HasTable(model interface{}) (bool, error)
	// AddColumn adds a new column to the table
	AddColumn(model interface{}, field string, dataType string) error
	// DropColumn drops a column from the table
	DropColumn(model interface{}, field string) error
	// HasColumn checks if a column exists in the table
	HasColumn(model interface{}, field string) (bool, error)
	// AddIndex adds an index to the table
	AddIndex(model interface{}, name string, columns ...string) error
	// DropIndex removes an index from the table
	DropIndex(model interface{}, name string) error
}

// GormSchemaManager implements SchemaManager using GORM
type GormSchemaManager struct {
	db *gorm.DB
}

// NewSchemaManager creates a new schema manager
func NewSchemaManager(db *gorm.DB) SchemaManager {
	return &GormSchemaManager{db: db}
}

func (m *GormSchemaManager) CreateTable(model interface{}) error {
	return m.db.Migrator().CreateTable(model)
}

func (m *GormSchemaManager) DropTable(model interface{}) error {
	return m.db.Migrator().DropTable(model)
}

func (m *GormSchemaManager) HasTable(model interface{}) (bool, error) {
	return m.db.Migrator().HasTable(model), nil
}

func (m *GormSchemaManager) AddColumn(model interface{}, field string, dataType string) error {
	migrator := m.db.Migrator()

	// Check if column already exists
	if migrator.HasColumn(model, field) {
		return fmt.Errorf("column %s already exists", field)
	}

	// Parse model to get table name
	stmt := &gorm.Statement{DB: m.db}
	if err := stmt.Parse(model); err != nil {
		return fmt.Errorf("failed to parse model: %v", err)
	}

	// Use a transaction for DDL operation
	return m.db.Transaction(func(tx *gorm.DB) error {
		// Create the column using parameterized query
		sql := "ALTER TABLE ? ADD COLUMN ? ?"
		if err := tx.Exec(sql, gorm.Expr(stmt.Table), gorm.Expr(field), gorm.Expr(dataType)).Error; err != nil {
			return fmt.Errorf("failed to add column %s: %v", field, err)
		}
		return nil
	})
}

func (m *GormSchemaManager) DropColumn(model interface{}, field string) error {
	migrator := m.db.Migrator()
	if !migrator.HasColumn(model, field) {
		return fmt.Errorf("column %s does not exist", field)
	}

	// Parse model to get table name
	stmt := &gorm.Statement{DB: m.db}
	if err := stmt.Parse(model); err != nil {
		return fmt.Errorf("failed to parse model: %v", err)
	}

	// Use a transaction for DDL operation
	return m.db.Transaction(func(tx *gorm.DB) error {
		// Drop column using parameterized query
		sql := "ALTER TABLE ? DROP COLUMN ?"
		if err := tx.Exec(sql, gorm.Expr(stmt.Table), gorm.Expr(field)).Error; err != nil {
			return fmt.Errorf("failed to drop column %s: %v", field, err)
		}
		return nil
	})
}

func (m *GormSchemaManager) HasColumn(model interface{}, field string) (bool, error) {
	return m.db.Migrator().HasColumn(model, field), nil
}

func (m *GormSchemaManager) AddIndex(model interface{}, name string, columns ...string) error {
	migrator := m.db.Migrator()

	// Check if index already exists
	if migrator.HasIndex(model, name) {
		return fmt.Errorf("index %s already exists", name)
	}

	// Get table name
	stmt := &gorm.Statement{DB: m.db}
	if err := stmt.Parse(model); err != nil {
		return fmt.Errorf("failed to parse model: %v", err)
	}

	// Validate columns exist
	for _, column := range columns {
		if !migrator.HasColumn(model, column) {
			return fmt.Errorf("column %s does not exist", column)
		}
	}

	// Use a transaction for DDL operation
	return m.db.Transaction(func(tx *gorm.DB) error {
		// Build column list safely
		columnList := make([]string, len(columns))
		for i, col := range columns {
			columnList[i] = col
		}

		// Create index using parameterized query
		sql := "CREATE INDEX ? ON ? (?)"
		if err := tx.Exec(sql,
			gorm.Expr(name),
			gorm.Expr(stmt.Table),
			gorm.Expr(strings.Join(columnList, ", ")),
		).Error; err != nil {
			return fmt.Errorf("failed to create index %s: %v", name, err)
		}
		return nil
	})
}

func (m *GormSchemaManager) DropIndex(model interface{}, name string) error {
	migrator := m.db.Migrator()
	if !migrator.HasIndex(model, name) {
		return fmt.Errorf("index %s does not exist", name)
	}
	return migrator.DropIndex(model, name)
}

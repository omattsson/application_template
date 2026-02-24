package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"backend/pkg/dberrors"

	"gorm.io/gorm"
)

// Base model with common fields
type Base struct {
	ID        uint       `gorm:"primarykey" json:"id" example:"1" description:"@Description Unique identifier"`
	CreatedAt time.Time  `json:"created_at" example:"2025-06-02T10:00:00Z" description:"@Description Creation timestamp"`
	UpdatedAt time.Time  `json:"updated_at" example:"2025-06-02T10:00:00Z" description:"@Description Last update timestamp"`
	DeletedAt *time.Time `gorm:"index" json:"deleted_at,omitempty" format:"date-time" description:"@Description Soft delete timestamp"`
}

// Item represents a basic item in the system
type Item struct {
	Base
	Name    string  `gorm:"size:255;not null" json:"name"`
	Price   float64 `json:"price"`
	Version uint    `gorm:"not null;default:1" json:"version"` // For optimistic locking (1 = initial; 0 = not provided)
}

// User represents a user in the system
type User struct {
	Base
	Username string `gorm:"size:255;not null;unique" json:"username"`
	Email    string `gorm:"size:255;not null;unique" json:"email"`
	Name     string `gorm:"size:255" json:"name"`
}

// Validator is an interface for model validation
type Validator interface {
	Validate() error
}

// Versionable is an interface for models that support optimistic locking.
type Versionable interface {
	GetVersion() uint
	SetVersion(v uint)
}

// GetVersion implements Versionable for Item.
func (i *Item) GetVersion() uint { return i.Version }

// SetVersion implements Versionable for Item.
func (i *Item) SetVersion(v uint) { i.Version = v }

// Repository defines the interface for database operations.
type Repository interface {
	Create(ctx context.Context, entity interface{}) error
	FindByID(ctx context.Context, id uint, dest interface{}) error
	Update(ctx context.Context, entity interface{}) error
	Delete(ctx context.Context, entity interface{}) error
	List(ctx context.Context, dest interface{}, conditions ...interface{}) error
	Ping(ctx context.Context) error
}

// GenericRepository implements the Repository interface
type GenericRepository struct {
	db *gorm.DB
}

// NewRepository creates a new GenericRepository
func NewRepository(db *gorm.DB) Repository {
	return &GenericRepository{db: db}
}

// Ping checks if the database is reachable
func (r *GenericRepository) Ping(ctx context.Context) error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func (r *GenericRepository) Create(ctx context.Context, entity interface{}) error {
	if v, ok := entity.(Validator); ok {
		if err := v.Validate(); err != nil {
			return dberrors.NewDatabaseError("validate", err)
		}
	}

	if err := r.db.WithContext(ctx).Create(entity).Error; err != nil {
		return r.handleError("create", err)
	}
	return nil
}

func (r *GenericRepository) FindByID(ctx context.Context, id uint, dest interface{}) error {
	if err := r.db.WithContext(ctx).First(dest, id).Error; err != nil {
		return r.handleError("find", err)
	}
	return nil
}

func (r *GenericRepository) Update(ctx context.Context, entity interface{}) error {
	if v, ok := entity.(Validator); ok {
		if err := v.Validate(); err != nil {
			return dberrors.NewDatabaseError("validate", err)
		}
	}

	// Optimistic locking for Versionable entities
	if ver, ok := entity.(Versionable); ok {
		currentVersion := ver.GetVersion()
		ver.SetVersion(currentVersion + 1)
		result := r.db.WithContext(ctx).Where("version = ?", currentVersion).Save(entity)
		if result.Error != nil {
			ver.SetVersion(currentVersion) // Roll back version on error
			return r.handleError("update", result.Error)
		}
		if result.RowsAffected == 0 {
			ver.SetVersion(currentVersion) // Roll back version on mismatch
			return dberrors.NewDatabaseError("update", errors.New("version mismatch"))
		}
		return nil
	}

	if err := r.db.WithContext(ctx).Save(entity).Error; err != nil {
		return r.handleError("update", err)
	}
	return nil
}

func (r *GenericRepository) Delete(ctx context.Context, entity interface{}) error {
	if err := r.db.WithContext(ctx).Delete(entity).Error; err != nil {
		return r.handleError("delete", err)
	}
	return nil
}

// allowedFilterFields is a whitelist of column names that may be used in filter queries.
// This prevents SQL injection via the Filter.Field parameter.
var allowedFilterFields = map[string]bool{
	"name":  true,
	"price": true,
}

func (r *GenericRepository) List(ctx context.Context, dest interface{}, conditions ...interface{}) error {
	query := r.db.WithContext(ctx)
	for _, cond := range conditions {
		switch c := cond.(type) {
		case Filter:
			if !allowedFilterFields[c.Field] {
				return dberrors.NewDatabaseError("list",
					fmt.Errorf("invalid filter field: %q", c.Field))
			}
			switch c.Op {
			case "exact":
				query = query.Where(fmt.Sprintf("%s = ?", c.Field), c.Value)
			case ">=":
				query = query.Where(fmt.Sprintf("%s >= ?", c.Field), c.Value)
			case "<=":
				query = query.Where(fmt.Sprintf("%s <= ?", c.Field), c.Value)
			default:
				// Default to LIKE for substring matching
				query = query.Where(fmt.Sprintf("%s LIKE ?", c.Field), fmt.Sprintf("%%%v%%", c.Value))
			}
		case Pagination:
			if c.Limit > 0 {
				query = query.Limit(c.Limit)
			}
			if c.Offset > 0 {
				query = query.Offset(c.Offset)
			}
		}
	}
	if err := query.Find(dest).Error; err != nil {
		return r.handleError("list", err)
	}
	return nil
}

// handleError translates database errors into our custom error types
func (r *GenericRepository) handleError(op string, err error) error {
	if err == nil {
		return nil
	}

	switch {
	case err == gorm.ErrRecordNotFound:
		return dberrors.NewDatabaseError(op, dberrors.ErrNotFound)
	case strings.Contains(err.Error(), "Duplicate entry"):
		return dberrors.NewDatabaseError(op, dberrors.ErrDuplicateKey)
	case strings.Contains(err.Error(), "validation failed"):
		return dberrors.NewDatabaseError(op, dberrors.ErrValidation)
	default:
		return dberrors.NewDatabaseError(op, err)
	}
}

// Filter represents a filter condition for queries
type Filter struct {
	Field string      `json:"field"`
	Op    string      `json:"op,omitempty"`
	Value interface{} `json:"value"`
}

// Pagination represents pagination parameters for queries
type Pagination struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

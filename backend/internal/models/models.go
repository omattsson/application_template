package models

import (
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
	Name  string  `gorm:"size:255;not null" json:"name"`
	Price float64 `json:"price"`
}

// User represents a user in the system
type User struct {
	Base
	Username string `gorm:"size:255;not null;unique" json:"username"`
	Email    string `gorm:"size:255;not null;unique" json:"email"`
	Name     string `gorm:"size:255" json:"name"`
}

// Repository defines the interface for database operations
type Repository interface {
	Create(interface{}) error
	FindByID(id uint, dest interface{}) error
	Update(interface{}) error
	Delete(interface{}) error
	List(dest interface{}, conditions ...interface{}) error
}

// GenericRepository implements the Repository interface
type GenericRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) Repository {
	return &GenericRepository{db: db}
}

// Validator is an interface for model validation
type Validator interface {
	Validate() error
}

func (r *GenericRepository) Create(entity interface{}) error {
	if v, ok := entity.(Validator); ok {
		if err := v.Validate(); err != nil {
			return dberrors.NewDatabaseError("validate", err)
		}
	}

	if err := r.db.Create(entity).Error; err != nil {
		return r.handleError("create", err)
	}
	return nil
}

func (r *GenericRepository) FindByID(id uint, dest interface{}) error {
	if err := r.db.First(dest, id).Error; err != nil {
		return r.handleError("find", err)
	}
	return nil
}

func (r *GenericRepository) Update(entity interface{}) error {
	if v, ok := entity.(Validator); ok {
		if err := v.Validate(); err != nil {
			return dberrors.NewDatabaseError("validate", err)
		}
	}

	if err := r.db.Save(entity).Error; err != nil {
		return r.handleError("update", err)
	}
	return nil
}

func (r *GenericRepository) Delete(entity interface{}) error {
	if err := r.db.Delete(entity).Error; err != nil {
		return r.handleError("delete", err)
	}
	return nil
}

func (r *GenericRepository) List(dest interface{}, conditions ...interface{}) error {
	if err := r.db.Find(dest, conditions...).Error; err != nil {
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

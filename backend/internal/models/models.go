package models

import (
	"time"

	"gorm.io/gorm"
)

// Base contains common columns for all tables
type Base struct {
	ID        uint           `gorm:"primarykey" json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// Item represents a single item in the application.
type Item struct {
	Base
	Name  string  `gorm:"size:255;not null" json:"name"`
	Price float64 `gorm:"type:decimal(10,2);not null" json:"price"`
}

// User represents a user in the application.
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

func (r *GenericRepository) Create(entity interface{}) error {
	return r.db.Create(entity).Error
}

func (r *GenericRepository) FindByID(id uint, dest interface{}) error {
	return r.db.First(dest, id).Error
}

func (r *GenericRepository) Update(entity interface{}) error {
	return r.db.Save(entity).Error
}

func (r *GenericRepository) Delete(entity interface{}) error {
	return r.db.Delete(entity).Error
}

func (r *GenericRepository) List(dest interface{}, conditions ...interface{}) error {
	return r.db.Find(dest, conditions...).Error
}

package schema

import (
	"time"

	"gorm.io/gorm"
)

// SchemaVersion tracks database migrations
type SchemaVersion struct {
	ID        uint           `gorm:"primarykey"`
	Version   string         `gorm:"size:50;uniqueIndex;not null"`
	Name      string         `gorm:"size:255;not null"`
	AppliedAt time.Time      `gorm:"not null"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

package models

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Test struct to demonstrate model testing
type TestModel struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func TestModelValidation(t *testing.T) {
	t.Run("Test model creation with valid data", func(t *testing.T) {
		model := TestModel{
			ID:        "test-id",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}

		assert.NotEmpty(t, model.ID)
		assert.False(t, model.CreatedAt.IsZero())
		assert.False(t, model.UpdatedAt.IsZero())
	})
}

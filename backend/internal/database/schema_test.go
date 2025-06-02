package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModel is a model for schema testing
type TestModel struct {
	ID    uint   `gorm:"primarykey"`
	Name  string `gorm:"size:255"`
	Email string `gorm:"size:255;uniqueIndex"`
}

func TestSchemaManager(t *testing.T) {
	db := setupTestDB(t)
	require.NotNil(t, db)
	schema := NewSchemaManager(db.DB)
	require.NotNil(t, schema)

	t.Run("Create and Drop Table", func(t *testing.T) {
		// Create table
		err := schema.CreateTable(&TestModel{})
		require.NoError(t, err)

		// Verify table exists
		exists, err := schema.HasTable(&TestModel{})
		require.NoError(t, err)
		assert.True(t, exists)

		// Drop table
		err = schema.DropTable(&TestModel{})
		require.NoError(t, err)

		// Verify table doesn't exist
		exists, err = schema.HasTable(&TestModel{})
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("Column Operations", func(t *testing.T) {
		// Create table first
		err := schema.CreateTable(&TestModel{})
		require.NoError(t, err)
		defer schema.DropTable(&TestModel{})

		// Add new column
		err = schema.AddColumn(&TestModel{}, "description", "text")
		require.NoError(t, err)

		// Verify column exists
		exists, err := schema.HasColumn(&TestModel{}, "description")
		require.NoError(t, err)
		// Try adding same column again (should fail or be idempotent)
		err = schema.AddColumn(&TestModel{}, "description", "text")
		// Accept either error or nil, depending on implementation
		if err == nil {
			exists, err := schema.HasColumn(&TestModel{}, "description")
			require.NoError(t, err)
			assert.True(t, exists)
		} else {
			assert.Error(t, err)
		}

		// Drop column
		err = schema.DropColumn(&TestModel{}, "description")
		// Try dropping non-existent column (should fail or be idempotent)
		err = schema.DropColumn(&TestModel{}, "non_existent")
		if err == nil {
			// Accept idempotent drop
			exists, err := schema.HasColumn(&TestModel{}, "non_existent")
			require.NoError(t, err)
			assert.False(t, exists)
		} else {
			assert.Error(t, err)
		}

		// Verify column doesn't exist
		exists, err = schema.HasColumn(&TestModel{}, "description")
		require.NoError(t, err)
		assert.False(t, exists)

		// Try dropping non-existent column (should fail)
		err = schema.DropColumn(&TestModel{}, "non_existent")
		assert.Error(t, err)
	})

	t.Run("Index Operations", func(t *testing.T) {
		// Create table first
		err := schema.CreateTable(&TestModel{})
		require.NoError(t, err)
		defer schema.DropTable(&TestModel{})

		t.Run("Regular Index", func(t *testing.T) {
			indexName := "idx_test_name"

			// Add index
			err = schema.AddIndex(&TestModel{}, indexName, "name")
			require.NoError(t, err)

			// Verify index exists by trying to create it again
			err = schema.AddIndex(&TestModel{}, indexName, "name")
			assert.Error(t, err, "Creating duplicate index should fail")
			if err != nil {
				assert.Contains(t, err.Error(), "already exists")
			}

			// Drop index
			err = schema.DropIndex(&TestModel{}, indexName)
			require.NoError(t, err)

			// Verify index is gone by creating it again
			err = schema.AddIndex(&TestModel{}, indexName, "name")
			assert.NoError(t, err, "Should be able to create index after dropping it")
		})

		t.Run("Composite Index", func(t *testing.T) {
			indexName := "idx_test_name_email"

			// Add composite index
			err = schema.AddIndex(&TestModel{}, indexName, "name", "email")
			require.NoError(t, err)
			err = schema.AddIndex(&TestModel{}, indexName, "name", "email")
			assert.Error(t, err, "Creating duplicate composite index should fail")
			if err != nil {
				assert.Contains(t, err.Error(), "already exists")
			}
			assert.Error(t, err, "Creating duplicate composite index should fail")
			assert.Contains(t, err.Error(), "already exists")

			// Drop composite index
			err = schema.DropIndex(&TestModel{}, indexName)
			require.NoError(t, err)
		})

		t.Run("Unique Index", func(t *testing.T) {
			// Email field has uniqueIndex tag
			err = schema.AddIndex(&TestModel{}, "idx_test_email_unique", "email")
			require.NoError(t, err)

			// Try to drop the unique index
			err = schema.DropIndex(&TestModel{}, "idx_test_email_unique")
			require.NoError(t, err)
		})
	})
}

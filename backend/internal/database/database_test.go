package database

import (
	"testing"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) *Database {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect to database: %v", err)
	}
	return &Database{DB: db}
}

func TestNewDatabase(t *testing.T) {
	db := setupTestDB(t)
	assert.NotNil(t, db)

	sqlDB, err := db.DB.DB()
	assert.NoError(t, err)
	assert.NoError(t, sqlDB.Ping())
}

func TestDatabaseMigrations(t *testing.T) {
	db := setupTestDB(t)

	// Run migrations
	err := db.AutoMigrate()
	assert.NoError(t, err, "Migrations should run successfully")

	// Verify that tables were created
	tables, err := db.DB.Migrator().GetTables()
	assert.NoError(t, err)

	expectedTables := []string{"users", "items"}
	for _, table := range expectedTables {
		assert.Contains(t, tables, table)
	}
}

func TestDatabaseTransaction(t *testing.T) {
	db := setupTestDB(t)

	// Run migrations first
	err := db.AutoMigrate()
	assert.NoError(t, err)

	t.Run("Successful Transaction", func(t *testing.T) {
		err := db.Transaction(func(tx *gorm.DB) error {
			user := &models.User{
				Username: "test_user",
				Email:    "test@example.com",
				Name:     "Test User",
			}
			return tx.Create(user).Error
		})
		assert.NoError(t, err)

		var user models.User
		err = db.First(&user, "username = ?", "test_user").Error
		assert.NoError(t, err)
		assert.Equal(t, "test@example.com", user.Email)
	})

	t.Run("Failed Transaction", func(t *testing.T) {
		err := db.Transaction(func(tx *gorm.DB) error {
			user := &models.User{
				Username: "test_user", // Duplicate username should cause error
				Email:    "another@example.com",
				Name:     "Another User",
			}
			return tx.Create(user).Error
		})
		assert.Error(t, err, "Should fail due to unique constraint violation")

		var count int64
		db.Model(&models.User{}).Count(&count)
		assert.Equal(t, int64(1), count, "Should still have only one user")
	})
}

func TestItemCRUD(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate())

	t.Run("Create Item", func(t *testing.T) {
		item := &models.Item{
			Name:  "Test Item",
			Price: 99.99,
		}
		err := db.Create(item)
		assert.NoError(t, err)
		assert.NotZero(t, item.ID)
	})

	t.Run("Read Item", func(t *testing.T) {
		var item models.Item
		err := db.FindByID(1, &item)
		assert.NoError(t, err)
		assert.Equal(t, "Test Item", item.Name)
		assert.Equal(t, 99.99, item.Price)
	})

	t.Run("Update Item", func(t *testing.T) {
		var item models.Item
		err := db.FindByID(1, &item)
		require.NoError(t, err)
		item.Price = 199.99
		err = db.Update(&item)
		assert.NoError(t, err)

		var updatedItem models.Item
		err = db.FindByID(1, &updatedItem)
		assert.Equal(t, 199.99, updatedItem.Price)
	})

	t.Run("Delete Item", func(t *testing.T) {
		item := &models.Item{Base: models.Base{ID: 1}}
		err := db.Delete(item)
		assert.NoError(t, err)

		var deleted models.Item
		err = db.FindByID(1, &deleted)
		assert.Error(t, err, "Should not find deleted item")
	})
}

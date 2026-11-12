package handlers

import (
	"context"
	"net/http"
	"testing"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// uniqueUser is a test-only model with a unique username.
type uniqueUser struct {
	ID       uint   `gorm:"primaryKey"`
	Username string `gorm:"size:64;not null;uniqueIndex"`
}

// A duplicate username on SQLite reaches the client as 409 through the real
// repository error mapping and mapError, without the duplicate value.
func TestDuplicateKeyOnSQLiteMapsTo409(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&uniqueUser{}))

	repo := models.NewRepository(db)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &uniqueUser{Username: "alice"}))

	err = repo.Create(ctx, &uniqueUser{Username: "alice"})
	status, msg := mapError(err, "User")

	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "User already exists", msg)
}

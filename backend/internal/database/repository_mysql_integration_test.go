//go:build integration

package database

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mysqlTestConfig(debug bool) *config.Config {
	getenv := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return &config.Config{
		App: config.AppConfig{Name: "testapp", Environment: "testing", Debug: debug},
		Database: config.DatabaseConfig{
			Host:            getenv("DB_HOST", "localhost"),
			Port:            getenv("DB_PORT", "3306"),
			User:            getenv("DB_USER", "appuser"),
			Password:        getenv("DB_PASSWORD", "apppass"),
			DBName:          getenv("DB_NAME", "app"),
			MaxOpenConns:    5,
			MaxIdleConns:    2,
			ConnMaxLifetime: 5 * time.Minute,
		},
	}
}

// TestDatabaseMySQLItemsRepository runs ListPaged (LIKE with ESCAPE), the
// version check and Create/Delete on MySQL with the release-mode GORM logger.
func TestDatabaseMySQLItemsRepository(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB)
	ctx := context.Background()

	prefix := fmt.Sprintf("lp-%d-", time.Now().UnixNano())
	t.Cleanup(func() { db.DB.Where("name LIKE ?", prefix+"%").Delete(&models.Item{}) })

	for i, name := range []string{"apple", "apple pie", "100%_sure"} {
		it := &models.Item{Name: prefix + name, Price: float64(i + 1)}
		require.NoError(t, repo.Create(ctx, it))
	}

	var got []models.Item
	total, err := repo.ListPaged(ctx, &got, 1, 1, models.ItemFilter{Name: prefix + "app"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	require.Len(t, got, 1)
	assert.Equal(t, prefix+"apple pie", got[0].Name)

	got = nil
	total, err = repo.ListPaged(ctx, &got, 0, 0, models.ItemFilter{Name: prefix + "100%_"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "% and _ are literals")

	// Stale version: mismatch error once, in-memory version rolled back.
	var a, b models.Item
	require.NoError(t, repo.FindByID(ctx, got[0].ID, &a))
	require.NoError(t, repo.FindByID(ctx, got[0].ID, &b))
	require.NoError(t, repo.Update(ctx, &a))
	err = repo.Update(ctx, &b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version mismatch")
	assert.Equal(t, uint(1), b.Version)

	require.NoError(t, repo.Delete(ctx, &models.Item{Base: models.Base{ID: a.ID}}))
}

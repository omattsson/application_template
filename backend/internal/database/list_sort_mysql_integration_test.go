//go:build integration

package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDatabaseSortIndexes checks on MySQL that the sort migration created its
// indexes, that a second Up and a Down and Up again keep them, and that every
// whitelisted item sort returns the rows in a stable order in both directions.
func TestDatabaseSortIndexes(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())

	indexed := func(table, name string) bool {
		var n int64
		require.NoError(t, db.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?", table, name).Scan(&n).Error)
		return n > 0
	}
	for table, names := range wantSortIndexes {
		for _, name := range names {
			assert.True(t, indexed(table, name), "%s.%s", table, name)
		}
	}

	mig := addSortIndexesMigration()
	require.NoError(t, mig.Up(db.DB), "a second Up is a no-op")
	require.NoError(t, mig.Down(db.DB))
	for table, names := range wantSortIndexes {
		for _, name := range names {
			assert.False(t, indexed(table, name), "%s.%s is dropped", table, name)
		}
	}
	require.NoError(t, mig.Down(db.DB), "a second Down is a no-op")
	require.NoError(t, mig.Up(db.DB))
	for table, names := range wantSortIndexes {
		for _, name := range names {
			assert.True(t, indexed(table, name), "%s.%s is back", table, name)
		}
	}
}

func TestDatabaseMySQLItemsSort(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB)
	ctx := context.Background()

	prefix := fmt.Sprintf("srt-%d-", time.Now().UnixNano())
	t.Cleanup(func() { db.DB.Where("name LIKE ?", prefix+"%").Delete(&models.Item{}) })
	// Prices repeat, so the id tiebreaker decides the order and the pages.
	for i, price := range []float64{5, 2, 5, 9, 2} {
		require.NoError(t, repo.Create(ctx, &models.Item{Name: fmt.Sprintf("%s%d", prefix, i), Price: price}))
	}

	for _, key := range models.ItemSortKeys {
		var asc, desc []models.Item
		total, err := repo.ListPaged(ctx, &asc, 0, 0, models.ItemFilter{Name: prefix, SortBy: key})
		require.NoError(t, err, key)
		assert.Equal(t, int64(5), total)
		_, err = repo.ListPaged(ctx, &desc, 0, 0, models.ItemFilter{Name: prefix, SortBy: key, SortDesc: true})
		require.NoError(t, err, key)
		require.Len(t, asc, 5, key)
		require.Len(t, desc, 5, key)
		for i := range asc {
			assert.Equal(t, asc[i].ID, desc[len(desc)-1-i].ID, "%s: descending reverses ascending", key)
		}

		var paged []models.Item
		for offset := 0; offset < 5; offset += 2 {
			var page []models.Item
			_, err := repo.ListPaged(ctx, &page, 2, offset, models.ItemFilter{Name: prefix, SortBy: key})
			require.NoError(t, err, key)
			paged = append(paged, page...)
		}
		require.Len(t, paged, 5, key)
		for i := range asc {
			assert.Equal(t, asc[i].ID, paged[i].ID, "%s: pages follow the full order", key)
		}
	}

	var got []models.Item
	_, err = repo.ListPaged(ctx, &got, 0, 0, models.ItemFilter{SortBy: "version"})
	assert.Error(t, err, "an unknown key is refused")
}

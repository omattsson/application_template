package database

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"backend/internal/models"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedItems(t *testing.T, repo models.Repository, items ...models.Item) {
	t.Helper()
	for i := range items {
		it := items[i]
		require.NoError(t, repo.Create(context.Background(), &it))
	}
}

func TestItemsListPagedTotalMatchesFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		filter    models.ItemFilter
		limit     int
		offset    int
		wantNames []string
		wantTotal int64
	}{
		{name: "no filter, no limit", wantNames: []string{"apple", "apple pie", "banana", "50%_off"}, wantTotal: 4},
		{name: "name substring counts matches only", filter: models.ItemFilter{Name: "app"}, wantNames: []string{"apple", "apple pie"}, wantTotal: 2},
		{name: "page of a filtered set: total is the full match count", filter: models.ItemFilter{Name: "app"}, limit: 1, offset: 1, wantNames: []string{"apple pie"}, wantTotal: 2},
		{name: "page beyond the end", filter: models.ItemFilter{Name: "app"}, limit: 5, offset: 10, wantNames: []string{}, wantTotal: 2},
		{name: "wildcards are literals", filter: models.ItemFilter{Name: "%_"}, wantNames: []string{"50%_off"}, wantTotal: 1},
		{name: "exact name", filter: models.ItemFilter{NameExact: "apple"}, wantNames: []string{"apple"}, wantTotal: 1},
		{name: "price range", filter: models.ItemFilter{MinPrice: 2, MaxPrice: 5}, wantNames: []string{"apple pie", "banana"}, wantTotal: 2},
		{name: "name and price", filter: models.ItemFilter{Name: "app", MinPrice: 2}, wantNames: []string{"apple pie"}, wantTotal: 1},
		{name: "no match", filter: models.ItemFilter{Name: "zzz"}, wantNames: []string{}, wantTotal: 0},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := setupTestDB(t)
			require.NoError(t, db.AutoMigrate())
			repo := models.NewRepository(db.DB)
			seedItems(t, repo,
				models.Item{Name: "apple", Price: 1},
				models.Item{Name: "apple pie", Price: 3},
				models.Item{Name: "banana", Price: 5},
				models.Item{Name: "50%_off", Price: 9},
			)

			var got []models.Item
			total, err := repo.ListPaged(context.Background(), &got, tt.limit, tt.offset, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTotal, total)
			names := make([]string, 0, len(got))
			for _, it := range got {
				names = append(names, it.Name)
			}
			assert.Equal(t, tt.wantNames, names)
		})
	}
}

func TestItemsListPagedErrors(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB)

	var wrong []string
	_, err := repo.ListPaged(context.Background(), &wrong, 10, 0, models.ItemFilter{})
	assert.Error(t, err, "a destination that is not []Item is rejected")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var items []models.Item
	_, err = repo.ListPaged(ctx, &items, 10, 0, models.ItemFilter{})
	assert.ErrorIs(t, err, context.Canceled)
}

// failUpdateWith makes the first `times` UPDATE statements fail with mysqlErr.
// The returned counter holds the number of UPDATE statements attempted.
func failUpdateWith(t *testing.T, db *gorm.DB, mysqlErr *mysql.MySQLError, times int32) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:fail_update", func(tx *gorm.DB) {
		if calls.Add(1) <= times {
			_ = tx.AddError(mysqlErr)
		}
	}))
	return &calls
}

func TestGenericRepositoryUpdateRetriesDeadlockOnce(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB)
	ctx := context.Background()

	item := &models.Item{Name: "locked", Price: 1}
	require.NoError(t, repo.Create(ctx, item))

	attempts := failUpdateWith(t, db.DB, &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}, 1)
	item.Price = 2
	require.NoError(t, repo.Update(ctx, item))

	assert.Equal(t, int32(2), attempts.Load(), "one deadlock and one retry")
	assert.Equal(t, uint(2), item.Version, "the version moved once, not twice")
	var stored models.Item
	require.NoError(t, repo.FindByID(ctx, item.ID, &stored))
	assert.Equal(t, uint(2), stored.Version)
	assert.Equal(t, 2.0, stored.Price)
}

func TestGenericRepositoryUpdateGivesUpOnLockTimeout(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB)
	ctx := context.Background()

	item := &models.Item{Name: "locked", Price: 1}
	require.NoError(t, repo.Create(ctx, item))

	attempts := failUpdateWith(t, db.DB, &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}, 100)
	item.Price = 2
	err := repo.Update(ctx, item)

	require.Error(t, err)
	assert.Equal(t, int32(4), attempts.Load(), "first attempt and 3 retries")
	assert.Equal(t, uint(1), item.Version, "the in-memory version is rolled back")
	var stored models.Item
	require.NoError(t, repo.FindByID(ctx, item.ID, &stored))
	assert.Equal(t, 1.0, stored.Price)
	assert.Equal(t, uint(1), stored.Version)
}

// A stale version must reach the caller as a version mismatch, once: the
// retry wrapper neither retries nor swallows it.
func TestGenericRepositoryStaleVersionNotRetried(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB)
	ctx := context.Background()

	item := &models.Item{Name: "v", Price: 1}
	require.NoError(t, repo.Create(ctx, item))
	var fresh, stale models.Item
	require.NoError(t, repo.FindByID(ctx, item.ID, &fresh))
	require.NoError(t, repo.FindByID(ctx, item.ID, &stale))
	require.NoError(t, repo.Update(ctx, &fresh))

	attempts := failUpdateWith(t, db.DB, &mysql.MySQLError{Number: 1213}, 0)
	err := repo.Update(ctx, &stale)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "version mismatch")
	assert.Equal(t, int32(1), attempts.Load(), "one statement, no retry")
	assert.Equal(t, uint(1), stale.Version)
}

func TestGenericRepositoryCreateDeleteRetryDeadlock(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB)
	ctx := context.Background()

	var creates atomic.Int32
	require.NoError(t, db.DB.Callback().Create().Before("gorm:create").Register("test:fail_create", func(tx *gorm.DB) {
		if creates.Add(1) == 1 {
			_ = tx.AddError(&mysql.MySQLError{Number: 1213, Message: "Deadlock found"})
		}
	}))
	item := &models.Item{Name: "once", Price: 1}
	require.NoError(t, repo.Create(ctx, item))
	assert.Equal(t, int32(2), creates.Load())

	var count int64
	require.NoError(t, db.DB.Model(&models.Item{}).Where("name = ?", "once").Count(&count).Error)
	assert.Equal(t, int64(1), count, "the row exists once")

	var deletes atomic.Int32
	require.NoError(t, db.DB.Callback().Delete().Before("gorm:delete").Register("test:fail_delete", func(tx *gorm.DB) {
		if deletes.Add(1) == 1 {
			_ = tx.AddError(fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1213}))
		}
	}))
	require.NoError(t, repo.Delete(ctx, &models.Item{Base: models.Base{ID: item.ID}}))
	assert.Equal(t, int32(2), deletes.Load())
}

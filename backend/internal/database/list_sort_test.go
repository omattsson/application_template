package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"backend/internal/database/schema"
	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSortItems stores five items. The IDs are 1 to 5 in this order; two
// prices repeat, so the tiebreaker decides.
//
//	id  name   price  created_at  updated_at
//	1   pear   5      2024-03     2024-01
//	2   apple  2      2024-01     2024-05
//	3   mango  5      2024-02     2024-03
//	4   kiwi   9      2024-05     2024-02
//	5   fig    2      2024-04     2024-04
func seedSortItems(t *testing.T) models.Repository {
	t.Helper()
	db := openMigratedDB(t)
	repo := models.NewRepository(db)
	rows := []struct {
		name            string
		price           float64
		created, update time.Month
	}{
		{"pear", 5, time.March, time.January},
		{"apple", 2, time.January, time.May},
		{"mango", 5, time.February, time.March},
		{"kiwi", 9, time.May, time.February},
		{"fig", 2, time.April, time.April},
	}
	for _, r := range rows {
		it := &models.Item{Name: r.name, Price: r.price}
		require.NoError(t, repo.Create(context.Background(), it))
		require.NoError(t, db.Model(&models.Item{}).Where("id = ?", it.ID).Updates(map[string]any{
			"created_at": time.Date(2024, r.created, 1, 0, 0, 0, 0, time.UTC),
			"updated_at": time.Date(2024, r.update, 1, 0, 0, 0, 0, time.UTC),
		}).Error)
	}
	return repo
}

func TestItemsListPagedSort(t *testing.T) {
	t.Parallel()
	repo := seedSortItems(t)

	tests := []struct {
		name   string
		filter models.ItemFilter
		limit  int
		offset int
		want   []uint
		total  int64
	}{
		{name: "default is id ascending", want: []uint{1, 2, 3, 4, 5}, total: 5},
		{name: "descending alone sorts id", filter: models.ItemFilter{SortDesc: true}, want: []uint{5, 4, 3, 2, 1}, total: 5},
		{name: "id asc", filter: models.ItemFilter{SortBy: "id"}, want: []uint{1, 2, 3, 4, 5}, total: 5},
		{name: "id desc", filter: models.ItemFilter{SortBy: "id", SortDesc: true}, want: []uint{5, 4, 3, 2, 1}, total: 5},
		{name: "name asc", filter: models.ItemFilter{SortBy: "name"}, want: []uint{2, 5, 4, 3, 1}, total: 5},
		{name: "name desc", filter: models.ItemFilter{SortBy: "name", SortDesc: true}, want: []uint{1, 3, 4, 5, 2}, total: 5},
		{name: "price asc, ties by id asc", filter: models.ItemFilter{SortBy: "price"}, want: []uint{2, 5, 1, 3, 4}, total: 5},
		{name: "price desc, ties by id desc", filter: models.ItemFilter{SortBy: "price", SortDesc: true}, want: []uint{4, 3, 1, 5, 2}, total: 5},
		{name: "created_at asc", filter: models.ItemFilter{SortBy: "created_at"}, want: []uint{2, 3, 1, 5, 4}, total: 5},
		{name: "created_at desc", filter: models.ItemFilter{SortBy: "created_at", SortDesc: true}, want: []uint{4, 5, 1, 3, 2}, total: 5},
		{name: "updated_at asc", filter: models.ItemFilter{SortBy: "updated_at"}, want: []uint{1, 4, 3, 5, 2}, total: 5},
		{name: "updated_at desc", filter: models.ItemFilter{SortBy: "updated_at", SortDesc: true}, want: []uint{2, 5, 3, 4, 1}, total: 5},
		{name: "page 1 of a price sort", filter: models.ItemFilter{SortBy: "price"}, limit: 2, want: []uint{2, 5}, total: 5},
		{name: "page 2 of a price sort", filter: models.ItemFilter{SortBy: "price"}, limit: 2, offset: 2, want: []uint{1, 3}, total: 5},
		{name: "page 3 of a price sort", filter: models.ItemFilter{SortBy: "price"}, limit: 2, offset: 4, want: []uint{4}, total: 5},
		{name: "page 1 of a descending price sort", filter: models.ItemFilter{SortBy: "price", SortDesc: true}, limit: 2, want: []uint{4, 3}, total: 5},
		{name: "page 2 of a descending price sort", filter: models.ItemFilter{SortBy: "price", SortDesc: true}, limit: 2, offset: 2, want: []uint{1, 5}, total: 5},
		{name: "filter and sort, total counts the filter", filter: models.ItemFilter{MinPrice: 3, SortBy: "price", SortDesc: true}, want: []uint{4, 3, 1}, total: 3},
		{name: "filtered page of a sort", filter: models.ItemFilter{Name: "a", SortBy: "name", SortDesc: true}, limit: 2, want: []uint{1, 3}, total: 3},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []models.Item
			total, err := repo.ListPaged(context.Background(), &got, tt.limit, tt.offset, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.total, total)
			ids := make([]uint, 0, len(got))
			for _, it := range got {
				ids = append(ids, it.ID)
			}
			assert.Equal(t, tt.want, ids)
		})
	}
}

func TestItemsListPagedSortTotalIgnoresSort(t *testing.T) {
	t.Parallel()
	repo := seedSortItems(t)
	ctx := context.Background()
	for _, key := range models.ItemSortKeys {
		for _, desc := range []bool{false, true} {
			var got []models.Item
			total, err := repo.ListPaged(ctx, &got, 2, 2, models.ItemFilter{MaxPrice: 5, SortBy: key, SortDesc: desc})
			require.NoError(t, err, key)
			assert.Equal(t, int64(4), total, "%s desc=%v", key, desc)
			assert.Len(t, got, 2)
		}
	}
}

func TestItemsListPagedRejectsUnknownSortKey(t *testing.T) {
	t.Parallel()
	repo := seedSortItems(t)
	for _, key := range []string{"version", "deleted_at", "Name", "name DESC", "id; DROP TABLE items", "1", "(SELECT 1)"} {
		var got []models.Item
		_, err := repo.ListPaged(context.Background(), &got, 10, 0, models.ItemFilter{SortBy: key})
		assert.ErrorIs(t, err, dberrors.ErrValidation, key)
		assert.Empty(t, got, key)
	}

	// The items table is intact.
	var got []models.Item
	total, err := repo.ListPaged(context.Background(), &got, 0, 0, models.ItemFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(5), total)
}

func TestItemFilterSortColumn(t *testing.T) {
	t.Parallel()
	for _, key := range models.ItemSortKeys {
		col, err := models.ItemFilter{SortBy: key}.SortColumn()
		require.NoError(t, err, key)
		assert.Equal(t, key, col)
	}
	col, err := models.ItemFilter{}.SortColumn()
	require.NoError(t, err)
	assert.Equal(t, "id", col, "no key sorts by id")
	_, err = models.ItemFilter{SortBy: "nope"}.SortColumn()
	assert.ErrorIs(t, err, dberrors.ErrValidation)
}

// seedSortUsers stores six users. The IDs ascend in this order.
//
//	id  username  role    email  created_at
//	u1  admin-a   admin   m@x    2024-03
//	u2  admin-b   admin   z@x    2024-01
//	u3  devops    devops  b@x    2024-06
//	u4  user-u    user    a@x    2024-02
//	u5  oidc-o    user    k@x    2024-05
//	u6  user-v    user    c@x    2024-04
func seedSortUsers(t *testing.T) *GORMUserRepository {
	t.Helper()
	db := openMigratedDB(t)
	repo := NewGORMUserRepository(db)
	rows := []struct {
		username, role, email string
		created               time.Month
	}{
		{"admin-a", models.RoleAdmin, "m@x.io", time.March},
		{"admin-b", models.RoleAdmin, "z@x.io", time.January},
		{"devops", models.RoleDevOps, "b@x.io", time.June},
		{"user-u", models.RoleUser, "a@x.io", time.February},
		{"oidc-o", models.RoleUser, "k@x.io", time.May},
		{"user-v", models.RoleUser, "c@x.io", time.April},
	}
	for i, r := range rows {
		id := fmt.Sprintf("u%d", i+1)
		require.NoError(t, repo.Create(context.Background(), &models.User{ID: id, Username: r.username, Role: r.role, Email: r.email}))
		require.NoError(t, db.Model(&models.User{}).Where("id = ?", id).
			Update("created_at", time.Date(2024, r.created, 1, 0, 0, 0, 0, time.UTC)).Error)
	}
	return repo
}

func TestUserRepositoryListPagedSort(t *testing.T) {
	t.Parallel()
	repo := seedSortUsers(t)

	tests := []struct {
		name   string
		filter models.UserFilter
		limit  int
		offset int
		want   []string
		total  int64
	}{
		{name: "default is username ascending", want: []string{"u1", "u2", "u3", "u5", "u4", "u6"}, total: 6},
		{name: "descending alone sorts username", filter: models.UserFilter{SortDesc: true}, want: []string{"u6", "u4", "u5", "u3", "u2", "u1"}, total: 6},
		{name: "username asc", filter: models.UserFilter{SortBy: "username"}, want: []string{"u1", "u2", "u3", "u5", "u4", "u6"}, total: 6},
		{name: "username desc", filter: models.UserFilter{SortBy: "username", SortDesc: true}, want: []string{"u6", "u4", "u5", "u3", "u2", "u1"}, total: 6},
		{name: "email asc", filter: models.UserFilter{SortBy: "email"}, want: []string{"u4", "u3", "u6", "u5", "u1", "u2"}, total: 6},
		{name: "email desc", filter: models.UserFilter{SortBy: "email", SortDesc: true}, want: []string{"u2", "u1", "u5", "u6", "u3", "u4"}, total: 6},
		{name: "role asc, ties by id asc", filter: models.UserFilter{SortBy: "role"}, want: []string{"u1", "u2", "u3", "u4", "u5", "u6"}, total: 6},
		{name: "role desc, ties by id desc", filter: models.UserFilter{SortBy: "role", SortDesc: true}, want: []string{"u6", "u5", "u4", "u3", "u2", "u1"}, total: 6},
		{name: "created_at asc", filter: models.UserFilter{SortBy: "created_at"}, want: []string{"u2", "u4", "u1", "u6", "u5", "u3"}, total: 6},
		{name: "created_at desc", filter: models.UserFilter{SortBy: "created_at", SortDesc: true}, want: []string{"u3", "u5", "u6", "u1", "u4", "u2"}, total: 6},
		{name: "page of a role sort", filter: models.UserFilter{SortBy: "role"}, limit: 2, offset: 2, want: []string{"u3", "u4"}, total: 6},
		{name: "page of a descending role sort", filter: models.UserFilter{SortBy: "role", SortDesc: true}, limit: 2, offset: 2, want: []string{"u4", "u3"}, total: 6},
		{name: "filter and sort", filter: models.UserFilter{Role: models.RoleUser, SortBy: "email"}, want: []string{"u4", "u6", "u5"}, total: 3},
		{name: "search and sort", filter: models.UserFilter{Search: "admin", SortBy: "created_at", SortDesc: true}, want: []string{"u1", "u2"}, total: 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			users, total, err := repo.ListPaged(context.Background(), tt.filter, tt.limit, tt.offset)
			require.NoError(t, err)
			assert.Equal(t, tt.total, total)
			ids := make([]string, 0, len(users))
			for _, u := range users {
				ids = append(ids, u.ID)
			}
			assert.Equal(t, tt.want, ids)
		})
	}
}

func TestUserRepositoryListPagedRejectsUnknownSortKey(t *testing.T) {
	t.Parallel()
	repo := seedSortUsers(t)
	for _, key := range []string{"id", "password_hash", "display_name", "disabled", "Username", "username DESC", "role; DROP TABLE users", "(SELECT 1)"} {
		users, total, err := repo.ListPaged(context.Background(), models.UserFilter{SortBy: key}, 10, 0)
		assert.ErrorIs(t, err, dberrors.ErrValidation, key)
		assert.Empty(t, users, key)
		assert.Zero(t, total, key)
	}

	users, total, err := repo.ListPaged(context.Background(), models.UserFilter{}, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(6), total)
	assert.Len(t, users, 6)
}

// The handler and the repository keep the same whitelist.
func TestUserSortKeysHaveColumns(t *testing.T) {
	t.Parallel()
	assert.Len(t, userSortColumns, len(models.UserSortKeys))
	for _, key := range models.UserSortKeys {
		col, ok := userSortColumns[key]
		assert.True(t, ok, key)
		assert.NotEmpty(t, col, key)
	}
}

// wantSortIndexes are the single-column indexes that serve a whitelisted sort:
// MySQL appends the primary key to a secondary index, so (col) serves both
// ORDER BY col, id and ORDER BY col DESC, id DESC.
var wantSortIndexes = map[string][]string{
	"items": {"idx_items_name", "idx_items_price", "idx_items_created_at", "idx_items_updated_at"},
	"users": {"idx_users_email", "idx_users_created_at"},
}

func TestAddSortIndexesMigration(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	mig := addSortIndexesMigration()
	assert.Equal(t, "20261115000001", mig.Version)
	assert.Equal(t, "add_sort_indexes", mig.Name)
	require.NotNil(t, mig.Down)

	assertIndexes := func(present bool) {
		t.Helper()
		for table, names := range wantSortIndexes {
			for _, name := range names {
				assert.Equal(t, present, db.Migrator().HasIndex(table, name), "%s.%s", table, name)
			}
		}
	}
	assertIndexes(true)

	require.NoError(t, mig.Up(db), "a second Up is a no-op")
	assertIndexes(true)

	require.NoError(t, mig.Down(db))
	assertIndexes(false)
	require.NoError(t, mig.Down(db), "a second Down is a no-op")
	for _, name := range []string{"idx_items_name_price", "idx_items_deleted_at"} {
		assert.True(t, db.Migrator().HasIndex("items", name), "Down keeps %s", name)
	}
	for _, name := range []string{"idx_users_username", "idx_users_role"} {
		assert.True(t, db.Migrator().HasIndex("users", name), "Down keeps %s", name)
	}

	require.NoError(t, mig.Up(db))
	assertIndexes(true)
}

// The sort indexes survive a rollback of the migration through the migrator,
// and the older Down steps still work after it.
func TestAddSortIndexesMigrationRollsBack(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	m := schema.NewMigrator(db)
	registerMigrations(m)

	require.NoError(t, m.MigrateDown(), "roll back the jobs")
	require.NoError(t, m.MigrateDown(), "roll back the password reset tokens")
	require.NoError(t, m.MigrateDown(), "roll back the outbox events")
	require.NoError(t, m.MigrateDown(), "roll back the idempotency keys")
	require.NoError(t, m.MigrateDown(), "roll back the sort indexes")
	assert.False(t, db.Migrator().HasIndex("items", "idx_items_price"))
	assert.False(t, db.Migrator().HasIndex("users", "idx_users_email"))
	assert.Len(t, appliedVersions(t, db), 6)

	require.NoError(t, m.MigrateUp())
	assert.True(t, db.Migrator().HasIndex("items", "idx_items_price"))
	assert.True(t, db.Migrator().HasIndex("users", "idx_users_email"))
	assert.Len(t, appliedVersions(t, db), 11)
}

// Every whitelisted sort column has an index of its own on a migrated
// database (the default id and the unique username are covered by the
// primary key and idx_users_username).
func TestSortKeysAreIndexed(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	covered := map[string]map[string]string{
		"items": {"id": "PRIMARY KEY", "name": "idx_items_name", "price": "idx_items_price", "created_at": "idx_items_created_at", "updated_at": "idx_items_updated_at"},
		"users": {"username": "idx_users_username", "email": "idx_users_email", "role": "idx_users_role", "created_at": "idx_users_created_at"},
	}
	keys := map[string][]string{"items": models.ItemSortKeys, "users": models.UserSortKeys}
	for table, tableKeys := range keys {
		assert.Len(t, covered[table], len(tableKeys), table)
		for _, key := range tableKeys {
			idx, ok := covered[table][key]
			require.True(t, ok, "%s.%s has no index", table, key)
			if idx != "PRIMARY KEY" {
				assert.True(t, db.Migrator().HasIndex(table, idx), "%s.%s", table, idx)
			}
		}
	}
}

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sortFixture seeds five items. The IDs are 1 to 5 in this order. The prices
// and the timestamps differ from the ID order, and two prices repeat (2 twice,
// 5 twice), so a sort needs its id tiebreaker.
//
//	id  name   price  created_at  updated_at
//	1   pear   5      2024-03     2024-01
//	2   apple  2      2024-01     2024-05
//	3   mango  5      2024-02     2024-03
//	4   kiwi   9      2024-05     2024-02
//	5   fig    2      2024-04     2024-04
func sortFixture(t *testing.T, repo *MockRepository) {
	t.Helper()
	month := func(m time.Month) time.Time { return time.Date(2024, m, 1, 0, 0, 0, 0, time.UTC) }
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
		item := &models.Item{Name: r.name, Price: r.price}
		item.CreatedAt = month(r.created)
		item.UpdatedAt = month(r.update)
		require.NoError(t, repo.Create(context.Background(), item))
	}
}

func itemIDs(items []models.Item) []uint {
	ids := make([]uint, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

func listItems(t *testing.T, query string) (int, []models.Item, string) {
	t.Helper()
	router, repo := setupTestRouter()
	sortFixture(t, repo)
	return listItemsOn(t, router, query)
}

func listItemsOn(t *testing.T, router http.Handler, query string) (int, []models.Item, string) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/items"+query, nil))
	var items []models.Item
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
	}
	return w.Code, items, w.Body.String()
}

func TestGetItemsSort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query string
		want  []uint
	}{
		{"default order is id ascending", "", []uint{1, 2, 3, 4, 5}},
		{"empty sort and order are the default", "?sort=&order=", []uint{1, 2, 3, 4, 5}},
		{"order alone sorts id", "?order=desc", []uint{5, 4, 3, 2, 1}},
		{"order asc alone", "?order=asc", []uint{1, 2, 3, 4, 5}},
		{"id asc", "?sort=id&order=asc", []uint{1, 2, 3, 4, 5}},
		{"id desc", "?sort=id&order=desc", []uint{5, 4, 3, 2, 1}},
		{"id without order", "?sort=id", []uint{1, 2, 3, 4, 5}},
		{"name asc", "?sort=name&order=asc", []uint{2, 5, 4, 3, 1}},
		{"name desc", "?sort=name&order=desc", []uint{1, 3, 4, 5, 2}},
		{"name without order is asc", "?sort=name", []uint{2, 5, 4, 3, 1}},
		{"price asc, ties by id asc", "?sort=price&order=asc", []uint{2, 5, 1, 3, 4}},
		{"price desc, ties by id desc", "?sort=price&order=desc", []uint{4, 3, 1, 5, 2}},
		{"created_at asc", "?sort=created_at&order=asc", []uint{2, 3, 1, 5, 4}},
		{"created_at desc", "?sort=created_at&order=desc", []uint{4, 5, 1, 3, 2}},
		{"updated_at asc", "?sort=updated_at&order=asc", []uint{1, 4, 3, 5, 2}},
		{"updated_at desc", "?sort=updated_at&order=desc", []uint{2, 5, 3, 4, 1}},
		{"filter and sort", "?min_price=3&sort=price&order=desc", []uint{4, 3, 1}},
		{"name filter and sort", "?name=a&sort=name&order=asc", []uint{2, 3, 1}},
		{"sort and limit", "?sort=price&order=desc&limit=2", []uint{4, 3}},
		{"sort, limit and offset", "?sort=price&order=desc&limit=2&offset=2", []uint{1, 5}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, items, body := listItems(t, tt.query)
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, tt.want, itemIDs(items))
		})
	}
}

func TestGetItemsSortRejectsBadValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		query   string
		wantMsg string
	}{
		{"unknown sort", "?sort=version", "Invalid sort parameter"},
		{"column that is not listed", "?sort=deleted_at", "Invalid sort parameter"},
		{"sort with the wrong case", "?sort=Name", "Invalid sort parameter"},
		{"sort with SQL", "?sort=id%3BDROP+TABLE+items", "Invalid sort parameter"},
		{"sort with a direction inside", "?sort=name+desc", "Invalid sort parameter"},
		{"unknown order", "?order=up", "Invalid order parameter"},
		{"order with the wrong case", "?sort=name&order=DESC", "Invalid order parameter"},
		{"order with SQL", "?order=asc,id", "Invalid order parameter"},
		{"good sort and bad order", "?sort=name&order=sideways", "Invalid order parameter"},
		{"bad sort and good order", "?sort=zzz&order=asc", "Invalid sort parameter"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, repo := setupTestRouter()
			sortFixture(t, repo)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/items"+tt.query, nil))
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var body map[string]string
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, tt.wantMsg, body["error"])
		})
	}
}

// Pages of a sort on a column with repeated values never repeat or skip a row.
func TestGetItemsSortPagesAreStable(t *testing.T) {
	t.Parallel()
	for _, sortKey := range models.ItemSortKeys {
		for _, order := range []string{"asc", "desc"} {
			sortKey, order := sortKey, order
			t.Run(sortKey+" "+order, func(t *testing.T) {
				t.Parallel()
				router, repo := setupTestRouter()
				sortFixture(t, repo)
				base := "?sort=" + sortKey + "&order=" + order

				code, all, body := listItemsOn(t, router, base)
				require.Equal(t, http.StatusOK, code, body)
				require.Len(t, all, 5)

				var paged []models.Item
				for _, offset := range []string{"0", "2", "4"} {
					code, page, body := listItemsOn(t, router, base+"&limit=2&offset="+offset)
					require.Equal(t, http.StatusOK, code, body)
					paged = append(paged, page...)
				}
				assert.Equal(t, itemIDs(all), itemIDs(paged))
			})
		}
	}
}

// Every whitelisted key sorts in both directions, and the descending order is
// the exact reverse of the ascending one (the tiebreaker follows the order).
func TestGetItemsSortDescendingReversesAscending(t *testing.T) {
	t.Parallel()
	for _, sortKey := range models.ItemSortKeys {
		sortKey := sortKey
		t.Run(sortKey, func(t *testing.T) {
			t.Parallel()
			router, repo := setupTestRouter()
			sortFixture(t, repo)
			_, asc, _ := listItemsOn(t, router, "?sort="+sortKey+"&order=asc")
			_, desc, _ := listItemsOn(t, router, "?sort="+sortKey+"&order=desc")
			require.Len(t, asc, 5)
			ids := itemIDs(asc)
			for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
				ids[i], ids[j] = ids[j], ids[i]
			}
			assert.Equal(t, ids, itemIDs(desc))
		})
	}
}

// The total counts the matching rows, whatever the sort.
func TestMockListPagedSort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("total does not depend on the sort", func(t *testing.T) {
		t.Parallel()
		repo := NewMockRepository()
		sortFixture(t, repo)
		var plain, sorted []models.Item
		totalPlain, err := repo.ListPaged(ctx, &plain, 2, 0, models.ItemFilter{MinPrice: 3})
		require.NoError(t, err)
		totalSorted, err := repo.ListPaged(ctx, &sorted, 2, 0, models.ItemFilter{MinPrice: 3, SortBy: "price", SortDesc: true})
		require.NoError(t, err)
		assert.Equal(t, int64(3), totalPlain)
		assert.Equal(t, totalPlain, totalSorted)
		assert.Equal(t, []uint{4, 3}, itemIDs(sorted))
	})

	t.Run("unknown key is a validation error", func(t *testing.T) {
		t.Parallel()
		repo := NewMockRepository()
		sortFixture(t, repo)
		var items []models.Item
		_, err := repo.ListPaged(ctx, &items, 0, 0, models.ItemFilter{SortBy: "name; DROP TABLE items"})
		assert.ErrorIs(t, err, dberrors.ErrValidation)
	})
}

// A repository error from the sorted list stays a 500 without detail.
func TestGetItemsSortRepositoryError(t *testing.T) {
	t.Parallel()
	router, repo := setupTestRouter()
	repo.SetError(assert.AnError)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/items?sort=name", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), assert.AnError.Error())
}

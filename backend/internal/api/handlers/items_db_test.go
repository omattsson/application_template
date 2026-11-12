package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupDBItemsRouter wires the item handlers to the real GenericRepository on
// an in-memory SQLite database, so the repository behavior (optimistic
// locking, lock retry, paged list) is part of the test.
func setupDBItemsRouter(t *testing.T) *gin.Engine {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.Item{}))

	h := NewHandler(models.NewRepository(db))
	router := gin.New()
	items := router.Group("/api/v1/items")
	items.GET("", h.GetItems)
	items.POST("", h.CreateItem)
	items.PUT("/:id", h.UpdateItem)
	return router
}

func doJSON(router *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestItemUpdateStaleVersionReturns409(t *testing.T) {
	router := setupDBItemsRouter(t)

	w := doJSON(router, http.MethodPost, "/api/v1/items", models.Item{Name: "orig", Price: 1})
	require.Equal(t, http.StatusCreated, w.Code)
	var created models.Item
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.Equal(t, uint(1), created.Version)

	// First update with the current version succeeds and moves it to 2.
	w = doJSON(router, http.MethodPut, "/api/v1/items/1", models.Item{Name: "first", Price: 2, Version: 1})
	require.Equal(t, http.StatusOK, w.Code)
	var updated models.Item
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, uint(2), updated.Version)

	// The same version again is stale: 409, not retried, not swallowed.
	w = doJSON(router, http.MethodPut, "/api/v1/items/1", models.Item{Name: "second", Price: 3, Version: 1})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "modified by another request")

	// The stale write left no trace.
	w = doJSON(router, http.MethodGet, "/api/v1/items", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var list []models.Item
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, "first", list[0].Name)
	assert.Equal(t, uint(2), list[0].Version)
}

func TestItemsListEndpointPagedAgainstDatabase(t *testing.T) {
	router := setupDBItemsRouter(t)
	for _, it := range []models.Item{{Name: "apple", Price: 1}, {Name: "apple pie", Price: 3}, {Name: "banana", Price: 5}} {
		require.Equal(t, http.StatusCreated, doJSON(router, http.MethodPost, "/api/v1/items", it).Code)
	}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "all", query: "", want: []string{"apple", "apple pie", "banana"}},
		{name: "name filter", query: "?name=app", want: []string{"apple", "apple pie"}},
		{name: "name filter with limit and offset", query: "?name=app&limit=1&offset=1", want: []string{"apple pie"}},
		{name: "exact name", query: "?name_exact=banana", want: []string{"banana"}},
		{name: "price range", query: "?min_price=2&max_price=4", want: []string{"apple pie"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doJSON(router, http.MethodGet, "/api/v1/items"+tt.query, nil)
			require.Equal(t, http.StatusOK, w.Code)
			var got []models.Item
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			names := make([]string, 0, len(got))
			for _, it := range got {
				names = append(names, it.Name)
			}
			assert.Equal(t, tt.want, names)
		})
	}
}

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"backend/internal/database"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctxAwareRepo wraps MockRepository and, like a real database driver, fails a
// call whose context has ended. onFind runs after a successful FindByID, so a
// test can cancel the request between the read and the write of an update.
type ctxAwareRepo struct {
	*MockRepository
	onFind func()
}

func (r *ctxAwareRepo) Create(ctx context.Context, entity interface{}) error {
	if err := ctx.Err(); err != nil {
		return database.NewDatabaseError("create", err)
	}
	return r.MockRepository.Create(ctx, entity)
}

func (r *ctxAwareRepo) FindByID(ctx context.Context, id uint, dest interface{}) error {
	if err := ctx.Err(); err != nil {
		return database.NewDatabaseError("find", err)
	}
	if err := r.MockRepository.FindByID(ctx, id, dest); err != nil {
		return err
	}
	if r.onFind != nil {
		r.onFind()
	}
	return nil
}

func (r *ctxAwareRepo) Update(ctx context.Context, entity interface{}) error {
	if err := ctx.Err(); err != nil {
		return database.NewDatabaseError("update", err)
	}
	return r.MockRepository.Update(ctx, entity)
}

func (r *ctxAwareRepo) Delete(ctx context.Context, entity interface{}) error {
	if err := ctx.Err(); err != nil {
		return database.NewDatabaseError("delete", err)
	}
	return r.MockRepository.Delete(ctx, entity)
}

func setupCtxAwareRouter(repo *ctxAwareRepo) *gin.Engine {
	router := gin.New()
	handler := NewHandler(repo)
	items := router.Group("/api/v1/items")
	items.POST("", handler.CreateItem)
	items.GET("/:id", handler.GetItem)
	items.PUT("/:id", handler.UpdateItem)
	items.DELETE("/:id", handler.DeleteItem)
	return router
}

func seedItem(t *testing.T, repo *MockRepository) models.Item {
	t.Helper()
	item := models.Item{Name: "seed", Price: 1}
	require.NoError(t, repo.Create(context.Background(), &item))
	return item
}

// TestItemsUpdateCancelledReturns499 checks that an update whose request is
// cancelled before the read answers 499 with a safe body, not 500.
func TestItemsUpdateCancelledReturns499(t *testing.T) {
	t.Parallel()

	repo := &ctxAwareRepo{MockRepository: NewMockRepository()}
	item := seedItem(t, repo.MockRepository)
	router := setupCtxAwareRouter(repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := bytes.NewBufferString(`{"name":"renamed","price":2}`)
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/items/%d", item.ID), body).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, 499, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Client closed request", resp["error"])

	var stored models.Item
	require.NoError(t, repo.MockRepository.FindByID(context.Background(), item.ID, &stored))
	assert.Equal(t, "seed", stored.Name, "a cancelled read must change nothing")
}

// TestItemsReadCancelledReturns499 checks that reads keep the request context.
func TestItemsReadCancelledReturns499(t *testing.T) {
	t.Parallel()

	repo := &ctxAwareRepo{MockRepository: NewMockRepository()}
	item := seedItem(t, repo.MockRepository)
	router := setupCtxAwareRouter(repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/items/%d", item.ID), nil).WithContext(ctx)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, 499, w.Code)
}

// TestItemsWritesSurviveRequestCancel checks that a client that leaves does
// not abort create, update or delete: the write runs on a detached context.
func TestItemsWritesSurviveRequestCancel(t *testing.T) {
	t.Parallel()

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		repo := &ctxAwareRepo{MockRepository: NewMockRepository()}
		router := setupCtxAwareRouter(repo)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/items", bytes.NewBufferString(`{"name":"new","price":3}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The client leaves right after the read, before the write.
		repo := &ctxAwareRepo{MockRepository: NewMockRepository(), onFind: cancel}
		item := seedItem(t, repo.MockRepository)
		router := setupCtxAwareRouter(repo)

		req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/items/%d", item.ID),
			bytes.NewBufferString(`{"name":"renamed","price":2}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		var stored models.Item
		require.NoError(t, repo.MockRepository.FindByID(context.Background(), item.ID, &stored))
		assert.Equal(t, "renamed", stored.Name)
	})

	t.Run("delete", func(t *testing.T) {
		t.Parallel()
		repo := &ctxAwareRepo{MockRepository: NewMockRepository()}
		item := seedItem(t, repo.MockRepository)
		router := setupCtxAwareRouter(repo)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/items/%d", item.ID), nil).WithContext(ctx)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNoContent, w.Code)
		var stored models.Item
		assert.Error(t, repo.MockRepository.FindByID(context.Background(), item.ID, &stored))
	})
}

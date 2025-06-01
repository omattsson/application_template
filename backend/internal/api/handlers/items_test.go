package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func setupTestRouter() (*gin.Engine, *MockRepository) {
	gin.SetMode(gin.TestMode)
	router := gin.Default()
	mockRepo := NewMockRepository()
	handler := NewHandler(mockRepo)

	// Setup routes
	items := router.Group("/api/v1/items")
	{
		items.GET("", handler.GetItems)
		items.GET("/:id", handler.GetItem)
		items.POST("", handler.CreateItem)
		items.PUT("/:id", handler.UpdateItem)
		items.DELETE("/:id", handler.DeleteItem)
	}

	return router, mockRepo
}

func TestCreateItem(t *testing.T) {
	router, _ := setupTestRouter()

	tests := []struct {
		name       string
		input      models.Item
		wantStatus int
	}{
		{
			name: "valid item",
			input: models.Item{
				Name:  "Test Item",
				Price: 99.99,
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "invalid item - empty name",
			input: models.Item{
				Name:  "",
				Price: 99.99,
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, _ := json.Marshal(tt.input)
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/api/v1/items", bytes.NewBuffer(payload))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)

			if tt.wantStatus == http.StatusCreated {
				var response models.Item
				err := json.Unmarshal(w.Body.Bytes(), &response)
				assert.NoError(t, err)
				assert.NotZero(t, response.ID)
				assert.Equal(t, tt.input.Name, response.Name)
				assert.Equal(t, tt.input.Price, response.Price)
			}
		})
	}
}

func TestGetItem(t *testing.T) {
	router, mockRepo := setupTestRouter()

	// Create a test item
	testItem := &models.Item{Name: "Test Item", Price: 99.99}
	mockRepo.Create(testItem)

	tests := []struct {
		name       string
		itemID     string
		wantStatus int
		wantItem   *models.Item
	}{
		{
			name:       "existing item",
			itemID:     "1",
			wantStatus: http.StatusOK,
			wantItem:   testItem,
		},
		{
			name:       "non-existent item",
			itemID:     "999",
			wantStatus: http.StatusNotFound,
			wantItem:   nil,
		},
		{
			name:       "invalid item ID",
			itemID:     "invalid",
			wantStatus: http.StatusBadRequest,
			wantItem:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/items/%s", tt.itemID), nil)
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)

			if tt.wantItem != nil {
				var response models.Item
				err := json.Unmarshal(w.Body.Bytes(), &response)
				assert.NoError(t, err)
				assert.Equal(t, tt.wantItem.Name, response.Name)
				assert.Equal(t, tt.wantItem.Price, response.Price)
			}
		})
	}
}

func TestUpdateItem(t *testing.T) {
	router, mockRepo := setupTestRouter()

	// Create a test item
	testItem := &models.Item{Name: "Test Item", Price: 99.99}
	mockRepo.Create(testItem)

	tests := []struct {
		name       string
		itemID     string
		input      models.Item
		wantStatus int
	}{
		{
			name:   "valid update",
			itemID: "1",
			input: models.Item{
				Name:  "Updated Item",
				Price: 199.99,
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "non-existent item",
			itemID: "999",
			input: models.Item{
				Name:  "Updated Item",
				Price: 199.99,
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "invalid item ID",
			itemID: "invalid",
			input: models.Item{
				Name:  "Updated Item",
				Price: 199.99,
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, _ := json.Marshal(tt.input)
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("PUT", fmt.Sprintf("/api/v1/items/%s", tt.itemID), bytes.NewBuffer(payload))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)

			if tt.wantStatus == http.StatusOK {
				var response models.Item
				err := json.Unmarshal(w.Body.Bytes(), &response)
				assert.NoError(t, err)
				assert.Equal(t, tt.input.Name, response.Name)
				assert.Equal(t, tt.input.Price, response.Price)
			}
		})
	}
}

func TestDeleteItem(t *testing.T) {
	router, mockRepo := setupTestRouter()

	// Create a test item
	testItem := &models.Item{Name: "Test Item", Price: 99.99}
	mockRepo.Create(testItem)

	tests := []struct {
		name       string
		itemID     string
		wantStatus int
	}{
		{
			name:       "existing item",
			itemID:     "1",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "non-existent item",
			itemID:     "999",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "invalid item ID",
			itemID:     "invalid",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("DELETE", fmt.Sprintf("/api/v1/items/%s", tt.itemID), nil)
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}

func TestListItems(t *testing.T) {
	router, mockRepo := setupTestRouter()

	// Create test items
	items := []models.Item{
		{Name: "Item 1", Price: 99.99},
		{Name: "Item 2", Price: 199.99},
		{Name: "Item 3", Price: 299.99},
	}

	for _, item := range items {
		mockRepo.Create(&item)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/items", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []models.Item
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)
	assert.Equal(t, len(items), len(response))

	// Verify items are in the response
	for i, item := range response {
		assert.Equal(t, items[i].Name, item.Name)
		assert.Equal(t, items[i].Price, item.Price)
	}
}

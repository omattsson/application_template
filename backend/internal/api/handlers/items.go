package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"backend/internal/database"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	repository models.Repository
}

func NewHandler(repository models.Repository) *Handler {
	return &Handler{repository: repository}
}

func handleDBError(err error) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}

	switch e := err.(type) {
	case *database.DatabaseError:
		switch e.Err {
		case database.ErrValidation:
			return http.StatusBadRequest, e.Error()
		case database.ErrNotFound:
			return http.StatusNotFound, "Item not found"
		case database.ErrDuplicateKey:
			return http.StatusConflict, "Item already exists"
		default:
			return http.StatusInternalServerError, "Internal server error"
		}
	default:
		if strings.Contains(err.Error(), "not found") {
			return http.StatusNotFound, "Item not found"
		}
		return http.StatusInternalServerError, err.Error()
	}
}

// CreateItem godoc
// @Summary Create a new item
// @Description Create a new item with the provided information
// @Tags items
// @Accept json
// @Produce json
// @Param item body models.Item true "Item object"
// @Success 201 {object} models.Item
// @Failure 400 {object} map[string]string
// @Router /api/v1/items [post]
func (h *Handler) CreateItem(c *gin.Context) {
	var item models.Item
	if err := c.ShouldBindJSON(&item); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	if item.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Name is required"})
		return
	}

	if err := h.repository.Create(&item); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	c.JSON(http.StatusCreated, item)
}

// GetItems godoc
// @Summary Get all items
// @Description Get a list of all items
// @Tags items
// @Produce json
// @Success 200 {array} models.Item
// @Router /api/v1/items [get]
func (h *Handler) GetItems(c *gin.Context) {
	// Parse query parameters
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	minPrice, _ := strconv.ParseFloat(c.Query("min_price"), 64)
	maxPrice, _ := strconv.ParseFloat(c.Query("max_price"), 64)

	// Validate parameters
	if c.Query("limit") != "" && limit <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid limit parameter"})
		return
	}
	if c.Query("offset") != "" && offset < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid offset parameter"})
		return
	}

	var items []models.Item
	conditions := make([]interface{}, 0)

	// Handle name filtering
	if c.Query("name_exact") != "" {
		conditions = append(conditions, models.Filter{
			Field: "name",
			Op:    "exact",
			Value: c.Query("name_exact"),
		})
	} else if name := c.Query("name"); name != "" {
		conditions = append(conditions, models.Filter{
			Field: "name",
			Value: name,
		})
	}
	if minPrice > 0 {
		conditions = append(conditions, models.Filter{Field: "price", Op: ">=", Value: minPrice})
	}
	if maxPrice > 0 {
		conditions = append(conditions, models.Filter{Field: "price", Op: "<=", Value: maxPrice})
	}
	if limit > 0 {
		conditions = append(conditions, models.Pagination{Limit: limit, Offset: offset})
	}

	if err := h.repository.List(&items, conditions...); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	c.JSON(http.StatusOK, items)
}

// GetItem godoc
// @Summary Get an item by ID
// @Description Get an item by its ID
// @Tags items
// @Produce json
// @Param id path int true "Item ID"
// @Success 200 {object} models.Item
// @Failure 404 {object} map[string]string
// @Router /api/v1/items/{id} [get]
func (h *Handler) GetItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	var item models.Item
	if err := h.repository.FindByID(uint(id), &item); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	c.JSON(http.StatusOK, item)
}

// UpdateItem godoc
// @Summary Update an item
// @Description Update an item's information
// @Tags items
// @Accept json
// @Produce json
// @Param id path int true "Item ID"
// @Param item body models.Item true "Item object"
// @Success 200 {object} models.Item
// @Failure 404 {object} map[string]string
// @Router /api/v1/items/{id} [put]
func (h *Handler) UpdateItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	// Get the current version from the database
	var currentItem models.Item
	if err := h.repository.FindByID(uint(id), &currentItem); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	var updateItem models.Item
	if err := c.ShouldBindJSON(&updateItem); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	// Keep all existing fields but update name and price from the request
	currentItem.Name = updateItem.Name
	currentItem.Price = updateItem.Price

	// Version check for optimistic locking (if version was provided in request)
	if updateItem.Version > 0 && updateItem.Version != currentItem.Version {
		c.JSON(http.StatusConflict, gin.H{"error": "Item has been modified by another request"})
		return
	}

	// Make sure we're using the version from the request for optimistic locking
	currentItem.Version = updateItem.Version

	// We don't increment the version here, the repository will handle that

	if err := h.repository.Update(&currentItem); err != nil {
		if err.Error() == "version mismatch" {
			c.JSON(http.StatusConflict, gin.H{"error": "Item has been modified by another request"})
			return
		}
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	c.JSON(http.StatusOK, currentItem)
}

// DeleteItem godoc
// @Summary Delete an item
// @Description Delete an item by its ID
// @Tags items
// @Produce json
// @Param id path int true "Item ID"
// @Success 204 "No Content"
// @Failure 404 {object} map[string]string
// @Router /api/v1/items/{id} [delete]
func (h *Handler) DeleteItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	item := &models.Item{}
	item.ID = uint(id)
	if err := h.repository.Delete(item); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	c.Status(http.StatusNoContent)
}

package handlers

import (
	"net/http"
	"strconv"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	repository models.Repository
}

func NewHandler(repository models.Repository) *Handler {
	return &Handler{repository: repository}
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

	if err := h.repository.Create(&item); err != nil {
		var status int
		switch err.(type) {
		case *dberrors.DatabaseError:
			dbErr := err.(*dberrors.DatabaseError)
			switch dbErr.Err {
			case dberrors.ErrValidation:
				status = http.StatusBadRequest
			case dberrors.ErrDuplicateKey:
				status = http.StatusConflict
			default:
				status = http.StatusInternalServerError
			}
		default:
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": err.Error()})
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
	var items []models.Item
	if err := h.repository.List(&items); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	var item models.Item
	if err := h.repository.FindByID(uint(id), &item); err != nil {
		status := http.StatusInternalServerError
		if dbErr, ok := err.(*dberrors.DatabaseError); ok && dbErr.Err == dberrors.ErrNotFound {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	var item models.Item
	if err := h.repository.FindByID(uint(id), &item); err != nil {
		status := http.StatusInternalServerError
		if dbErr, ok := err.(*dberrors.DatabaseError); ok && dbErr.Err == dberrors.ErrNotFound {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	if err := c.ShouldBindJSON(&item); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	if err := h.repository.Update(&item); err != nil {
		var status int
		switch err.(type) {
		case *dberrors.DatabaseError:
			dbErr := err.(*dberrors.DatabaseError)
			switch dbErr.Err {
			case dberrors.ErrValidation:
				status = http.StatusBadRequest
			case dberrors.ErrDuplicateKey:
				status = http.StatusConflict
			default:
				status = http.StatusInternalServerError
			}
		default:
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, item)
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	var item models.Item
	if err := h.repository.FindByID(uint(id), &item); err != nil {
		status := http.StatusInternalServerError
		if dbErr, ok := err.(*dberrors.DatabaseError); ok && dbErr.Err == dberrors.ErrNotFound {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	if err := h.repository.Delete(&item); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusNoContent)
}

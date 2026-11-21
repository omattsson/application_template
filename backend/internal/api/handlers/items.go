package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"backend/internal/api/middleware"
	"backend/internal/models"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	repository models.Repository
	hub        websocket.BroadcastSender
}

func NewHandler(repository models.Repository) *Handler {
	return &Handler{repository: repository}
}

func NewHandlerWithHub(repository models.Repository, hub websocket.BroadcastSender) *Handler {
	return &Handler{repository: repository, hub: hub}
}

func (h *Handler) broadcast(msgType string, payload interface{}) {
	if h.hub == nil {
		return
	}
	msg, err := websocket.NewMessage(msgType, payload)
	if err != nil {
		slog.Error("Failed to create WebSocket message", "type", msgType, "error", err)
		return
	}
	b, err := msg.Bytes()
	if err != nil {
		slog.Error("Failed to serialise WebSocket message", "type", msgType, "error", err)
		return
	}
	h.hub.Broadcast(b)
}

// handleDBError maps a repository error to the status and message of an Item
// response. It delegates to mapError, so a cancelled request context answers
// 499 and an unexpected error answers 500 with a generic message.
func handleDBError(err error) (int, string) {
	return mapError(err, "Item")
}

// CreateItem godoc
// @Summary Create a new item
// @Description Create a new item with the provided information. With IDEMPOTENCY_ENABLED=true the request may carry an Idempotency-Key header: the same key with the same request replays the stored response (header Idempotency-Replayed: true) and creates no second item, the same key with another request is 422 and while the first request still runs it is 409 with Retry-After. A response that is a 5xx or 499 is not stored, so the client can retry.
// @Tags items
// @Accept json
// @Produce json
// @Param item body models.Item true "Item object"
// @Param Idempotency-Key header string false "Optional key (1 to 255 printable ASCII characters) that makes the request safe to retry; used only with IDEMPOTENCY_ENABLED=true" maxlength(255)
// @Success 201 {object} models.Item
// @Failure 400 {object} map[string]string
// @Failure 409 {object} map[string]string "Duplicate, or a request with the same Idempotency-Key is still in progress (Retry-After)"
// @Failure 422 {object} map[string]string "The Idempotency-Key was used with a different request"
// @Failure 413 {object} map[string]string "Request body too large"
// @Failure 499 {object} map[string]string "Client closed request"
// @Failure 500 {object} map[string]string
// @Security BearerAuth
// @Security ApiKeyAuth
// @Failure 401 {object} map[string]string "Authentication required (AUTH_ENABLED=true)"
// @Router /api/v1/items [post]
func (h *Handler) CreateItem(c *gin.Context) {
	var item models.Item
	if err := c.ShouldBindJSON(&item); err != nil {
		respondBindError(c, err)
		return
	}

	if item.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Name is required"})
		return
	}

	// Version is server-managed; force initial value regardless of client input.
	item.Version = 1

	// The write is detached from the request: a client that leaves cannot skip it.
	ctx, cancel := detachedCtx(c)
	defer cancel()
	if err := h.repository.Create(ctx, &item); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	// The audit entry (when the audit middleware runs) names the new item.
	middleware.SetAuditEntityID(c, strconv.FormatUint(uint64(item.ID), 10))
	h.broadcast("item.created", item)
	c.JSON(http.StatusCreated, item)
}

// GetItems godoc
// @Summary Get all items
// @Description Get a list of all items. Sort with sort and order; without them the order is id ascending. Rows with an equal sort value are ordered by id in the same direction, so pages never repeat or skip a row. The X-Total-Count header holds the number of items that match the filter, ignoring limit and offset; the body is a plain array.
// @Tags items
// @Produce json
// @Param limit query int false "Page size (omit for all items)"
// @Param offset query int false "Rows to skip (needs limit)"
// @Param name query string false "Name contains the text"
// @Param name_exact query string false "Name equals the text"
// @Param min_price query number false "Lowest price"
// @Param max_price query number false "Highest price"
// @Param sort query string false "Sort key (default id)" Enums(id, name, price, created_at, updated_at)
// @Param order query string false "Sort direction (default asc)" Enums(asc, desc)
// @Success 200 {array} models.Item
// @Header 200 {integer} X-Total-Count "Number of items that match the filter, ignoring limit and offset"
// @Failure 400 {object} map[string]string
// @Failure 499 {object} map[string]string "Client closed request"
// @Failure 500 {object} map[string]string
// @Security BearerAuth
// @Security ApiKeyAuth
// @Failure 401 {object} map[string]string "Authentication required (AUTH_ENABLED=true)"
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

	sortBy, sortDesc, ok := parseSort(c, models.ItemSortKeys)
	if !ok {
		return
	}

	filter := models.ItemFilter{
		Name:      c.Query("name"),
		NameExact: c.Query("name_exact"),
		MinPrice:  minPrice,
		MaxPrice:  maxPrice,
		SortBy:    sortBy,
		SortDesc:  sortDesc,
	}
	if limit <= 0 {
		// Without a limit the whole list is returned and offset is ignored.
		offset = 0
	}

	var items []models.Item
	// The body stays a plain array. ListPaged counts every row that matches the
	// filter (limit and offset do not apply), and the count goes in a header
	// that CORS exposes, so a pager can show the number of pages.
	total, err := h.repository.ListPaged(c.Request.Context(), &items, limit, offset, filter)
	if err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	c.Header(middleware.TotalCountHeader, strconv.FormatInt(total, 10))
	c.JSON(http.StatusOK, items)
}

// GetItem godoc
// @Summary Get an item by ID
// @Description Get an item by its ID
// @Tags items
// @Produce json
// @Param id path int true "Item ID"
// @Success 200 {object} models.Item
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 499 {object} map[string]string "Client closed request"
// @Failure 500 {object} map[string]string
// @Security BearerAuth
// @Security ApiKeyAuth
// @Failure 401 {object} map[string]string "Authentication required (AUTH_ENABLED=true)"
// @Router /api/v1/items/{id} [get]
func (h *Handler) GetItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	var item models.Item
	if err := h.repository.FindByID(c.Request.Context(), uint(id), &item); err != nil {
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
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Failure 413 {object} map[string]string "Request body too large"
// @Failure 499 {object} map[string]string "Client closed request"
// @Failure 500 {object} map[string]string
// @Security BearerAuth
// @Security ApiKeyAuth
// @Failure 401 {object} map[string]string "Authentication required (AUTH_ENABLED=true)"
// @Router /api/v1/items/{id} [put]
func (h *Handler) UpdateItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	// Get the current version from the database
	var currentItem models.Item
	if err := h.repository.FindByID(c.Request.Context(), uint(id), &currentItem); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	var updateItem models.Item
	if err := c.ShouldBindJSON(&updateItem); err != nil {
		respondBindError(c, err)
		return
	}

	// Update fields from request
	currentItem.Name = updateItem.Name
	currentItem.Price = updateItem.Price

	// Optimistic locking: if the client provided a version, use it so the
	// repository can detect conflicts. If version=0 (not provided), the
	// repository uses the version we just read — this still detects conflicts
	// that occur between our FindByID and the repository's WHERE-version check,
	// but the client must send the version to guarantee end-to-end safety.
	if updateItem.Version > 0 {
		currentItem.Version = updateItem.Version
	}

	// The write is detached from the request: a client that leaves cannot skip it.
	ctx, cancel := detachedCtx(c)
	defer cancel()
	if err := h.repository.Update(ctx, &currentItem); err != nil {
		if strings.Contains(err.Error(), "version mismatch") {
			c.JSON(http.StatusConflict, gin.H{"error": "Item has been modified by another request"})
			return
		}
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	h.broadcast("item.updated", currentItem)
	c.JSON(http.StatusOK, currentItem)
}

// DeleteItem godoc
// @Summary Delete an item
// @Description Delete an item by its ID
// @Tags items
// @Produce json
// @Param id path int true "Item ID"
// @Success 204 "No Content"
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 499 {object} map[string]string "Client closed request"
// @Failure 500 {object} map[string]string
// @Security BearerAuth
// @Security ApiKeyAuth
// @Failure 401 {object} map[string]string "Authentication required (AUTH_ENABLED=true)"
// @Router /api/v1/items/{id} [delete]
func (h *Handler) DeleteItem(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID format"})
		return
	}

	// Delete directly — the repository returns ErrNotFound if the item doesn't exist.
	// This avoids a race condition between a FindByID check and the actual delete.
	item := &models.Item{Base: models.Base{ID: uint(id)}}
	// The write is detached from the request: a client that leaves cannot skip it.
	ctx, cancel := detachedCtx(c)
	defer cancel()
	if err := h.repository.Delete(ctx, item); err != nil {
		status, message := handleDBError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}

	h.broadcast("item.deleted", gin.H{"id": id})
	c.Status(http.StatusNoContent)
}

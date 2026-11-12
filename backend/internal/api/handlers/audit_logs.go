package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// Bounds of the audit log filters. The patterns are the shape of the values
// that the application writes: an action and an entity type are lower-case
// words with underscores, an entity ID a UUID or a number, a user ID a UUID.
const (
	// maxAuditOffset is the deepest offset a page can start at; deeper pages
	// use the cursor (an offset scan costs the database every skipped row).
	maxAuditOffset = 100_000
	// maxAuditCursorLen bounds a cursor before it is decoded.
	maxAuditCursorLen = 512
	maxAuditWordLen   = 100
)

var (
	auditWordRe     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	auditEntityIDRe = regexp.MustCompile(`^[A-Za-z0-9._:@-]+$`)
	auditUserIDRe   = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	auditCursorRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// AuditLogHandler handles the audit log query endpoint. It is for admins and
// needs a login session (requireAdminSession).
type AuditLogHandler struct {
	auditRepo models.AuditLogRepository
	users     models.UserRepository
}

// NewAuditLogHandler creates an AuditLogHandler. users is read to check that
// the caller is still an enabled admin.
func NewAuditLogHandler(auditRepo models.AuditLogRepository, users models.UserRepository) *AuditLogHandler {
	return &AuditLogHandler{auditRepo: auditRepo, users: users}
}

// auditFilterError returns the client message of an invalid filter as an error.
func auditFilterError(msg string) error { return errors.New(msg) }

// parseAuditFilters reads and validates the filter, time range and paging
// parameters. The error text is a client message.
func parseAuditFilters(c *gin.Context) (models.AuditLogFilters, error) {
	f := models.AuditLogFilters{
		UserID:     c.Query("user_id"),
		EntityType: c.Query("entity_type"),
		EntityID:   c.Query("entity_id"),
		Action:     c.Query("action"),
		Cursor:     c.Query("cursor"),
		Limit:      models.DefaultPageSize,
	}

	if f.UserID != "" && (len(f.UserID) > maxIDParamLen || !auditUserIDRe.MatchString(f.UserID)) {
		return f, auditFilterError("Invalid user_id parameter")
	}
	if f.Action != "" && (len(f.Action) > maxAuditWordLen || !auditWordRe.MatchString(f.Action)) {
		return f, auditFilterError("Invalid action parameter")
	}
	if f.EntityType != "" && (len(f.EntityType) > maxAuditWordLen || !auditWordRe.MatchString(f.EntityType)) {
		return f, auditFilterError("Invalid entity_type parameter")
	}
	if f.EntityID != "" && (len(f.EntityID) > models.MaxAuditEntityIDLen || !auditEntityIDRe.MatchString(f.EntityID)) {
		return f, auditFilterError("Invalid entity_id parameter")
	}
	if f.Cursor != "" && (len(f.Cursor) > maxAuditCursorLen || !auditCursorRe.MatchString(f.Cursor)) {
		return f, auditFilterError("Invalid cursor parameter")
	}

	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, auditFilterError("Invalid limit parameter")
		}
		// 0 is the default; a page is never larger than the maximum.
		if n > 0 {
			f.Limit = min(n, models.MaxPageSize)
		}
	}
	if v := c.Query("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, auditFilterError("Invalid offset parameter")
		}
		f.Offset = min(n, maxAuditOffset)
	}

	var err error
	if f.StartDate, err = parseAuditTime(c.Query("start_date")); err != nil {
		return f, auditFilterError("Invalid start_date format (use RFC3339)")
	}
	if f.EndDate, err = parseAuditTime(c.Query("end_date")); err != nil {
		return f, auditFilterError("Invalid end_date format (use RFC3339)")
	}
	if f.StartDate != nil && f.EndDate != nil && f.StartDate.After(*f.EndDate) {
		return f, auditFilterError("start_date must not be after end_date")
	}
	return f, nil
}

// parseAuditTime parses an RFC3339 time (a zone is required, fractional
// seconds are allowed). An empty value is no filter.
func parseAuditTime(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListAuditLogs godoc
// @Summary     List audit logs
// @Description Returns one page of audit entries, newest first. Admin only, with a login session (an API key is refused with 403); the caller must still be an enabled admin in the database. Filters: user_id, action, entity_type, entity_id and a time range (start_date, end_date: RFC3339 with a zone, inclusive); an invalid value is 400. Paging: limit (default 25, at most 100) and offset (at most 100000), or the next_cursor of the previous page (keyset paging for deep pages; the cursor ignores offset and the answer has no exact total). An entry holds the action, the entity, the user and details (method, route, status, client IP, request ID and small values such as the old and new role); never a request or response body, header, password or key.
// @Tags        audit-logs
// @Produce     json
// @Security    BearerAuth
// @Param       user_id     query    string false "Filter by user ID"
// @Param       entity_type query    string false "Filter by entity type (lower case words with underscores)"
// @Param       entity_id   query    string false "Filter by entity ID"
// @Param       action      query    string false "Filter by action (lower case words with underscores)"
// @Param       start_date  query    string false "Start (RFC3339 with a zone, inclusive)"
// @Param       end_date    query    string false "End (RFC3339 with a zone, inclusive)"
// @Param       limit       query    int    false "Page size (default 25, at most 100)"
// @Param       offset      query    int    false "Offset (default 0, at most 100000)"
// @Param       cursor      query    string false "next_cursor of the previous page (overrides offset)"
// @Success     200         {object} models.PaginatedAuditLogs
// @Failure     400         {object} map[string]string
// @Failure     401         {object} map[string]string
// @Failure     403         {object} map[string]string
// @Failure     499         {object} map[string]string "Client closed request"
// @Failure     500         {object} map[string]string
// @Router      /api/v1/audit-logs [get]
func (h *AuditLogHandler) ListAuditLogs(c *gin.Context) {
	if _, ok := requireAdminSession(c, h.users); !ok {
		return
	}
	filters, err := parseAuditFilters(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.auditRepo.List(c.Request.Context(), filters)
	if err != nil {
		status, message := mapError(err, "Audit log")
		if status >= http.StatusInternalServerError {
			slog.ErrorContext(c.Request.Context(), "Failed to list audit logs", "error", err, "request_id", c.GetString(middleware.RequestIDKey))
		}
		c.JSON(status, gin.H{"error": message})
		return
	}

	data := result.Data
	if data == nil {
		data = []models.AuditLog{}
	}
	noStore(c)
	c.JSON(http.StatusOK, models.PaginatedAuditLogs{
		Data:       data,
		Total:      result.Total,
		Limit:      filters.Limit,
		Offset:     filters.Offset,
		NextCursor: result.NextCursor,
	})
}

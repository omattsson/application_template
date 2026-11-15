package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"backend/internal/ctxutil"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AuditLogger allows the audit middleware to persist audit entries.
// models.AuditLogRepository satisfies this interface.
type AuditLogger interface {
	Create(ctx context.Context, log *models.AuditLog) error
}

// Context keys of the hints that a handler gives the audit middleware. They
// are strings because gin.Context.Set takes a string key; the prefix keeps
// them apart from the keys of other middleware.
const (
	auditKeySkip     = "audit.skip"
	auditKeyAction   = "audit.action"
	auditKeyEntityID = "audit.entity_id"
	auditKeyDetails  = "audit.details"
	auditKeyForce    = "audit.force"
)

// SkipAudit tells the audit middleware not to record this request, although
// it succeeded. A handler calls it for a mutating request that changed
// nothing (a no-op), for example a role change to the current role, and for
// an endpoint whose use must not be recorded.
func SkipAudit(c *gin.Context) { c.Set(auditKeySkip, true) }

// AuditCommitted tells the audit middleware to record this request although
// its status is an error (400 and above). A handler calls it when it committed
// a change and then answers with an error (for example a failed revocation
// after a role change): the change happened and must be in the log.
func AuditCommitted(c *gin.Context) { c.Set(auditKeyForce, true) }

// SetAuditAction replaces the action of the route table for this request. A
// handler that serves two actions on one route (disable and enable) uses it.
// The action must be one of KnownAuditActions.
func SetAuditAction(c *gin.Context, action string) { c.Set(auditKeyAction, action) }

// SetAuditEntityID sets the entity ID of the entry. A create handler calls it
// with the ID of the new entity (the middleware never reads a response body);
// a handler that has the canonical ID of the entity calls it so the entry
// does not hold the raw path value.
func SetAuditEntityID(c *gin.Context, id string) { c.Set(auditKeyEntityID, id) }

// AddAuditDetail adds one key to the details of the entry. The handler must
// pass only small, non-secret values (an old role, a flag): never a
// password, token, key or request field. A key that the middleware writes
// itself (method, path, status, client_ip, request_id) cannot be replaced.
func AddAuditDetail(c *gin.Context, key string, value any) {
	details, _ := c.Get(auditKeyDetails)
	m, ok := details.(map[string]any)
	if !ok {
		m = map[string]any{}
		c.Set(auditKeyDetails, m)
	}
	m[key] = value
}

// Audit entry limits. They keep every insert inside its column and every
// entry small.
const (
	// maxAuditPathLen is the longest route template that an entry keeps.
	maxAuditPathLen = 255
	// maxAuditDetailsLen is the size at which the extra details of a handler
	// are dropped (the entry keeps its own keys).
	maxAuditDetailsLen = 4096
	// maxAuditRequestIDLen is the longest request ID that an entry keeps.
	maxAuditRequestIDLen = 64
)

// DefaultAuditMaxInFlight is the number of audit writes that can run at the
// same time. A write over the bound is dropped (logged and counted), so a
// stalled database cannot grow the number of goroutines without limit.
const DefaultAuditMaxInFlight = 64

// AuditRecorder writes the audit entries of mutating requests. The write
// runs after the response, in a goroutine, on a context that the client
// cannot cancel and that ends after ctxutil.WriteTimeout: a failing or slow
// audit write never fails or delays a request. At most maxInFlight writes run
// at once; over the bound an entry is dropped with an Error log that names
// the user, action and entity (and counted, see Dropped). Shutdown waits for
// the writes in flight, so a graceful stop loses no entry.
//
// Recorded: a POST, PUT, PATCH or DELETE that an authenticated user made and
// that ended below 400 (or that a handler marked with AuditCommitted), unless
// a handler called SkipAudit. Not recorded:
// reads; requests without a user (an audit entry needs a user, so failed
// logins and other unauthenticated requests are out of scope); failed
// requests (4xx and 5xx: a refused or failed request changed nothing); the
// endpoints under /auth (authentication events), except the routes with an
// entry in auditRoutes (the self-service password change).
//
// An entry holds the action, the entity type and ID, the user, and in its
// details the method, the route template (never the query string or the raw
// path), the status, the client IP (c.ClientIP, so only the trusted proxies
// can set it) and the request ID, plus the small values that the handler
// added. It never holds a request or response body or a header.
type AuditRecorder struct {
	repo    AuditLogger
	sem     chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64
	mu      sync.Mutex // guards closed and wg.Add against Shutdown
	closed  bool
}

// NewAuditRecorder creates a recorder that writes to repo with
// DefaultAuditMaxInFlight writes at once.
func NewAuditRecorder(repo AuditLogger) *AuditRecorder {
	return newAuditRecorder(repo, DefaultAuditMaxInFlight)
}

func newAuditRecorder(repo AuditLogger, maxInFlight int) *AuditRecorder {
	if maxInFlight < 1 {
		maxInFlight = 1
	}
	return &AuditRecorder{repo: repo, sem: make(chan struct{}, maxInFlight)}
}

// NewAuditMiddleware returns the audit middleware of a recorder that nobody
// can shut down. Use NewAuditRecorder and Shutdown where the process stops
// gracefully. A nil repo gives middleware that records nothing.
func NewAuditMiddleware(repo AuditLogger) gin.HandlerFunc {
	if repo == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return NewAuditRecorder(repo).Middleware()
}

// Dropped returns the number of entries that were not written because the
// recorder was full or shut down.
func (a *AuditRecorder) Dropped() int64 {
	if a == nil {
		return 0
	}
	return a.dropped.Load()
}

// Shutdown stops the recorder from accepting entries and waits for the writes
// in flight, until ctx ends (then it returns ctx.Err()). It can be called more
// than once. A nil recorder does nothing.
func (a *AuditRecorder) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isAuditedMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// Middleware returns the gin middleware. It must run after the
// authentication middleware (or anywhere: it reads the user after the
// handler).
func (a *AuditRecorder) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method
		if !isAuditedMethod(method) {
			c.Next()
			return
		}
		route := AuditRouteFor(method, c.FullPath())

		c.Next()

		if route.Skip || c.GetBool(auditKeySkip) {
			return
		}
		status := c.Writer.Status()
		if status >= http.StatusBadRequest && !c.GetBool(auditKeyForce) {
			return
		}
		userID := GetUserIDFromContext(c)
		if userID == "" {
			return
		}
		entry := buildAuditEntry(c, route, userID, status)
		a.submit(c.Request.Context(), entry)
	}
}

// submit starts the write of an entry, or drops it when the recorder is full
// or shut down.
func (a *AuditRecorder) submit(reqCtx context.Context, entry *models.AuditLog) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.drop(entry, "shut down")
		return
	}
	select {
	case a.sem <- struct{}{}:
		a.wg.Add(1)
		a.mu.Unlock()
	default:
		a.mu.Unlock()
		a.drop(entry, "too many writes in flight")
		return
	}

	// Detached: a client that leaves cannot skip the entry, and a stalled
	// database cannot hold the goroutine longer than WriteTimeout.
	ctx, cancel := ctxutil.Detach(reqCtx, ctxutil.WriteTimeout)
	go func() {
		defer a.wg.Done()
		defer func() { <-a.sem }()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic while writing an audit log", "panic", r)
			}
		}()
		if err := a.repo.Create(ctx, entry); err != nil {
			slog.Error("failed to create audit log", "error", err,
				"user_id", entry.UserID, "action", entry.Action,
				"entity_type", entry.EntityType, "entity_id", entry.EntityID)
		}
	}()
}

func (a *AuditRecorder) drop(entry *models.AuditLog, reason string) {
	a.dropped.Add(1)
	slog.Error("audit log dropped", "reason", reason,
		"user_id", entry.UserID, "action", entry.Action,
		"entity_type", entry.EntityType, "entity_id", entry.EntityID)
}

// AuditRoute describes how the audit middleware records a route.
type AuditRoute struct {
	// Action is the audit action, for example "change_role".
	Action string
	// EntityType is the audit entity type, for example "user".
	EntityType string
	// IDParam is the path parameter with the entity ID.
	IDParam string
	// IDFromHandler is true for a route without an ID parameter whose handler
	// gives the entity ID with SetAuditEntityID (the password change names the
	// caller).
	IDFromHandler bool
	// Skip means no audit entry: the endpoint is an authentication event (a
	// route under /auth/ that has no table entry).
	Skip bool
	// CRUD is true for a route that is not in the route table: the action
	// comes from the HTTP method and the entity type from the route.
	CRUD bool
}

// Audit actions and entity types. The action of a plain CRUD route comes
// from the HTTP method.
const (
	AuditActionCreate     = "create"
	AuditActionUpdate     = "update"
	AuditActionDelete     = "delete"
	AuditActionDisable    = "disable"
	AuditActionEnable     = "enable"
	AuditActionChangeRole = "change_role"
	// AuditActionChangePassword is a user changing their own password
	// (PUT /auth/me/password).
	AuditActionChangePassword = "change_password"

	AuditEntityItem   = "item"
	AuditEntityUser   = "user"
	AuditEntityAPIKey = "api_key"
)

// KnownAuditActions lists every action that new audit entries can have. The
// audit log page offers the same list as filter options.
var KnownAuditActions = []string{
	AuditActionCreate, AuditActionUpdate, AuditActionDelete,
	AuditActionDisable, AuditActionEnable, AuditActionChangeRole, AuditActionChangePassword,
}

// KnownAuditEntityTypes lists every entity type that new audit entries can
// have. The audit log page offers the same list as filter options.
var KnownAuditEntityTypes = []string{AuditEntityItem, AuditEntityUser, AuditEntityAPIKey}

// IsKnownAuditAction reports whether action is in KnownAuditActions.
func IsKnownAuditAction(action string) bool { return slices.Contains(KnownAuditActions, action) }

// IsKnownAuditEntityType reports whether entityType is in KnownAuditEntityTypes.
func IsKnownAuditEntityType(entityType string) bool {
	return slices.Contains(KnownAuditEntityTypes, entityType)
}

const (
	auditAPIPrefix  = "/api/v1"
	auditAuthPrefix = "/auth/"
)

// auditRoutes maps "METHOD route pattern" (gin FullPath without /api/v1) to
// the audit entry of operations that are not plain CRUD. Keep it in sync with
// the routes; the routes test checks that every mutating route gives a known
// action and entity type.
var auditRoutes = map[string]AuditRoute{
	"PUT /users/:id/role": {Action: AuditActionChangeRole, EntityType: AuditEntityUser, IDParam: "id"},
	// The handler sets AuditActionEnable when the request enables the user.
	"PUT /users/:id/disabled": {Action: AuditActionDisable, EntityType: AuditEntityUser, IDParam: "id"},
	// A route under /auth/ is an authentication event and is skipped unless it
	// has an entry here. A password change is a change of the account: the
	// handler names the caller as the entity (SetAuditEntityID) and never puts
	// a password in the details.
	"PUT /auth/me/password": {Action: AuditActionChangePassword, EntityType: AuditEntityUser, IDFromHandler: true},
}

// AuditRouteFor returns how the audit middleware records a request with this
// method and route pattern (gin FullPath). The endpoints under /auth are not
// recorded, unless the route table has an entry for them (the password
// change). Routes in the route table get their entry from it. Other routes
// are plain CRUD: the action comes from the method and the entity type from
// the last resource segment of the route; the entity ID is the "id" path
// parameter, or what the handler gives to SetAuditEntityID (a create).
func AuditRouteFor(method, fullPath string) AuditRoute {
	trimmed := strings.TrimPrefix(fullPath, auditAPIPrefix)
	// An explicit entry wins, also under /auth/.
	if r, ok := auditRoutes[method+" "+trimmed]; ok {
		return r
	}
	if strings.HasPrefix(trimmed, auditAuthPrefix) {
		return AuditRoute{Skip: true}
	}
	r := AuditRoute{EntityType: extractEntityType(fullPath), CRUD: true}
	switch method {
	case http.MethodPost:
		r.Action = AuditActionCreate
	case http.MethodPut, http.MethodPatch:
		r.Action = AuditActionUpdate
	case http.MethodDelete:
		r.Action = AuditActionDelete
	}
	return r
}

// auditEntityID returns the entity ID of the entry: what the handler set,
// else the path parameter of the route.
func auditEntityID(c *gin.Context, route AuditRoute) string {
	if id := c.GetString(auditKeyEntityID); id != "" {
		return id
	}
	switch {
	case route.IDParam != "":
		return c.Param(route.IDParam)
	case route.CRUD:
		return c.Param("id")
	}
	return ""
}

// auditExtraDetails returns the details that the handler added, or none when
// they are too large.
func auditExtraDetails(c *gin.Context) map[string]any {
	extra, _ := c.Get(auditKeyDetails)
	m, ok := extra.(map[string]any)
	if !ok {
		return nil
	}
	if raw, err := json.Marshal(m); err != nil || len(raw) >= maxAuditDetailsLen {
		return nil
	}
	return m
}

// buildAuditEntry builds the entry of a request that ended with status.
func buildAuditEntry(c *gin.Context, route AuditRoute, userID string, status int) *models.AuditLog {
	action := route.Action
	if v := c.GetString(auditKeyAction); v != "" {
		action = v
	}

	details := map[string]any{}
	for k, v := range auditExtraDetails(c) {
		details[k] = v
	}
	// The keys of the middleware come last: a handler cannot replace them.
	details["method"] = c.Request.Method
	details["path"] = truncateAuditText(c.FullPath(), maxAuditPathLen)
	details["status"] = status
	details["client_ip"] = c.ClientIP()
	if id := c.GetString(RequestIDKey); id != "" {
		details["request_id"] = truncateAuditText(id, maxAuditRequestIDLen)
	}

	entry := &models.AuditLog{
		ID:         uuid.New().String(),
		UserID:     userID,
		Username:   truncateAuditText(GetUsernameFromContext(c), models.MaxUsernameLen),
		Action:     action,
		EntityType: route.EntityType,
		EntityID:   truncateEntityID(auditEntityID(c, route)),
		Timestamp:  time.Now().UTC(),
	}
	if raw, err := json.Marshal(details); err == nil {
		entry.Details = string(raw)
	}
	return entry
}

// truncateAuditText cuts s to at most limit bytes at a UTF-8 rune start.
func truncateAuditText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// truncateEntityID cuts an entity ID to models.MaxAuditEntityIDLen bytes (at
// a UTF-8 rune start), so the insert never fails on the column size. IDs are
// UUIDs or numbers; a longer value is not a valid ID, and the cut keeps the
// entry.
func truncateEntityID(id string) string {
	return truncateAuditText(id, models.MaxAuditEntityIDLen)
}

// extractEntityType derives the entity type from the route path, for example
// "/api/v1/api-keys/:id" gives "api_key".
func extractEntityType(fullPath string) string {
	parts := strings.Split(strings.Trim(fullPath, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		seg := parts[i]
		if seg == "" || strings.HasPrefix(seg, ":") || strings.HasPrefix(seg, "*") {
			continue
		}
		return normalizeEntityType(seg)
	}
	return "unknown"
}

// entityTypeBySegment maps route segments to singular entity type names.
var entityTypeBySegment = map[string]string{
	"items":    AuditEntityItem,
	"users":    AuditEntityUser,
	"api-keys": AuditEntityAPIKey,
}

func normalizeEntityType(segment string) string {
	if v, ok := entityTypeBySegment[segment]; ok {
		return v
	}
	return strings.ReplaceAll(segment, "-", "_")
}

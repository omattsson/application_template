package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"backend/internal/api/middleware"
	"backend/internal/models"
	"backend/internal/websocket"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
)

// Limits of API keys. The maximum lifetime and the number of active keys per
// user can be changed with APIKeyOptions; a key never lives without an expiry.
const (
	// DefaultAPIKeyMaxLifetimeDays is the longest lifetime of a new key.
	DefaultAPIKeyMaxLifetimeDays = 365
	// DefaultAPIKeyMaxActive is the number of active (not expired) keys one
	// user can have.
	DefaultAPIKeyMaxActive = 25
	// maxAPIKeyNameChars is the longest name of a key, in characters.
	maxAPIKeyNameChars = 100
	// maxIDParamLen is the size of an ID column: a longer ID cannot exist.
	maxIDParamLen = 36
)

// Messages of the API key endpoints.
const (
	msgAPIKeySessionOnly = "API keys can only be managed with a login session" // #nosec G101 -- an error message, not a credential
	msgForbidden         = "Forbidden"
	msgAPIKeyName        = "name is required, at most 100 characters: letters, digits, spaces and . _ - : @ / ( ) +"
)

// apiKeyNameRe is the charset of a key name (after trimming): letters and
// digits of any alphabet, space and a few punctuation marks. It refuses
// control and format characters (newlines, bidi overrides) and markup.
var apiKeyNameRe = regexp.MustCompile(`^[\p{L}\p{N} ._:@/()+\-]+$`)

// APIKeyOptions are the limits that APIKeyHandler enforces on new keys. A
// value of 0 or less is the default.
type APIKeyOptions struct {
	// MaxLifetimeDays is the longest lifetime of a new key in days (default
	// DefaultAPIKeyMaxLifetimeDays). Every key has an expiry.
	MaxLifetimeDays int
	// MaxActivePerUser is the number of keys that have not expired that one
	// user can have (default DefaultAPIKeyMaxActive).
	MaxActivePerUser int
}

// APIKeyHandler handles the API key endpoints: a user creates, lists and
// deletes their own keys; an admin can also list and delete the keys of
// others. The endpoints need a login session (a JWT): a request that was
// authenticated with an API key is refused, so a stolen key cannot create a
// successor that outlives its own revocation.
type APIKeyHandler struct {
	keys      models.APIKeyRepository
	users     models.UserRepository
	wsRevoker websocket.ClientRevoker // nil: no socket close on delete
	opts      APIKeyOptions
}

// NewAPIKeyHandler creates an APIKeyHandler.
func NewAPIKeyHandler(keys models.APIKeyRepository, users models.UserRepository, opts APIKeyOptions) *APIKeyHandler {
	if opts.MaxLifetimeDays <= 0 {
		opts.MaxLifetimeDays = DefaultAPIKeyMaxLifetimeDays
	}
	if opts.MaxActivePerUser <= 0 {
		opts.MaxActivePerUser = DefaultAPIKeyMaxActive
	}
	return &APIKeyHandler{keys: keys, users: users, opts: opts}
}

// SetWebSocketRevoker makes DeleteAPIKey close the WebSocket connections that
// were opened with the deleted key (on this replica; other replicas close
// theirs at the next revalidation). nil turns it off.
func (h *APIKeyHandler) SetWebSocketRevoker(r websocket.ClientRevoker) { h.wsRevoker = r }

// CreateAPIKeyRequest is the request body for creating an API key. Exactly
// one of expires_at and expires_in_days is required. Any other field is
// refused (there is no role: a key has the role of its owner).
type CreateAPIKeyRequest struct {
	Name          string  `json:"name"`
	ExpiresAt     *string `json:"expires_at,omitempty"`
	ExpiresInDays *int    `json:"expires_in_days,omitempty"`
}

// CreateAPIKeyResponse is returned once, at creation. raw_key is never stored
// and cannot be retrieved again.
type CreateAPIKeyResponse struct {
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	// Prefix is the first 16 hex characters of the key (not secret enough to
	// use alone): it identifies the key in the list.
	Prefix string `json:"prefix"`
	// RawKey is "sk_" and 64 hex characters, shown once.
	RawKey string `json:"raw_key"`
}

// APIKeyListResponse is one page of API keys.
type APIKeyListResponse struct {
	Data     []*models.APIKey `json:"data"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"pageSize"`
}

// sessionOnly answers 401 (no identity) or 403 (anything but a login session)
// and reports false when the request must not go on. It is an allowlist: only
// the JWT method passes, so an API key, a request whose route set a user
// without an auth method, and any method added later are refused.
func sessionOnly(c *gin.Context) (userID string, ok bool) {
	userID = middleware.GetUserIDFromContext(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgAuthRequired})
		return "", false
	}
	if middleware.GetAuthMethodFromContext(c) != middleware.AuthMethodJWT {
		c.JSON(http.StatusForbidden, gin.H{"error": msgAPIKeySessionOnly})
		return "", false
	}
	return userID, true
}

// decodeStrictJSON decodes one JSON value from the body into dst and refuses
// unknown fields and trailing data.
func decodeStrictJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// parseKeyExpiry returns the expiry of a new key from the request, or the
// message of the 400 answer. The expiry is required, in the future and at most
// maxDays days away (date-only values end at 23:59:59 UTC of that day).
func parseKeyExpiry(req *CreateAPIKeyRequest, now time.Time, maxDays int) (time.Time, string) {
	if req.ExpiresAt != nil && *req.ExpiresAt == "" {
		req.ExpiresAt = nil
	}
	if req.ExpiresAt != nil && req.ExpiresInDays != nil {
		return time.Time{}, "Specify either expires_at or expires_in_days, not both"
	}
	maxDate := now.AddDate(0, 0, maxDays)
	maxExpiry := time.Date(maxDate.Year(), maxDate.Month(), maxDate.Day(), 23, 59, 59, 0, time.UTC)

	var expiresAt time.Time
	switch {
	case req.ExpiresInDays != nil:
		days := *req.ExpiresInDays
		if days <= 0 || days > maxDays {
			return time.Time{}, fmt.Sprintf("expires_in_days must be between 1 and %d", maxDays)
		}
		expiresAt = now.AddDate(0, 0, days)
	case req.ExpiresAt != nil:
		parsed, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			day, derr := time.Parse("2006-01-02", *req.ExpiresAt)
			if derr != nil {
				return time.Time{}, "Invalid expires_at format; use YYYY-MM-DD or RFC3339"
			}
			parsed = day.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
		}
		expiresAt = parsed.UTC()
	default:
		return time.Time{}, "An expiry is required; set expires_at or expires_in_days"
	}

	if !expiresAt.After(now) {
		return time.Time{}, "Expiry must be in the future"
	}
	if expiresAt.After(maxExpiry) {
		return time.Time{}, fmt.Sprintf("Expiry exceeds the maximum lifetime of %d days", maxDays)
	}
	return expiresAt, ""
}

// validKeyName returns the trimmed name and whether it is acceptable.
func validKeyName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxAPIKeyNameChars || !apiKeyNameRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// CreateAPIKey godoc
// @Summary      Create an API key
// @Description  Creates a key for the caller. The key has the role of its owner at the time of each request (a demoted owner's keys are demoted). An expiry is required: set expires_at (YYYY-MM-DD or RFC3339) or expires_in_days, not both, at most 365 days ahead by default. The caller can have 25 active keys by default (409 above that). The raw key (sk_ and 64 hex characters) is returned once in raw_key, with Cache-Control no-store, and cannot be retrieved again; only its SHA-256 is stored. A request authenticated with an API key is refused (403): manage keys with a login session. Unknown request fields are refused.
// @Tags         api-keys
// @Accept       json
// @Produce      json
// @Param        request  body      CreateAPIKeyRequest  true  "Key name and expiry"
// @Success      201  {object}  CreateAPIKeyResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      413  {object}  map[string]string "Request body too large"
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/api-keys [post]
func (h *APIKeyHandler) CreateAPIKey(c *gin.Context) {
	userID, ok := sessionOnly(c)
	if !ok {
		return
	}

	var req CreateAPIKeyRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}
	name, ok := validKeyName(req.Name)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgAPIKeyName})
		return
	}
	now := time.Now().UTC()
	expiresAt, msg := parseKeyExpiry(&req, now, h.opts.MaxLifetimeDays)
	if msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	// A client that leaves must not turn a committed key into a 499.
	ctx, cancel := detachedCtx(c)
	defer cancel()

	user, err := h.users.FindByID(ctx, userID)
	if err != nil {
		status, message := mapError(err, "User")
		c.JSON(status, gin.H{"error": message})
		return
	}
	if user.Disabled {
		c.JSON(http.StatusForbidden, gin.H{"error": msgAccountDisabled})
		return
	}

	// Generate the key only after all checks passed.
	rawKey, prefix, hash, err := models.GenerateAPIKey()
	if err != nil {
		slog.ErrorContext(ctx, "API key generation failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}
	key := &models.APIKey{UserID: userID, Name: name, KeyHash: hash, Prefix: prefix, ExpiresAt: &expiresAt}
	if err := h.keys.CreateLimited(ctx, key, h.opts.MaxActivePerUser, now); err != nil {
		if errors.Is(err, models.ErrTooManyAPIKeys) {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Maximum number of active API keys reached (%d)", h.opts.MaxActivePerUser)})
			return
		}
		status, message := mapError(err, "User")
		c.JSON(status, gin.H{"error": message})
		return
	}
	slog.InfoContext(ctx, "API key created", "key_id", key.ID, "user_id", userID)
	// The audit entry names the new key (never its value).
	middleware.SetAuditEntityID(c, key.ID)

	noStore(c)
	c.JSON(http.StatusCreated, CreateAPIKeyResponse{
		ID:        key.ID,
		Name:      key.Name,
		Prefix:    prefix,
		RawKey:    models.FormatAPIKey(rawKey),
		CreatedAt: key.CreatedAt,
		ExpiresAt: key.ExpiresAt,
	})
}

// ListAPIKeys godoc
// @Summary      List API keys
// @Description  Returns one page of the caller's keys, newest first. The key itself is never returned, only its prefix. An admin can pass all=true for the keys of every user, or user_id for one user; for any other role both are 403 (except the caller's own user_id). A request authenticated with an API key is refused (403).
// @Tags         api-keys
// @Produce      json
// @Param        page      query  int     false  "Page, from 1"
// @Param        pageSize  query  int     false  "Page size (default 25, at most 100)"
// @Param        all       query  bool    false  "Admin only: the keys of all users"
// @Param        user_id   query  string  false  "Admin only: the keys of one user"
// @Success      200  {object}  APIKeyListResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/api-keys [get]
func (h *APIKeyHandler) ListAPIKeys(c *gin.Context) {
	callerID, ok := sessionOnly(c)
	if !ok {
		return
	}
	isAdmin := middleware.GetRoleFromContext(c) == models.RoleAdmin

	all := false
	if v := c.Query("all"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid all parameter"})
			return
		}
		all = parsed
	}
	targetID := c.Query("user_id")
	if len(targetID) > maxIDParamLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id parameter"})
		return
	}
	if !isAdmin && (all || (targetID != "" && targetID != callerID)) {
		c.JSON(http.StatusForbidden, gin.H{"error": msgForbidden})
		return
	}

	// The owner filter comes from the caller's own identity, never from the
	// client, unless an admin asks for another scope.
	filter := models.APIKeyFilter{UserID: callerID}
	switch {
	case targetID != "":
		filter.UserID = targetID
	case all:
		filter.UserID = ""
	}

	page, pageSize, offset := listPagination(c)
	keys, total, err := h.keys.ListPaged(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		status, message := mapError(err, "API key")
		c.JSON(status, gin.H{"error": message})
		return
	}
	c.JSON(http.StatusOK, APIKeyListResponse{Data: keys, Total: total, Page: page, PageSize: pageSize})
}

// DeleteAPIKey godoc
// @Summary      Delete an API key
// @Description  Deletes (revokes) a key at once: it stops authenticating with the next request and the WebSocket connections opened with it close with code 1008. A user can delete their own keys; an admin can delete any key. A key of another user, for a caller who is not an admin, is 404 like a key that does not exist. A request authenticated with an API key is refused (403).
// @Tags         api-keys
// @Produce      json
// @Param        id   path  string  true  "API key ID"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/api-keys/{id} [delete]
func (h *APIKeyHandler) DeleteAPIKey(c *gin.Context) {
	callerID, ok := sessionOnly(c)
	if !ok {
		return
	}
	keyID := c.Param("id")
	notFound := func() {
		status, message := mapError(dberrors.NewDatabaseError("delete", dberrors.ErrNotFound), "API key")
		c.JSON(status, gin.H{"error": message})
	}
	if keyID == "" || len(keyID) > maxIDParamLen {
		notFound()
		return
	}

	// Detached: a client that leaves must not leave the key valid, and the
	// sockets must still be closed.
	ctx, cancel := detachedCtx(c)
	defer cancel()

	// The owner is the caller. Only an admin acts on the owner of the key:
	// the repository query matches (owner, key), so another user's key is
	// "not found" and never deleted.
	ownerID := callerID
	if middleware.GetRoleFromContext(c) == models.RoleAdmin {
		found, err := h.keys.FindByIDs(ctx, []string{keyID})
		if err != nil {
			status, message := mapError(err, "API key")
			c.JSON(status, gin.H{"error": message})
			return
		}
		if len(found) == 0 {
			notFound()
			return
		}
		ownerID = found[0].UserID
	}

	if err := h.keys.Delete(ctx, ownerID, keyID); err != nil {
		status, message := mapError(err, "API key")
		c.JSON(status, gin.H{"error": message})
		return
	}
	slog.InfoContext(ctx, "API key deleted", "key_id", keyID, "user_id", ownerID, "deleted_by", callerID)
	if ownerID != callerID {
		middleware.AddAuditDetail(c, "owner_id", ownerID)
	}

	if h.wsRevoker != nil {
		if n := h.wsRevoker.DisconnectAPIKey(keyID); n > 0 {
			slog.InfoContext(ctx, "Closed WebSocket connections of a deleted API key", "key_id", keyID, "connections", n)
		}
	}
	c.Status(http.StatusNoContent)
}

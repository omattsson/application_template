package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/ctxutil"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Messages of the user administration.
const (
	msgAdminRequired    = "Admin role required"
	msgAdminSessionOnly = "User administration requires a login session"
	msgLastAdmin        = "The last enabled admin cannot be demoted, disabled or deleted"
	msgInvalidUserID    = "Invalid user ID"
	msgInvalidRole      = "Role must be one of: user, devops, admin"
	msgNoUserFields     = "Provide at least one of display_name, email, password"
	msgPasswordNotLocal = "A password can only be set for a local user"
	msgDisabledRequired = "disabled is required (true or false)"
	msgInvalidSearch    = "search must be at most 100 characters"
	msgDisplayNameEmpty = "Display name must not be empty"
)

// maxUserSearchLen is the longest search text of the user list, in bytes.
const maxUserSearchLen = 100

// UserHandler handles the user administration endpoints. Every endpoint is
// for admins and needs a login session (see requireAdminSession).
//
// A change that can leave a user with more access than they should have (a
// role change, a disable, a delete, a password reset) ends the access of the
// user at once: see revokeUserAccess.
type UserHandler struct {
	users     models.UserRepository
	refresh   models.RefreshTokenRepository
	keys      models.APIKeyRepository
	sessions  sessionstore.SessionStore
	wsRevoker websocket.ClientRevoker
	cfg       *config.AuthConfig
	logger    *slog.Logger // nil uses slog.Default() (tests set it)
	// blockRetryDelays are the waits between the attempts to write a user
	// block: len+1 attempts in all. Tests shorten them.
	blockRetryDelays []time.Duration
}

// defaultBlockRetryDelays are the waits between the attempts to write a user
// block (at most 3 retries, 1 s of waiting in all, inside the WriteTimeout
// budget of the step).
var defaultBlockRetryDelays = []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond}

// NewUserHandler creates a UserHandler. refresh and keys may be nil (no
// refresh tokens or keys to revoke); cfg gives the lifetime of the user
// block (a nil cfg uses a safe default).
func NewUserHandler(users models.UserRepository, refresh models.RefreshTokenRepository, keys models.APIKeyRepository, cfg *config.AuthConfig) *UserHandler {
	return &UserHandler{users: users, refresh: refresh, keys: keys, cfg: cfg, blockRetryDelays: defaultBlockRetryDelays}
}

// SetSessionStore sets the session store that receives the user blocks. nil:
// no block is written.
func (h *UserHandler) SetSessionStore(store sessionstore.SessionStore) { h.sessions = store }

// SetWebSocketRevoker sets the hub that closes the WebSocket connections of a
// user whose access was revoked. nil: sockets close at token expiry only.
func (h *UserHandler) SetWebSocketRevoker(r websocket.ClientRevoker) { h.wsRevoker = r }

func (h *UserHandler) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}

// requireAdminSession is the admin gate of the user and audit log endpoints.
// It answers and returns false unless the request is
//   - authenticated (401),
//   - authenticated with a login session, a JWT (403): an API key of an
//     admin cannot administer users, so a stolen key cannot create an admin,
//   - from a caller whose token says admin (403), and
//   - from a caller who is an enabled admin in the database now (403; 401 for
//     a caller that no longer exists). The role in a token is stale until the
//     token expires, so a demoted or disabled admin is refused here.
//
// The gate runs inside each handler: it does not rely on route wiring. It
// returns the caller ID.
func requireAdminSession(c *gin.Context, users models.UserRepository) (string, bool) {
	callerID := middleware.GetUserIDFromContext(c)
	if callerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgAuthRequired})
		return "", false
	}
	// An allowlist: only a JWT session passes.
	if middleware.GetAuthMethodFromContext(c) != middleware.AuthMethodJWT {
		c.JSON(http.StatusForbidden, gin.H{"error": msgAdminSessionOnly})
		return "", false
	}
	if middleware.GetRoleFromContext(c) != models.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": msgAdminRequired})
		return "", false
	}
	caller, err := users.FindByID(c.Request.Context(), callerID)
	if err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": msgAuthRequired})
			return "", false
		}
		status, message := mapError(err, "User")
		if status >= http.StatusInternalServerError {
			slog.ErrorContext(c.Request.Context(), "Failed to look up the admin caller", "user_id", callerID, "error", err)
		}
		c.JSON(status, gin.H{"error": message})
		return "", false
	}
	if caller.Disabled || caller.Role != models.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": msgAdminRequired})
		return "", false
	}
	return callerID, true
}

// validUserID reports whether id has the form of a user ID: a UUID in its
// canonical 36-character form.
func validUserID(id string) bool {
	return len(id) == maxIDParamLen && uuid.Validate(id) == nil
}

// beginTarget runs the checks that every endpoint on one user starts with:
// the admin gate, then the ID of the path (400 for a malformed ID). It
// returns the caller ID and the ID of the path.
func (h *UserHandler) beginTarget(c *gin.Context) (callerID, id string, ok bool) {
	callerID, ok = requireAdminSession(c, h.users)
	if !ok {
		return "", "", false
	}
	id = c.Param("id")
	if !validUserID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidUserID})
		return "", "", false
	}
	return callerID, id, true
}

// findTarget reads the user of the path. It answers 404 (or 499/500) and
// returns false on an error. From here on the handler uses the stored
// (canonical) ID: MySQL compares IDs without case, so the path ID can differ
// from it in case only.
func (h *UserHandler) findTarget(ctx context.Context, c *gin.Context, id string) (*models.User, bool) {
	user, err := h.users.FindByID(ctx, id)
	if err != nil {
		h.respondError(c, err, "Failed to look up a user")
		return nil, false
	}
	return user, true
}

// respondError answers a repository error with mapError and logs a 500.
func (h *UserHandler) respondError(c *gin.Context, err error, what string) {
	status, message := mapError(err, "User")
	if status >= http.StatusInternalServerError {
		h.log().Error(what, "error", err, "request_id", c.GetString(middleware.RequestIDKey))
	}
	c.JSON(status, gin.H{"error": message})
}

// respondGuardedChangeError answers an error of a guarded admin change
// (UpdateRole, SetDisabled, DeleteGuarded). The two guard errors come back
// wrapped and the generic mapping would answer 500, so they are checked first.
func (h *UserHandler) respondGuardedChangeError(c *gin.Context, err error, what string) {
	switch {
	case errors.Is(err, models.ErrCallerNotAdmin):
		// The caller lost the admin role or was disabled after the check
		// above (for example by a concurrent change).
		c.JSON(http.StatusForbidden, gin.H{"error": msgAdminRequired})
	case errors.Is(err, models.ErrLastAdmin):
		c.JSON(http.StatusConflict, gin.H{"error": msgLastAdmin})
	default:
		h.respondError(c, err, what)
	}
}

// blockUser writes the user block with a few bounded retries: the block is
// the step that ends the access tokens that are already out, and an admin
// change that committed cannot always be repeated (a delete cannot). Only an
// error that can be transient is retried (not a cancelled context and not an
// invalid key); the waits are blockRetryDelays and end with ctx.
func (h *UserHandler) blockUser(ctx context.Context, userID string) error {
	return blockUserWithRetry(ctx, h.log(), h.sessions, h.cfg, userID, h.blockRetryDelays)
}

// blockUserWithRetry is the retrying user block that the admin changes and the
// OIDC role sync share (see UserHandler.blockUser): blockUserNow, retried after
// each of delays while the error can be transient.
func blockUserWithRetry(ctx context.Context, log *slog.Logger, store sessionstore.SessionStore, cfg *config.AuthConfig, userID string, delays []time.Duration) error {
	for attempt := 0; ; attempt++ {
		err := blockUserNow(ctx, store, userID, userBlockUntil(cfg))
		if err == nil || attempt >= len(delays) ||
			errors.Is(err, context.Canceled) || errors.Is(err, sessionstore.ErrInvalidKey) {
			return err
		}
		log.Warn("Retrying the user block", "user_id", userID, "attempt", attempt+1, "error", err)
		timer := time.NewTimer(delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

// blockUserStep runs blockUser with a WriteTimeout budget of its own on a
// context that the client cannot cancel. Without a session store it does
// nothing.
func (h *UserHandler) blockUserStep(ctx context.Context, userID string) error {
	if h.sessions == nil {
		return nil
	}
	stepCtx, cancel := ctxutil.Detach(ctx, ctxutil.WriteTimeout)
	defer cancel()
	return h.blockUser(stepCtx, userID)
}

// revokeOptions selects the optional behaviour of revokeUserAccess.
type revokeOptions struct {
	// deleteKeys deletes all API keys of the user (the user is deleted).
	deleteKeys bool
	// blockBestEffort logs a failed block but does not report it: the caller
	// wrote the block before its change (a delete) and this one only covers
	// the tokens issued since.
	blockBestEffort bool
}

// revokeUserAccess ends the access of a user after an admin change that
// committed. Steps, all of them run even when one fails:
//  1. Revoke all refresh tokens (no new access token from a session).
//  2. Block the user in the session store: every access token issued up to
//     now gets 401 (retried a few times, see blockUser). It runs after the
//     revoke: a refresh that signs a token after the block must also pass its
//     session re-check after the block, and that re-check fails once the
//     revoke has run. blockUserNow answers after the block millisecond, so a
//     login that follows the answer is not caught by the block. (Enabling a
//     user later does not need a new block and does not remove this one: a
//     login after the enable has a later iat_ms, tokens from before stay
//     dead.)
//  3. With opts.deleteKeys (the user is deleted) delete all API keys of the
//     user. A disabled user's keys stay: key authentication refuses a
//     disabled owner, and enabling the user restores them. A key follows the
//     current role of its owner, so a role change needs no key step.
//  4. Close the user's WebSocket connections on this replica (the others
//     close theirs at the next revalidation).
//
// The refresh endpoint reads the user (role, disabled) from the database, so
// a failed step 1 does not bring back the old role: a disabled or deleted
// user is refused, and a new token carries the new role.
//
// It is idempotent: the endpoints run it again for a repeated request (an
// unchanged role, a repeated disable), so a retry after a failed step
// finishes the revocation.
//
// It returns the joined errors of the steps that failed. Each step has a
// WriteTimeout budget of its own on a context that the client cannot cancel.
func (h *UserHandler) revokeUserAccess(ctx context.Context, userID string, opts revokeOptions) error {
	var errs []error
	step := func(what string, fn func(context.Context) error) error {
		stepCtx, cancel := ctxutil.Detach(ctx, ctxutil.WriteTimeout)
		defer cancel()
		err := fn(stepCtx)
		if err != nil {
			h.log().Error("Failed to "+what, "user_id", userID, "error", err)
		}
		return err
	}
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	if h.refresh != nil {
		collect(step("revoke the refresh tokens of a user", func(c context.Context) error {
			return h.refresh.RevokeAllForUser(c, userID)
		}))
	}
	if h.sessions != nil {
		err := step("block the user", func(c context.Context) error { return h.blockUser(c, userID) })
		if !opts.blockBestEffort {
			collect(err)
		}
	}
	if opts.deleteKeys && h.keys != nil {
		collect(step("delete the API keys of a user", func(c context.Context) error {
			_, err := h.keys.DeleteAllForUser(c, userID)
			return err
		}))
	}
	if h.wsRevoker != nil {
		if n := h.wsRevoker.DisconnectUser(userID); n > 0 {
			h.log().Info("Closed WebSocket connections of a user whose access was revoked", "user_id", userID, "connections", n)
		}
	}
	return errors.Join(errs...)
}

// respondRevocationFailed answers 500 after the change committed but a
// revocation step failed (already logged). The change is recorded in the
// audit log although the status is an error.
func respondRevocationFailed(c *gin.Context) {
	middleware.AddAuditDetail(c, "revocation_failed", true)
	middleware.AuditCommitted(c)
	c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
}

// UserListResponse is one page of users.
type UserListResponse struct {
	Data     []models.User `json:"data"`
	Total    int64         `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"pageSize"`
}

// ListUsers godoc
// @Summary      List users
// @Description  Returns one page of users ordered by username, never with a password hash. Admin only, with a login session (an API key is refused with 403); the caller must still be an enabled admin in the database. Filter by role (exact) or search (username, display name or email contains the text).
// @Tags         users
// @Produce      json
// @Security     BearerAuth
// @Param        page      query  int     false  "Page, from 1"
// @Param        pageSize  query  int     false  "Page size (default 25, at most 100)"
// @Param        role      query  string  false  "Role" Enums(admin, devops, user)
// @Param        search    query  string  false  "Text in the username, display name or email (at most 100 characters)"
// @Success      200  {object}  UserListResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/users [get]
func (h *UserHandler) ListUsers(c *gin.Context) {
	if _, ok := requireAdminSession(c, h.users); !ok {
		return
	}
	filter := models.UserFilter{Role: c.Query("role"), Search: strings.TrimSpace(c.Query("search"))}
	if filter.Role != "" && !models.IsValidRole(filter.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRole})
		return
	}
	if len(filter.Search) > maxUserSearchLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidSearch})
		return
	}

	page, pageSize, offset := listPagination(c)
	users, total, err := h.users.ListPaged(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		h.respondError(c, err, "Failed to list users")
		return
	}
	noStore(c)
	// models.User.PasswordHash is tagged json:"-" and the list does not read it.
	c.JSON(http.StatusOK, UserListResponse{Data: users, Total: total, Page: page, PageSize: pageSize})
}

// GetUser godoc
// @Summary      Get a user
// @Description  Returns one user, never with a password hash. Admin only, with a login session. A malformed ID is 400, an unknown one 404.
// @Tags         users
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      string  true  "User ID (UUID)"
// @Success      200  {object}  models.User
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/users/{id} [get]
func (h *UserHandler) GetUser(c *gin.Context) {
	_, id, ok := h.beginTarget(c)
	if !ok {
		return
	}
	user, err := h.users.FindByID(c.Request.Context(), id)
	if err != nil {
		h.respondError(c, err, "Failed to look up a user")
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, user)
}

// CreateUserRequest is the request body of POST /users. The user is always a
// local user with a password; any other field (auth_provider, disabled,
// service_account, id, ...) is refused. SSO users are created by their first
// login.
type CreateUserRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	// Role is required: "user", "devops" or "admin".
	Role string `json:"role" enums:"user,devops,admin"`
}

// CreateUser godoc
// @Summary      Create a user
// @Description  Creates a local user with the given role. Admin only, with a login session. The username is 3 to 100 characters (ASCII letters, digits and . _ - @, starting with a letter or digit), the password at least 8 characters and at most 72 bytes; the role is required. A display name that is empty defaults to the username. Unknown request fields are refused. A username that exists is 409. The response never holds the password or its hash.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        request  body      CreateUserRequest  true  "New user"
// @Success      201  {object}  models.User
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      413  {object}  map[string]string "Request body too large"
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/users [post]
func (h *UserHandler) CreateUser(c *gin.Context) {
	if _, ok := requireAdminSession(c, h.users); !ok {
		return
	}
	var req CreateUserRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}
	if !models.IsValidRole(req.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRole})
		return
	}
	if msg := validateRegistration(&RegisterRequest{
		Username: req.Username, Password: req.Password, DisplayName: req.DisplayName, Email: req.Email,
	}); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	// A client that already left gets 499 before the bcrypt work.
	if err := c.Request.Context().Err(); err != nil {
		if !respondCancelled(c, err) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		}
		return
	}
	hash, err := hashPassword(c.Request.Context(), req.Password)
	if err != nil {
		if respondCancelled(c, err) {
			return
		}
		h.log().Error("Failed to hash the password of a new user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = req.Username
	}
	user := &models.User{
		Username:     req.Username,
		PasswordHash: hash,
		DisplayName:  displayName,
		Email:        req.Email,
		Role:         req.Role,
		AuthProvider: models.AuthProviderLocal,
	}
	// Detached: a client that leaves after the hash must not cancel the write
	// halfway or skip the audit entry.
	ctx, cancel := detachedCtx(c)
	defer cancel()
	if err := h.users.Create(ctx, user); err != nil {
		h.respondError(c, err, "Failed to create a user")
		return
	}
	h.log().Info("User created by an admin", "user_id", user.ID, "role", user.Role, "request_id", c.GetString(middleware.RequestIDKey))
	middleware.SetAuditEntityID(c, user.ID)
	noStore(c)
	c.JSON(http.StatusCreated, user)
}

// UpdateUserRequest is the request body of PUT /users/{id}: the fields to
// change. A field that is not sent keeps its value; at least one is required.
// The role and the disabled flag have endpoints of their own (they are guarded
// and revoke access); the username cannot be changed. Any other field is
// refused.
type UpdateUserRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Email       *string `json:"email,omitempty"`
	// Password is only for a local user (400 for an SSO user).
	Password *string `json:"password,omitempty"`
}

// validateUserUpdate checks an update and returns the message of the first
// problem, or "".
func validateUserUpdate(req *UpdateUserRequest) string {
	if req.DisplayName == nil && req.Email == nil && req.Password == nil {
		return msgNoUserFields
	}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" {
			return msgDisplayNameEmpty
		}
		if utf8.RuneCountInString(name) > maxDisplayNameLen {
			return msgDisplayNameTooLong
		}
		req.DisplayName = &name
	}
	if req.Email != nil && *req.Email != "" && (utf8.RuneCountInString(*req.Email) > maxEmailLen || !models.IsValidEmail(*req.Email)) {
		return msgInvalidEmail
	}
	if req.Password != nil {
		if err := validatePassword(*req.Password); err != nil {
			return err.Error()
		}
	}
	return ""
}

// UpdateUser godoc
// @Summary      Update a user
// @Description  Changes the display name, the email and/or the password of a user. Admin only, with a login session. Only these fields are accepted; any other field (role, disabled, username, ...) is refused with 400, because role and disabled have endpoints of their own that enforce the last-admin rule and end the user's sessions. A password can be set only for a local user (400 otherwise); setting it ends every session of the user (access tokens issued before are rejected, refresh tokens are revoked, WebSockets are closed) and keeps the API keys. The display name and email of an SSO user are overwritten by the identity provider at the next login. The response never holds a password or hash.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id       path      string             true  "User ID (UUID)"
// @Param        request  body      UpdateUserRequest  true  "Fields to change"
// @Success      200  {object}  models.User
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      413  {object}  map[string]string "Request body too large"
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/users/{id} [put]
func (h *UserHandler) UpdateUser(c *gin.Context) {
	_, id, ok := h.beginTarget(c)
	if !ok {
		return
	}
	var req UpdateUserRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}
	if msg := validateUserUpdate(&req); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	// The target is read first: a missing user is 404 before any bcrypt work.
	reqCtx := c.Request.Context()
	target, ok := h.findTarget(reqCtx, c, id)
	if !ok {
		return
	}
	hash := ""
	if req.Password != nil {
		if authProviderOf(target) != models.AuthProviderLocal {
			c.JSON(http.StatusBadRequest, gin.H{"error": msgPasswordNotLocal})
			return
		}
		var err error
		if hash, err = hashPassword(reqCtx, *req.Password); err != nil {
			if respondCancelled(c, err) {
				return
			}
			h.log().Error("Failed to hash the password of a user", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
			return
		}
	}
	if err := reqCtx.Err(); err != nil {
		if !respondCancelled(c, err) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		}
		return
	}

	// Detached from here on: a client that leaves must not stop the change
	// halfway or skip its revocation.
	ctx, cancel := detachedCtx(c)
	defer cancel()
	// The profile and the password are separate writes. After the first one
	// committed, a later failure still leaves a change behind: the entry is
	// recorded although the answer is an error (AuditCommitted).
	if req.DisplayName != nil || req.Email != nil {
		if err := h.users.UpdateProfile(ctx, target.ID, models.UserProfileUpdate{DisplayName: req.DisplayName, Email: req.Email}); err != nil {
			h.respondError(c, err, "Failed to update a user")
			return
		}
		middleware.SetAuditEntityID(c, target.ID)
		middleware.AuditCommitted(c)
	}
	if req.Password != nil {
		// Write only the password hash: a full-row save would overwrite a
		// concurrent role or disabled change.
		if err := h.users.UpdatePassword(ctx, target.ID, hash); err != nil {
			h.respondError(c, err, "Failed to set the password of a user")
			return
		}
		middleware.SetAuditEntityID(c, target.ID)
		middleware.AuditCommitted(c)
		middleware.AddAuditDetail(c, "password_changed", true)
	}
	middleware.SetAuditEntityID(c, target.ID) // also for a request that changed nothing

	if req.Password != nil {
		if err := h.revokeUserAccess(ctx, target.ID, revokeOptions{}); err != nil {
			respondRevocationFailed(c)
			return
		}
	}
	updated, err := h.users.FindByID(ctx, target.ID)
	if err != nil {
		h.respondError(c, err, "Failed to read an updated user")
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, updated)
}

// ChangeRoleRequest is the request body of a role change. Any other field is
// refused.
type ChangeRoleRequest struct {
	// Role is the new role: "user", "devops" or "admin".
	Role string `json:"role" enums:"user,devops,admin" example:"devops"`
}

// ChangeRoleResponse is the response of a role change.
type ChangeRoleResponse struct {
	ID      string `json:"id"`
	OldRole string `json:"old_role"`
	NewRole string `json:"new_role"`
	Message string `json:"message"`
	// Changed is false when the user already had the role (a no-op: nothing
	// is revoked and no audit entry is written).
	Changed bool `json:"changed"`
	// IDPManaged is true for an SSO user: the identity provider sets the role
	// at each login (OIDC_ADMIN_ROLES and OIDC_DEVOPS_ROLES), so this change
	// can be overridden at the next login.
	IDPManaged bool `json:"idp_managed"`
}

// ChangeUserRole godoc
// @Summary      Change the role of a user
// @Description  Sets the role of a user. Admin only, with a login session; the caller must still be an enabled admin in the database (403). The last enabled admin cannot be demoted (409); an admin may demote themselves when another enabled admin exists. An unchanged role changes nothing (changed=false, no audit entry) but ends the sessions of the user again, so repeating a request whose answer was 500 finishes the revocation. A change ends every session of the user: access tokens issued before it are rejected (user block), refresh tokens are revoked and the WebSockets are closed, so the next request needs a new login that carries the new role; the user's API keys stay and use the new role at once. The role of an SSO user is set by the identity provider at each login (OIDC_ADMIN_ROLES, OIDC_DEVOPS_ROLES) and can override this change: the response has idp_managed=true. The user block is retried a few times; when the change committed but a revocation step still failed, the answer is 500 and the change stands: repeat the same request to finish the revocation. Unknown request fields are refused.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id       path      string             true  "User ID (UUID)"
// @Param        request  body      ChangeRoleRequest  true  "New role"
// @Success      200  {object}  ChangeRoleResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      413  {object}  map[string]string "Request body too large"
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/users/{id}/role [put]
func (h *UserHandler) ChangeUserRole(c *gin.Context) {
	callerID, id, ok := h.beginTarget(c)
	if !ok {
		return
	}
	var req ChangeRoleRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}
	if !models.IsValidRole(req.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRole})
		return
	}

	// Detached: a client cancel must not commit the change and then skip the
	// revocation or the audit entry.
	ctx, cancel := detachedCtx(c)
	defer cancel()
	target, ok := h.findTarget(ctx, c, id)
	if !ok {
		return
	}
	oldRole, err := h.users.UpdateRole(ctx, callerID, target.ID, req.Role)
	if err != nil {
		h.respondGuardedChangeError(c, err, "Failed to change the role of a user")
		return
	}
	middleware.SetAuditEntityID(c, target.ID)

	changed := oldRole != req.Role
	resp := ChangeRoleResponse{
		ID: target.ID, OldRole: oldRole, NewRole: req.Role, Changed: changed,
		IDPManaged: authProviderOf(target) != models.AuthProviderLocal,
	}
	if !changed {
		// A no-op is not a change: no audit entry. The revocation runs again
		// anyway (it is idempotent), so a retry after a failed revocation of
		// the same change finishes it, as a repeated disable does.
		middleware.SkipAudit(c)
		if err := h.revokeUserAccess(ctx, target.ID, revokeOptions{}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
			return
		}
		resp.Message = "Role unchanged; the sessions of the user were ended again"
		c.JSON(http.StatusOK, resp)
		return
	}

	middleware.AddAuditDetail(c, "old_role", oldRole)
	middleware.AddAuditDetail(c, "new_role", req.Role)
	h.log().Info("User role changed", "user_id", target.ID, "old_role", oldRole, "new_role", req.Role, "changed_by", callerID, "request_id", c.GetString(middleware.RequestIDKey))
	// Tokens carry the role claim: end the sessions so the next request needs
	// a new login with the new role.
	if err := h.revokeUserAccess(ctx, target.ID, revokeOptions{}); err != nil {
		respondRevocationFailed(c)
		return
	}
	resp.Message = "Role changed"
	if resp.IDPManaged {
		resp.Message = "Role changed; the identity provider may override it at the next login"
	}
	c.JSON(http.StatusOK, resp)
}

// SetDisabledRequest is the request body of PUT /users/{id}/disabled. Any
// other field is refused.
type SetDisabledRequest struct {
	// Disabled is required: true disables the user, false enables them.
	Disabled *bool `json:"disabled"`
}

// SetDisabledResponse is the response of PUT /users/{id}/disabled.
type SetDisabledResponse struct {
	ID       string `json:"id"`
	Message  string `json:"message"`
	Disabled bool   `json:"disabled"`
	// Changed is false when the user already had that state.
	Changed bool `json:"changed"`
}

// SetUserDisabled godoc
// @Summary      Disable or enable a user
// @Description  Disables (disabled=true) or enables (disabled=false) a user. Admin only, with a login session; the caller must still be an enabled admin in the database (403). The last enabled admin cannot be disabled (409); an admin may disable themselves when another enabled admin exists. Disabling ends every session of the user: access tokens issued before it are rejected (user block), refresh tokens are revoked and the WebSockets are closed. The user's API keys are kept but stop authenticating while the owner is disabled. Enabling writes no new block: tokens issued before the disable stay rejected, and a login after the enable works. A request that does not change the state is a no-op (changed=false, no audit entry; a repeated disable still ends the sessions). Unknown request fields are refused.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id       path      string              true  "User ID (UUID)"
// @Param        request  body      SetDisabledRequest  true  "New state"
// @Success      200  {object}  SetDisabledResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      413  {object}  map[string]string "Request body too large"
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/users/{id}/disabled [put]
func (h *UserHandler) SetUserDisabled(c *gin.Context) {
	callerID, id, ok := h.beginTarget(c)
	if !ok {
		return
	}
	var req SetDisabledRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgDisabledRequired})
		return
	}
	disabled := *req.Disabled

	ctx, cancel := detachedCtx(c) // see ChangeUserRole
	defer cancel()
	target, ok := h.findTarget(ctx, c, id)
	if !ok {
		return
	}
	if err := h.users.SetDisabled(ctx, callerID, target.ID, disabled); err != nil {
		h.respondGuardedChangeError(c, err, "Failed to change the disabled state of a user")
		return
	}
	middleware.SetAuditEntityID(c, target.ID)

	changed := target.Disabled != disabled
	if !changed {
		middleware.SkipAudit(c)
	} else if !disabled {
		middleware.SetAuditAction(c, middleware.AuditActionEnable)
	}
	h.log().Info("User disabled state set", "user_id", target.ID, "disabled", disabled, "changed", changed, "changed_by", callerID, "request_id", c.GetString(middleware.RequestIDKey))

	// A repeated disable still ends the sessions: it makes a retry after a
	// failed revocation work. Enabling needs no revocation.
	if disabled {
		if err := h.revokeUserAccess(ctx, target.ID, revokeOptions{}); err != nil {
			respondRevocationFailed(c)
			return
		}
	}
	message := "User enabled"
	if disabled {
		message = "User disabled"
	}
	c.JSON(http.StatusOK, SetDisabledResponse{ID: target.ID, Disabled: disabled, Changed: changed, Message: message})
}

// DeleteUser godoc
// @Summary      Delete a user
// @Description  Permanently deletes a user. Admin only, with a login session; the caller must still be an enabled admin in the database (403). The last enabled admin cannot be deleted (409, answered before anything is blocked when the admin deletes themselves); an admin may delete themselves when another enabled admin exists. The user's access ends: access tokens issued before are rejected (user block), refresh tokens are revoked, all their API keys are deleted and the WebSockets are closed. The user block is written before the delete (a failure answers 500 and deletes nothing, so the request can be repeated; a delete that the transaction guard refuses after a race, 403 or 409, has still blocked the user's tokens issued so far) and again after it (best effort); when a later step fails the answer is 500 and the user is gone.
// @Tags         users
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      string  true  "User ID (UUID)"
// @Success      204
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/users/{id} [delete]
func (h *UserHandler) DeleteUser(c *gin.Context) {
	callerID, id, ok := h.beginTarget(c)
	if !ok {
		return
	}
	ctx, cancel := detachedCtx(c) // see ChangeUserRole
	defer cancel()
	target, ok := h.findTarget(ctx, c, id)
	if !ok {
		return
	}
	// An enabled admin who deletes themselves while no other enabled admin
	// exists is refused (409) before anything is blocked: a refused delete
	// must not sign the caller out. This read is only a pre-check; the guard
	// in the delete transaction stays the authority (a change that races
	// this check is still refused there).
	if target.ID == callerID && target.Role == models.RoleAdmin && !target.Disabled {
		n, err := h.users.CountEnabledAdmins(ctx)
		if err != nil {
			h.respondError(c, err, "Failed to count the enabled admins before a delete")
			return
		}
		if n <= 1 {
			c.JSON(http.StatusConflict, gin.H{"error": msgLastAdmin})
			return
		}
	}
	// Block first: once the row is gone no request can find the user again to
	// block them, so a block that fails after the delete could not be
	// repeated. When this block fails nothing is deleted and the request can
	// be retried. (A delete that the guard refuses after a race, 403 or 409,
	// has blocked the user's tokens issued so far: they must sign in again.)
	if err := h.blockUserStep(ctx, target.ID); err != nil {
		h.log().Error("Failed to block the user before a delete", "user_id", target.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}
	if err := h.users.DeleteGuarded(ctx, callerID, target.ID); err != nil {
		h.respondGuardedChangeError(c, err, "Failed to delete a user")
		return
	}
	middleware.SetAuditEntityID(c, target.ID)
	h.log().Info("User deleted", "user_id", target.ID, "deleted_by", callerID, "request_id", c.GetString(middleware.RequestIDKey))

	// The block after the delete covers the tokens issued since the first
	// one; the first one already ended every older token, so it is best
	// effort.
	if err := h.revokeUserAccess(ctx, target.ID, revokeOptions{deleteKeys: true, blockBestEffort: true}); err != nil {
		respondRevocationFailed(c)
		return
	}
	c.Status(http.StatusNoContent)
}

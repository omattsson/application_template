package handlers

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// passwordHashCost is the bcrypt cost of every stored password hash and of
// the dummy hash of the login.
const passwordHashCost = bcrypt.DefaultCost

// Password and profile bounds.
const (
	// minPasswordLength is the shortest accepted password in characters
	// (the rule of the reset-password endpoint).
	minPasswordLength = 8
	// maxPasswordBytes is the longest accepted password in bytes. bcrypt
	// uses only the first 72 bytes: a longer password would match every
	// password with the same prefix.
	maxPasswordBytes = 72
	// minUsernameLen is the shortest username of a self-registration.
	minUsernameLen = 3
	// maxDisplayNameLen and maxEmailLen are the column sizes (characters).
	maxDisplayNameLen = 255
	maxEmailLen       = 255
)

// Client messages of the auth endpoints.
const (
	msgInvalidCredentials   = "Invalid username or password"
	msgTooManyLoginAttempts = "Too many failed login attempts. Try again later."
	msgAuthRequired         = "Authentication required"
	msgAccountDisabled      = "Account disabled"
	msgNotFound             = "Not found"
	msgInvalidUsername      = "Username must be 3 to 100 characters: letters, digits and . _ - @, starting with a letter or digit"
	msgPasswordTooShort     = "Password must be at least 8 characters"
	msgPasswordTooLong      = "Password must be at most 72 bytes"
	msgInvalidEmail         = "Invalid email address"
	msgDisplayNameTooLong   = "Display name must be at most 255 characters"
	// msgSessionRequired is the 403 of an endpoint that only a login session
	// may use (the text of middleware.RequireSessionAuth).
	msgSessionRequired = "This action requires a login session"
	// msgJSONContentTypeRequired is the 415 of a session login without a JSON
	// Content-Type.
	msgJSONContentTypeRequired = "Content-Type must be application/json"
)

// usernamePattern is the format of a self-registered username. The length is
// checked separately (minUsernameLen, models.MaxUsernameLen).
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// requestIDKey is the gin context key that middleware.RequestID sets.
const requestIDKey = "request_id"

// bcryptSem limits concurrent bcrypt operations to the number of CPU cores.
// bcrypt is intentionally CPU-expensive; without a limit, many concurrent
// logins would starve every other request.
var bcryptSem = make(chan struct{}, runtime.NumCPU())

// dummyPasswordHash returns the hash that the login compares against when no
// real hash can be checked (unknown user, no or malformed hash, SSO user,
// over-long password), so that the answer takes about the same time as a
// wrong password.
var dummyPasswordHash = sync.OnceValue(func() []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte(rand.Text()), passwordHashCost)
	if err != nil {
		panic("auth: dummy password hash: " + err.Error())
	}
	return hash
})

// AuthHandler handles the local login, self-registration and the current
// user endpoint.
type AuthHandler struct {
	userRepo         models.UserRepository
	refreshTokenRepo models.RefreshTokenRepository
	sessionStore     sessionstore.SessionStore
	wsRevoker        websocket.ClientRevoker
	cfg              *config.AuthConfig
	oidcCfg          *config.OIDCConfig
	throttle         *loginThrottle
	logger           *slog.Logger                                          // nil uses slog.Default() (tests set it)
	compareHash      func(hashedPassword, password []byte) error           // bcrypt.CompareHashAndPassword; tests wrap it
	signToken        func(middleware.GenerateTokenOptions) (string, error) // middleware.GenerateTokenWithOpts; tests replace it
}

// NewAuthHandler creates an AuthHandler. oidcCfg may be nil (OIDC off).
func NewAuthHandler(userRepo models.UserRepository, cfg *config.AuthConfig, oidcCfg *config.OIDCConfig) *AuthHandler {
	h := &AuthHandler{
		userRepo:    userRepo,
		cfg:         cfg,
		oidcCfg:     oidcCfg,
		throttle:    newLoginThrottle(loginFailureThreshold, loginFailureWindow, loginThrottleMaxEntries),
		compareHash: bcrypt.CompareHashAndPassword,
		signToken:   middleware.GenerateTokenWithOpts,
	}
	// Create the dummy hash now: the first login of an unknown user must not
	// take longer.
	dummyPasswordHash()
	return h
}

// SetRefreshTokenRepo sets the refresh token repository (refresh sessions).
// nil: logins issue only an access token.
func (h *AuthHandler) SetRefreshTokenRepo(repo models.RefreshTokenRepository) {
	h.refreshTokenRepo = repo
}

// SetSessionStore sets the session store (token blocklist). nil: no blocks
// are written.
func (h *AuthHandler) SetSessionStore(store sessionstore.SessionStore) {
	h.sessionStore = store
}

// SetWebSocketRevoker sets the hub that closes open WebSocket connections on
// logout. nil: sockets close at access-token expiry only.
func (h *AuthHandler) SetWebSocketRevoker(r websocket.ClientRevoker) {
	h.wsRevoker = r
}

// comparePassword runs one bcrypt compare. bcryptSem limits the concurrent
// compares.
func (h *AuthHandler) comparePassword(hash []byte, password string) error {
	bcryptSem <- struct{}{}
	defer func() { <-bcryptSem }()
	return h.compareHash(hash, []byte(password))
}

// hashPassword returns the bcrypt hash of password at passwordHashCost.
func hashPassword(password string) (string, error) {
	bcryptSem <- struct{}{}
	defer func() { <-bcryptSem }()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), passwordHashCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// isNotFoundError reports whether err is a missing record.
func isNotFoundError(err error) bool {
	return errors.Is(err, dberrors.ErrNotFound)
}

// noStore marks a response as not cacheable (it carries a token or account
// data).
func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
}

// LoginRequest represents the login request body.
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginResponse represents the login response.
type LoginResponse struct {
	Token string      `json:"token"`
	User  models.User `json:"user"`
}

// RegisterRequest represents the register request body. Any other field
// (role, disabled, auth_provider, service_account, ...) is ignored.
type RegisterRequest struct {
	Username    string `json:"username" binding:"required"`
	Password    string `json:"password" binding:"required"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// Login godoc
// @Summary     User login
// @Description Authenticate with username and password; returns a JWT access token and the user. With refresh sessions on, the login starts a session: the access token lives ACCESS_TOKEN_EXPIRATION and carries the session ID (sid), and the response sets the httpOnly refresh_token cookie (Path /api/v1/auth, SameSite=Strict, Secure unless SECURE_COOKIES=false) for POST /api/v1/auth/refresh; without them the token lives JWT_EXPIRATION. With refresh sessions on, the request needs the X-Requested-With header (any value) and must not be cross-site (403), and its Content-Type must be application/json (415); both are checked first (login CSRF protection). Every credential failure (unknown user, wrong password, disabled account, SSO-only account) answers the same 401 "Invalid username or password". After 5 failed attempts for one username within 15 minutes, every login for that username answers 429 with Retry-After for 15 minutes.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       X-Requested-With header   string       false "Required with refresh sessions on (any value, for example XMLHttpRequest)"
// @Param       credentials      body     LoginRequest true  "Login credentials"
// @Success     200              {object} LoginResponse
// @Failure     400              {object} map[string]string
// @Failure     401              {object} map[string]string
// @Failure     403              {object} map[string]string "Cross-site request refused"
// @Failure     413              {object} map[string]string
// @Failure     415              {object} map[string]string "Content-Type must be application/json"
// @Failure     429              {object} map[string]string
// @Failure     500              {object} map[string]string
// @Router      /api/v1/auth/login [post]
func (h *AuthHandler) Login(c *gin.Context) {
	// A login that sets the session cookie must come from our own pages, or
	// a cross-site form could plant the attacker's session in the victim's
	// browser (login CSRF): a SameSite=Strict cookie can still be set by the
	// answer to a cross-site top-level POST. Checked before the body, the
	// throttle and any lookup, so a forged request costs nothing and uses no
	// throttle budget of the name.
	if h.refreshTokenRepo != nil && !requireSessionLoginRequest(c) {
		return
	}

	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
		return
	}

	// Reserve a slot under the name key before any work, so a parallel burst
	// cannot pass the throttle before its failures are counted. The name key
	// never depends on whether the account exists, so this 429 (the only one
	// Login gives) is the same for known and unknown names. The deferred
	// settle releases every reserved key exactly once on every exit (also a
	// cancel or a panic); the outcome stays neutral unless set below.
	attempt, retryAfter := h.throttle.begin(nameThrottleKey(req.Username))
	if attempt == nil {
		h.log().Warn("Login throttled",
			"username", truncateForLog(req.Username, maxLoggedUsernameLen),
			"client_ip", c.ClientIP(),
			"request_id", c.GetString(requestIDKey))
		c.Header("Retry-After", retryAfterSeconds(retryAfter))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": msgTooManyLoginAttempts})
		return
	}
	defer attempt.settle()

	// fail logs the failure and writes the response. A credential failure
	// (401) counts towards the throttle; a server-side failure does not.
	fail := func(f loginFailure) {
		f.username = req.Username
		f.clientIP = c.ClientIP()
		f.requestID = c.GetString(requestIDKey)
		h.logLoginFailure(f)
		if f.httpCode == http.StatusUnauthorized {
			attempt.outcome = outcomeFailure
			c.JSON(http.StatusUnauthorized, gin.H{"error": msgInvalidCredentials})
			return
		}
		c.JSON(f.httpCode, gin.H{"error": msgInternalServerError})
	}
	// failDummy spends one compare on the dummy hash (the time of a real
	// compare), then fails with a credential failure. The password is cut to
	// maxPasswordBytes so an over-long one costs the same.
	failDummy := func(reason, userID string) {
		_ = h.comparePassword(dummyPasswordHash(), truncateAtRune(req.Password, maxPasswordBytes))
		fail(loginFailure{reason: reason, userID: userID, httpCode: http.StatusUnauthorized})
	}

	ctx := c.Request.Context()

	// A username over the column size cannot exist: skip the query.
	if len(req.Username) > models.MaxUsernameLen {
		failDummy(loginReasonUserNotFound, "")
		return
	}
	user, err := h.userRepo.FindByUsername(ctx, req.Username)
	if err != nil {
		if respondCancelled(c, err) {
			return
		}
		if !isNotFoundError(err) {
			// A server-side failure; a 401 would claim the password is wrong.
			fail(loginFailure{reason: loginReasonLookupError, httpCode: http.StatusInternalServerError, err: err})
			return
		}
		failDummy(loginReasonUserNotFound, "")
		return
	}

	// The collation may have matched a variant of the stored username (case,
	// accents, width): count the attempt under the account key too, which
	// caps the password checks of the account over every spelling. When that
	// key is locked or busy, the password is not checked, for every spelling
	// including the stored name: the answer is the generic 401, which counts
	// under the name key like the 401 of an unknown name, so neither the
	// status nor the Retry-After shows that the account exists.
	if _, ok := attempt.add(accountThrottleKey(user.ID)); !ok {
		failDummy(loginReasonAccountThrottled, user.ID)
		return
	}

	if reason := unusableForPassword(user, req.Password); reason != "" {
		failDummy(reason, user.ID)
		return
	}
	if err := h.comparePassword([]byte(user.PasswordHash), req.Password); err != nil {
		reason := loginReasonPasswordMismatch
		if !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			reason = loginReasonHashInvalid
		}
		fail(loginFailure{reason: reason, userID: user.ID, httpCode: http.StatusUnauthorized})
		return
	}

	// Re-read the user after the password check (bcrypt takes time): a
	// disable, role change or password reset that committed during the check
	// applies to this login.
	fresh, err := h.userRepo.FindByID(ctx, user.ID)
	if err != nil {
		if respondCancelled(c, err) {
			return
		}
		if isNotFoundError(err) {
			fail(loginFailure{reason: loginReasonRereadNotFound, userID: user.ID, httpCode: http.StatusUnauthorized})
		} else {
			fail(loginFailure{reason: loginReasonRereadError, userID: user.ID, httpCode: http.StatusInternalServerError, err: err})
		}
		return
	}
	if fresh.PasswordHash != user.PasswordHash {
		fail(loginFailure{reason: loginReasonHashChanged, userID: user.ID, httpCode: http.StatusUnauthorized})
		return
	}
	// Account state answers the same 401 as a wrong password.
	if fresh.Disabled {
		fail(loginFailure{reason: loginReasonDisabled, userID: user.ID, httpCode: http.StatusUnauthorized})
		return
	}
	if h.oidcCfg != nil && h.oidcCfg.Enabled && !h.oidcCfg.LocalAuth && !fresh.ServiceAccount {
		fail(loginFailure{reason: loginReasonLocalLoginNotAllowed, userID: user.ID, httpCode: http.StatusUnauthorized})
		return
	}

	// With a refresh repository the login starts a session; the access token
	// carries its ID ("sid").
	var sess refreshSession
	if h.refreshTokenRepo != nil {
		sess = newRefreshSession()
	}
	token, err := h.signToken(middleware.GenerateTokenOptions{
		UserID:       fresh.ID,
		Username:     fresh.Username,
		DisplayName:  fresh.DisplayName,
		Role:         fresh.Role,
		Secret:       h.cfg.JWTSecret,
		Expiration:   h.loginTokenExpiration(),
		AuthProvider: authProviderOf(fresh),
		Email:        fresh.Email,
		SessionID:    sess.familyID,
	})
	if err != nil {
		fail(loginFailure{reason: loginReasonTokenError, userID: user.ID, httpCode: http.StatusInternalServerError, err: err})
		return
	}

	// Close the disable race: re-read the user after signing and before the
	// token or a refresh token leaves the server.
	if err := recheckUserActive(ctx, h.userRepo, fresh.ID); err != nil {
		if respondCancelled(c, err) {
			return
		}
		if errors.Is(err, errAccountDisabled) || errors.Is(err, errUserGone) {
			fail(loginFailure{reason: loginReasonRecheckFailed, userID: user.ID, httpCode: http.StatusUnauthorized})
		} else {
			fail(loginFailure{reason: loginReasonRecheckError, userID: user.ID, httpCode: http.StatusInternalServerError, err: err})
		}
		return
	}

	if h.refreshTokenRepo != nil && !h.startSession(c, fresh.ID, sess, fail) {
		return
	}

	attempt.outcome = outcomeSuccess
	h.log().Info("Login succeeded",
		"user_id", fresh.ID,
		"username", truncateForLog(fresh.Username, maxLoggedUsernameLen),
		"client_ip", c.ClientIP(),
		"request_id", c.GetString(requestIDKey))
	noStore(c)
	c.JSON(http.StatusOK, LoginResponse{Token: token, User: *fresh})
}

// requireSessionLoginRequest refuses a login that would set the session
// cookie unless the request carries the same-site marker (the rule of
// refresh and logout, 403) and a Content-Type of application/json (415). A
// cross-site form can send neither: it cannot set the header, and its
// enctype is never application/json (text/plain can still carry a JSON
// body, which ShouldBindJSON would decode). It writes the answer and returns
// false on refusal.
func requireSessionLoginRequest(c *gin.Context) bool {
	if !requireSameSiteRequest(c) {
		return false
	}
	if c.ContentType() != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": msgJSONContentTypeRequired})
		return false
	}
	return true
}

// loginTokenExpiration is the lifetime of a login token. A login with a
// refresh repository starts a session and its token lives
// ACCESS_TOKEN_EXPIRATION (the client refreshes it). Without one the token
// is "issued outside a refresh session" and lives JWT_EXPIRATION.
func (h *AuthHandler) loginTokenExpiration() time.Duration {
	if h.refreshTokenRepo != nil {
		return h.cfg.AccessTokenExpiration
	}
	return h.cfg.JWTExpiration
}

// startSession stores the first refresh token of the login session sess and
// sets the cookie. It fails closed: when the token cannot be stored, or a
// logout-all or revoke ended the session before the answer, it revokes the
// family, answers through fail and returns false. The writes are detached: a
// client that leaves cannot skip the session cap or the abandon revoke.
func (h *AuthHandler) startSession(c *gin.Context, userID string, sess refreshSession, fail func(loginFailure)) bool {
	wctx, cancel := detachedCtx(c)
	defer cancel()
	raw, expires, err := h.createRefreshToken(wctx, c, h.refreshTokenRepo, userID, sess, "")
	if err != nil {
		fail(loginFailure{reason: loginReasonSessionError, userID: userID, httpCode: http.StatusInternalServerError, err: err})
		return false
	}
	if err := recheckSessionActive(wctx, h.refreshTokenRepo, sess.familyID); err != nil {
		if rerr := h.refreshTokenRepo.RevokeFamily(wctx, sess.familyID); rerr != nil {
			h.log().Error("Failed to revoke an abandoned login session", "user_id", userID, "session_id", sess.familyID, "error", rerr)
		}
		if errors.Is(err, errSessionRevoked) {
			fail(loginFailure{reason: loginReasonSessionRevoked, userID: userID, httpCode: http.StatusUnauthorized})
		} else {
			fail(loginFailure{reason: loginReasonSessionError, userID: userID, httpCode: http.StatusInternalServerError, err: err})
		}
		return false
	}
	h.setRefreshCookie(c, raw, expires)
	return true
}

// unusableForPassword returns the failure reason when the user cannot sign in
// with this password without a real compare, or "" when the stored hash must
// be compared.
func unusableForPassword(user *models.User, password string) string {
	switch {
	case authProviderOf(user) != models.AuthProviderLocal:
		return loginReasonNotLocal
	case user.PasswordHash == "":
		return loginReasonNoPassword
	case len(password) > maxPasswordBytes:
		return loginReasonPasswordTooLong
	}
	if _, err := bcrypt.Cost([]byte(user.PasswordHash)); err != nil {
		return loginReasonHashInvalid
	}
	return ""
}

// authProviderOf returns the auth provider of the user record; "local" when
// the record has none.
func authProviderOf(user *models.User) string {
	if user.AuthProvider == "" {
		return models.AuthProviderLocal
	}
	return user.AuthProvider
}

// validatePassword checks the password policy: at least minPasswordLength
// characters and at most maxPasswordBytes bytes.
func validatePassword(password string) error {
	if utf8.RuneCountInString(password) < minPasswordLength {
		return errors.New(msgPasswordTooShort)
	}
	if len(password) > maxPasswordBytes {
		return errors.New(msgPasswordTooLong)
	}
	return nil
}

// validateRegistration checks a registration request and returns the client
// message of the first problem, or "".
func validateRegistration(req *RegisterRequest) string {
	if len(req.Username) < minUsernameLen || len(req.Username) > models.MaxUsernameLen || !usernamePattern.MatchString(req.Username) {
		return msgInvalidUsername
	}
	if err := validatePassword(req.Password); err != nil {
		return err.Error()
	}
	if req.Email != "" && (utf8.RuneCountInString(req.Email) > maxEmailLen || !models.IsValidEmail(req.Email)) {
		return msgInvalidEmail
	}
	if utf8.RuneCountInString(req.DisplayName) > maxDisplayNameLen {
		return msgDisplayNameTooLong
	}
	return ""
}

// Register godoc
// @Summary     Register a new user
// @Description Self-registration of a local account with the role user. Only available when SELF_REGISTRATION is true; otherwise 404. Role, disabled, provider and service account fields in the body are ignored. The username is 3 to 100 characters (letters, digits and . _ - @, starting with a letter or digit); the password is at least 8 characters and at most 72 bytes.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       user body     RegisterRequest true "User registration data"
// @Success     201  {object} models.User
// @Failure     400  {object} map[string]string
// @Failure     404  {object} map[string]string
// @Failure     409  {object} map[string]string
// @Failure     413  {object} map[string]string
// @Failure     500  {object} map[string]string
// @Router      /api/v1/auth/register [post]
func (h *AuthHandler) Register(c *gin.Context) {
	if !h.cfg.SelfRegistration {
		c.JSON(http.StatusNotFound, gin.H{"error": msgNotFound})
		return
	}

	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
		return
	}
	if msg := validateRegistration(&req); msg != "" {
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

	hash, err := hashPassword(req.Password)
	if err != nil {
		h.log().Error("Failed to hash the password of a registration", "error", err)
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
		Role:         models.RoleUser,
		AuthProvider: models.AuthProviderLocal,
	}

	// Detached: a client that leaves after the hash must not cancel the write
	// halfway.
	wctx, cancel := detachedCtx(c)
	defer cancel()
	if err := h.userRepo.Create(wctx, user); err != nil {
		status, message := mapError(err, "User")
		if status >= http.StatusInternalServerError {
			h.log().Error("Failed to create a registered user", "error", err)
		}
		c.JSON(status, gin.H{"error": message})
		return
	}

	h.log().Info("User registered", "user_id", user.ID, "username", truncateForLog(user.Username, maxLoggedUsernameLen))
	c.JSON(http.StatusCreated, user)
}

// GetCurrentUser godoc
// @Summary     Get current user
// @Description Returns the authenticated user. A disabled account answers 401, a deleted one 404.
// @Tags        auth
// @Produce     json
// @Success     200 {object} models.User
// @Failure     401 {object} map[string]string
// @Failure     404 {object} map[string]string
// @Failure     500 {object} map[string]string
// @Security    BearerAuth
// @Security    ApiKeyAuth
// @Router      /api/v1/auth/me [get]
func (h *AuthHandler) GetCurrentUser(c *gin.Context) {
	noStore(c)
	userID := middleware.GetUserIDFromContext(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgAuthRequired})
		return
	}

	user, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if err != nil {
		status, message := mapError(err, "User")
		if status >= http.StatusInternalServerError {
			h.log().Error("Failed to look up the current user", "user_id", userID, "error", err)
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	if user.Disabled {
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgAccountDisabled})
		return
	}

	c.JSON(http.StatusOK, user)
}

// EnsureAdminUser creates the initial admin user (ADMIN_USERNAME, local,
// role admin, service account so it can still sign in when OIDC is the only
// interactive login) when ADMIN_PASSWORD is set and no user with that name
// exists. Without ADMIN_PASSWORD it does nothing.
//
// It is idempotent and safe when several replicas run it at once: an existing
// user is never changed (not its password, not its role), and a duplicate key
// from a concurrent create counts as success. A user with that name that is
// not an enabled admin is left alone with a warning. The returned error never
// contains the password.
func (h *AuthHandler) EnsureAdminUser(ctx context.Context) error {
	if h.cfg.AdminPassword == "" {
		h.log().Debug("ADMIN_PASSWORD is not set; no admin user is created")
		return nil
	}
	username := h.cfg.AdminUsername
	if strings.TrimSpace(username) == "" {
		return errors.New("ADMIN_USERNAME must not be empty when ADMIN_PASSWORD is set")
	}
	if utf8.RuneCountInString(h.cfg.AdminPassword) < minPasswordLength {
		return fmt.Errorf("ADMIN_PASSWORD must be at least %d characters", minPasswordLength)
	}
	if len(h.cfg.AdminPassword) > maxPasswordBytes {
		return fmt.Errorf("ADMIN_PASSWORD must be at most %d bytes", maxPasswordBytes)
	}

	existing, err := h.userRepo.FindByUsername(ctx, username)
	if err == nil {
		h.checkExistingAdmin(existing)
		return nil
	}
	// Only a missing user is created: a failed lookup (for example a
	// timeout) must not try a second admin row.
	if !isNotFoundError(err) {
		return fmt.Errorf("look up the admin user: %w", err)
	}

	hash, err := hashPassword(h.cfg.AdminPassword)
	if err != nil {
		// bcrypt errors do not contain the password.
		return fmt.Errorf("hash the admin password: %w", err)
	}
	admin := &models.User{
		Username:       username,
		PasswordHash:   hash,
		DisplayName:    "Administrator",
		Role:           models.RoleAdmin,
		AuthProvider:   models.AuthProviderLocal,
		ServiceAccount: true,
	}
	if err := h.userRepo.Create(ctx, admin); err != nil {
		if dberrors.IsDuplicateKey(err) || errors.Is(err, dberrors.ErrDuplicateKey) {
			h.log().Info("Admin user already created by another replica", "username", username)
			return nil
		}
		return fmt.Errorf("create the admin user: %w", err)
	}
	h.log().Info("Admin user created", "username", username, "user_id", admin.ID)
	return nil
}

// checkExistingAdmin logs a warning when the user named ADMIN_USERNAME is not
// an enabled local admin. It changes nothing.
func (h *AuthHandler) checkExistingAdmin(u *models.User) {
	switch {
	case u.Role != models.RoleAdmin:
		h.log().Warn("ADMIN_USERNAME belongs to a user that is not an admin; it is left unchanged",
			"username", u.Username, "user_id", u.ID, "role", u.Role)
	case u.Disabled:
		h.log().Warn("ADMIN_USERNAME belongs to a disabled admin; it is left unchanged",
			"username", u.Username, "user_id", u.ID)
	default:
		h.log().Debug("Admin user exists", "username", u.Username, "user_id", u.ID)
	}
}

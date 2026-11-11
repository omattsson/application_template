package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// IDs of the fixture users (valid UUIDs).
const (
	idAdminA  = "00000000-0000-4000-8000-000000000001"
	idAdminB  = "00000000-0000-4000-8000-000000000002"
	idDevops  = "00000000-0000-4000-8000-000000000003"
	idUserU   = "00000000-0000-4000-8000-000000000004"
	idOIDC    = "00000000-0000-4000-8000-000000000005"
	idUserV   = "00000000-0000-4000-8000-000000000006"
	idMissing = "00000000-0000-4000-8000-0000000000ff"
)

// adminEnv is the user administration over real repositories on SQLite, an
// in-memory session store, the login and CombinedAuth, the audit middleware
// and the audit log endpoint.
type adminEnv struct {
	db       *gorm.DB
	users    *database.GORMUserRepository
	keys     *database.GORMAPIKeyRepository
	tokens   *database.GORMRefreshTokenRepository
	audit    *database.GORMAuditLogRepository
	store    sessionstore.SessionStore
	revoker  *recordingRevoker
	handler  *UserHandler
	auditH   *AuditLogHandler
	auth     *AuthHandler
	recorder *middleware.AuditRecorder
	router   *gin.Engine
	logs     *syncBuffer
	cfg      *config.AuthConfig
}

type adminOpt struct {
	users  func(models.UserRepository) models.UserRepository
	tokens func(models.RefreshTokenRepository) models.RefreshTokenRepository
	keys   func(models.APIKeyRepository) models.APIKeyRepository
	store  func(sessionstore.SessionStore) sessionstore.SessionStore
	audit  func(models.AuditLogRepository) models.AuditLogRepository
}

func openAdminDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.RefreshToken{}, &models.AuditLog{}, &models.Item{}))
	return db
}

func newAdminEnv(t *testing.T, opts ...adminOpt) *adminEnv {
	t.Helper()
	var opt adminOpt
	if len(opts) > 0 {
		opt = opts[0]
	}
	db := openAdminDB(t)
	e := &adminEnv{
		db:      db,
		users:   database.NewGORMUserRepository(db),
		keys:    database.NewGORMAPIKeyRepository(db),
		tokens:  database.NewGORMRefreshTokenRepository(db),
		audit:   database.NewGORMAuditLogRepository(db),
		revoker: &recordingRevoker{},
		cfg:     testAuthConfig(),
	}
	e.cfg.RefreshTokenExpiration = 168 * time.Hour
	e.cfg.RefreshReuseGrace = 30 * time.Second
	e.cfg.SecureCookies = true
	mem := sessionstore.NewMemory()
	t.Cleanup(mem.Stop)
	e.store = mem
	if opt.store != nil {
		e.store = opt.store(e.store)
	}

	ctx := context.Background()
	extID := "sub-1"
	fixtures := []models.User{
		{ID: idAdminA, Username: "admin-a", Role: models.RoleAdmin},
		{ID: idAdminB, Username: "admin-b", Role: models.RoleAdmin},
		{ID: idDevops, Username: "devops", Role: models.RoleDevOps},
		{ID: idUserU, Username: "user-u", Role: models.RoleUser},
		{ID: idOIDC, Username: "oidc-o", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC, ExternalID: &extID},
		{ID: idUserV, Username: "user-v", Role: models.RoleUser},
	}
	for i := range fixtures {
		u := fixtures[i]
		if u.AuthProvider != models.AuthProviderOIDC {
			u.PasswordHash = testPasswordHash()
		}
		u.DisplayName = "Display " + u.Username
		u.Email = u.Username + "@example.com"
		require.NoError(t, e.users.Create(ctx, &u))
	}

	var users models.UserRepository = e.users
	if opt.users != nil {
		users = opt.users(users)
	}
	var tokens models.RefreshTokenRepository = e.tokens
	if opt.tokens != nil {
		tokens = opt.tokens(tokens)
	}
	var keys models.APIKeyRepository = e.keys
	if opt.keys != nil {
		keys = opt.keys(keys)
	}
	var audit models.AuditLogRepository = e.audit
	if opt.audit != nil {
		audit = opt.audit(audit)
	}

	e.auth = NewAuthHandler(users, e.cfg, nil)
	e.auth.SetRefreshTokenRepo(tokens)
	e.auth.SetSessionStore(e.store)
	e.auth.SetWebSocketRevoker(e.revoker)

	e.handler = NewUserHandler(users, tokens, keys, e.cfg)
	e.handler.SetSessionStore(e.store)
	e.handler.SetWebSocketRevoker(e.revoker)
	e.logs = &syncBuffer{}
	e.handler.logger = slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.auditH = NewAuditLogHandler(audit, users)
	e.recorder = middleware.NewAuditRecorder(audit)

	r := gin.New()
	r.Use(middleware.RequestID())
	r.POST("/api/v1/auth/login", e.auth.Login)
	r.GET("/open/users", e.handler.ListUsers) // no auth middleware at all
	authed := r.Group("/api/v1", middleware.CombinedAuth(middleware.APIKeyAuthDeps{
		JWTSecret: testJWTSecret, APIKeyRepo: e.keys, UserRepo: e.users, SessionStore: e.store, FailClosedOnStoreError: true,
	}), e.recorder.Middleware())
	authed.GET("/users", e.handler.ListUsers)
	authed.POST("/users", e.handler.CreateUser)
	authed.GET("/users/:id", e.handler.GetUser)
	authed.PUT("/users/:id", e.handler.UpdateUser)
	authed.DELETE("/users/:id", e.handler.DeleteUser)
	authed.PUT("/users/:id/role", e.handler.ChangeUserRole)
	authed.PUT("/users/:id/disabled", e.handler.SetUserDisabled)
	authed.GET("/audit-logs", e.auditH.ListAuditLogs)
	items := NewHandler(models.NewRepository(db))
	authed.POST("/items", items.CreateItem)
	authed.PUT("/items/:id", items.UpdateItem)
	authed.DELETE("/items/:id", items.DeleteItem)
	apiKeys := NewAPIKeyHandler(keys, users, APIKeyOptions{})
	authed.POST("/api-keys", apiKeys.CreateAPIKey)
	authed.DELETE("/api-keys/:id", apiKeys.DeleteAPIKey)
	authed.GET("/whoami", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": middleware.GetUserIDFromContext(c), "role": middleware.GetRoleFromContext(c), "method": middleware.GetAuthMethodFromContext(c)})
	})
	e.router = r
	return e
}

// tokenFor returns a bearer token with the given claims (not read from the
// database: a test can hold a token of a user that was demoted later).
func tokenWith(t *testing.T, id, username, role string) string {
	t.Helper()
	tok, err := middleware.GenerateTokenWithOpts(middleware.GenerateTokenOptions{
		UserID: id, Username: username, Role: role, Secret: testJWTSecret, Expiration: time.Hour,
	})
	require.NoError(t, err)
	return tok
}

// bearer returns the headers of a token for the fixture user as stored now.
func (e *adminEnv) bearer(t *testing.T, id string) map[string]string {
	t.Helper()
	u, err := e.users.FindByID(context.Background(), id)
	require.NoError(t, err)
	return map[string]string{"Authorization": "Bearer " + tokenWith(t, u.ID, u.Username, u.Role)}
}

func bearerOf(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func (e *adminEnv) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// asAdmin sends a request as admin A.
func (e *adminEnv) asAdmin(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(method, path, body, e.bearer(t, idAdminA))
}

// login signs the fixture user in and returns the access token and the raw
// refresh token.
func (e *adminEnv) login(t *testing.T, username string) (string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody(t, username, testPassword)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp LoginResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	ck := refreshCookieOf(t, w)
	require.NotNil(t, ck)
	return resp.Token, ck.Value
}

// newKey creates an API key of the user and returns its raw value and ID.
func (e *adminEnv) newKey(t *testing.T, userID string) (raw, id string) {
	t.Helper()
	rawKey, prefix, hash, err := models.GenerateAPIKey()
	require.NoError(t, err)
	exp := time.Now().Add(time.Hour)
	k := &models.APIKey{UserID: userID, Name: "ci", KeyHash: hash, Prefix: prefix, ExpiresAt: &exp}
	require.NoError(t, e.keys.Create(context.Background(), k))
	return models.FormatAPIKey(rawKey), k.ID
}

func keyHeader(raw string) map[string]string { return map[string]string{"X-API-Key": raw} }

func (e *adminEnv) user(t *testing.T, id string) *models.User {
	t.Helper()
	u, err := e.users.FindByID(context.Background(), id)
	require.NoError(t, err)
	return u
}

func (e *adminEnv) userExists(id string) bool {
	_, err := e.users.FindByID(context.Background(), id)
	return err == nil
}

func (e *adminEnv) keyCount(t *testing.T, userID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, e.db.Model(&models.APIKey{}).Where("user_id = ?", userID).Count(&n).Error)
	return n
}

func (e *adminEnv) activeRefreshTokens(t *testing.T, userID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, e.db.Model(&models.RefreshToken{}).Where("user_id = ? AND revoked = ?", userID, false).Count(&n).Error)
	return n
}

// enabledAdmins counts the enabled admins.
func (e *adminEnv) enabledAdmins(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, e.db.Model(&models.User{}).Where("role = ? AND disabled = ?", models.RoleAdmin, false).Count(&n).Error)
	return n
}

// auditRows flushes the audit recorder and returns the rows, oldest first.
// The recorder cannot be used afterwards.
func (e *adminEnv) auditRows(t *testing.T) []models.AuditLog {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.recorder.Shutdown(ctx))
	var rows []models.AuditLog
	require.NoError(t, e.db.Order("timestamp ASC").Find(&rows).Error)
	return rows
}

// tokenStatus returns the status of GET /whoami with the headers.
func (e *adminEnv) tokenStatus(headers map[string]string) int {
	return e.do(http.MethodGet, "/api/v1/whoami", "", headers).Code
}

func decodeInto[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v), w.Body.String())
	return v
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeInto[map[string]string](t, w)
	return m["error"]
}

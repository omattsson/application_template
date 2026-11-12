package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/auth"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// authTestSecret is the JWT secret of the auth-on routers.
const authTestSecret = "routes-auth-test-secret-0123456789"

// testPassword is the password of every fixture user (at least 8 characters).
const testPassword = "correct-horse-battery"

// authEnv is a router built by Setup with AUTH_ENABLED=true over real
// repositories on SQLite and an in-memory session store.
type authEnv struct {
	router  *gin.Engine
	res     *Result
	db      *gorm.DB
	users   *database.GORMUserRepository
	keys    *database.GORMAPIKeyRepository
	refresh *database.GORMRefreshTokenRepository
	audit   *database.GORMAuditLogRepository
	store   sessionstore.SessionStore
	hub     *websocket.Hub
	cfg     *config.Config
}

// authOn returns a config with AUTH_ENABLED=true and the defaults that
// LoadConfig gives. SecureCookies is off so the httptest client keeps the
// refresh cookie over plain HTTP.
func authOn() *config.Config {
	cfg := testConfig(1000, 1000, nil)
	cfg.App.Title = "Auth Test"
	cfg.Auth = config.AuthConfig{
		Enabled:                true,
		JWTSecret:              authTestSecret,
		JWTExpiration:          time.Hour,
		AccessTokenExpiration:  15 * time.Minute,
		SessionIdleTimeout:     30 * time.Minute,
		SessionMaxLifetime:     12 * time.Hour,
		RefreshTokenExpiration: 168 * time.Hour,
		RefreshReuseGrace:      30 * time.Second,
		AdminUsername:          "admin",
	}
	return cfg
}

// openAuthDB opens an in-memory SQLite database with the tables of the auth
// module and the items table.
func openAuthDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.RefreshToken{},
		&models.AuditLog{}, &models.Item{}))
	return db
}

// envOption changes the dependencies before Setup runs.
type envOption func(*authEnv, *AuthDeps)

// newAuthEnv builds the router with Setup. cfg nil uses authOn().
func newAuthEnv(t *testing.T, cfg *config.Config, opts ...envOption) *authEnv {
	t.Helper()
	if cfg == nil {
		cfg = authOn()
	}
	db := openAuthDB(t)
	store := sessionstore.NewMemory()
	t.Cleanup(store.Stop)
	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Shutdown)

	env := &authEnv{
		db:      db,
		users:   database.NewGORMUserRepository(db),
		keys:    database.NewGORMAPIKeyRepository(db),
		refresh: database.NewGORMRefreshTokenRepository(db),
		audit:   database.NewGORMAuditLogRepository(db),
		store:   store,
		hub:     hub,
		cfg:     cfg,
	}
	deps := &AuthDeps{
		Users:         env.users,
		RefreshTokens: env.refresh,
		APIKeys:       env.keys,
		AuditLogs:     env.audit,
		SessionStore:  store,
	}
	for _, o := range opts {
		o(env, deps)
	}

	healthChecker := health.New()
	healthChecker.SetReady(true)
	router := gin.New()
	res, err := Setup(router, Deps{
		Config:        cfg,
		Repository:    models.NewRepository(db),
		HealthChecker: healthChecker,
		Hub:           hub,
		Auth:          deps,
	})
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = res.AuditRecorder.Shutdown(ctx)
	})
	env.router = router
	env.res = res
	return env
}

// createUser stores a local user with testPassword.
func (e *authEnv) createUser(t *testing.T, username, role string) *models.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	require.NoError(t, err)
	u := &models.User{Username: username, PasswordHash: string(hash), DisplayName: username, Role: role}
	require.NoError(t, e.users.Create(context.Background(), u))
	return u
}

// token signs an access token for u (no session).
func (e *authEnv) token(t *testing.T, u *models.User) string {
	t.Helper()
	tok, err := middleware.GenerateTokenWithOpts(middleware.GenerateTokenOptions{
		UserID: u.ID, Username: u.Username, Role: u.Role, Secret: authTestSecret, Expiration: 15 * time.Minute,
	})
	require.NoError(t, err)
	return tok
}

// apiKey creates an API key for u and returns the raw key.
func (e *authEnv) apiKey(t *testing.T, u *models.User) (raw string, id string) {
	t.Helper()
	rawKey, prefix, hash, err := models.GenerateAPIKey()
	require.NoError(t, err)
	exp := time.Now().Add(24 * time.Hour)
	k := &models.APIKey{UserID: u.ID, Name: "test", KeyHash: hash, Prefix: prefix, ExpiresAt: &exp}
	require.NoError(t, e.keys.Create(context.Background(), k))
	return models.FormatAPIKey(rawKey), k.ID
}

// login logs u in through the router and returns the access token and the
// refresh cookie.
func (e *authEnv) login(t *testing.T, username string) (string, *http.Cookie) {
	t.Helper()
	w := e.do(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, testPassword),
		map[string]string{"X-Requested-With": "XMLHttpRequest", "Content-Type": "application/json"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Token)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "refresh_token" {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "login sets the refresh cookie (plain name: SecureCookies is off)")
	require.Equal(t, "/", cookie.Path, "Path=/ so it is sent through a proxy that strips /api")
	return body.Token, cookie
}

// do sends a request from testPeer.
func (e *authEnv) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return doFrom(e.router, testPeer, method, path, body, headers)
}

func doFrom(router http.Handler, peer, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = peer
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

// auditEntries returns the audit entries written so far.
func (e *authEnv) auditEntries(t *testing.T) []models.AuditLog {
	t.Helper()
	var rows []models.AuditLog
	require.NoError(t, e.db.Order("timestamp").Find(&rows).Error)
	return rows
}

// errStore is the error of failingStore.
var errStore = errors.New("dial tcp: session store down")

// failingStore is a session store whose lookups fail (R20 tests). A nil
// *failingStore is a typed nil for the Setup refusal test.
type failingStore struct {
	sessionstore.SessionStore
	mu   sync.Mutex
	fail bool
}

func newFailingStore(t *testing.T) *failingStore {
	t.Helper()
	inner := sessionstore.NewMemory()
	t.Cleanup(inner.Stop)
	return &failingStore{SessionStore: inner}
}

func (s *failingStore) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *failingStore) failing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail
}

func (s *failingStore) IsTokenBlocked(ctx context.Context, jti string) (bool, error) {
	if s.failing() {
		return false, errStore
	}
	return s.SessionStore.IsTokenBlocked(ctx, jti)
}

func (s *failingStore) IsSessionBlocked(ctx context.Context, id string) (bool, error) {
	if s.failing() {
		return false, errStore
	}
	return s.SessionStore.IsSessionBlocked(ctx, id)
}

func (s *failingStore) IsUserBlocked(ctx context.Context, id string, at time.Time) (bool, error) {
	if s.failing() {
		return false, errStore
	}
	return s.SessionStore.IsUserBlocked(ctx, id, at)
}

// failingKeys is an API key repository whose prefix lookup fails.
type failingKeys struct {
	models.APIKeyRepository
}

func (failingKeys) FindByPrefix(context.Context, string) ([]*models.APIKey, error) {
	return nil, errors.New("dial tcp: database down")
}

// fakeOIDC is an identity provider client that never talks to a network.
type fakeOIDC struct{}

func (fakeOIDC) AuthCodeURL(_ context.Context, state, _, _ string) (string, error) {
	return "https://idp.example.com/authorize?state=" + state, nil
}

func (fakeOIDC) Exchange(context.Context, string, string, string) (*auth.Identity, error) {
	return nil, errors.New("not used")
}

package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// invalidSessionBody is the one answer of every refused refresh.
const invalidSessionBody = `{"error":"Invalid or expired session"}`

const refreshResponseSchema = `{
"type": "object",
"required": ["token"],
"additionalProperties": false,
"properties": {"token": {"type": "string", "minLength": 1}}
}`

const messageSchema = `{
"type": "object",
"required": ["message"],
"additionalProperties": false,
"properties": {"message": {"type": "string", "minLength": 1}}
}`

// sha256Hex is the stored form of a raw refresh token, computed
// independently of the handler.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// openRefreshDB returns a SQLite database with the refresh_tokens table on
// one connection: a query outside the rotation transaction would block, so
// the tests also catch a write that bypasses the transaction.
func openRefreshDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.RefreshToken{}))
	return db
}

// recordingRevoker records the WebSocket disconnects.
type recordingRevoker struct {
	users  []string
	tokens []string
	mu     sync.Mutex
}

func (r *recordingRevoker) DisconnectUser(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, id)
	return 1
}

func (r *recordingRevoker) DisconnectToken(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = append(r.tokens, id)
	return 1
}

func (r *recordingRevoker) DisconnectAPIKey(string) int { return 0 }

func (r *recordingRevoker) snapshot() (users, tokens []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.users...), append([]string(nil), r.tokens...)
}

// hookUserRepo honours the context and runs onFindByID first (a test cancels
// the request there).
type hookUserRepo struct {
	*mockUserRepo
	onFindByID func(ctx context.Context)
	mu         sync.Mutex
}

func (r *hookUserRepo) setHook(fn func(ctx context.Context)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onFindByID = fn
}

func (r *hookUserRepo) FindByID(ctx context.Context, id string) (*models.User, error) {
	r.mu.Lock()
	hook := r.onFindByID
	r.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.mockUserRepo.FindByID(ctx, id)
}

// faultyTokens wraps the real repository and fails chosen methods.
type faultyTokens struct {
	models.RefreshTokenRepository
	findErr        error
	createErr      error
	countFamilyErr error
	revokeErr      error
	listErr        error
	claimErr       error
	// zeroActive makes CountActiveInFamily report 0 (a revoke that raced).
	zeroActive bool
}

func (f *faultyTokens) ClaimReuseGrace(ctx context.Context, id string, since time.Time) (bool, error) {
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return f.RefreshTokenRepository.ClaimReuseGrace(ctx, id, since)
}

func (f *faultyTokens) FindByTokenHash(ctx context.Context, hash string) (*models.RefreshToken, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.RefreshTokenRepository.FindByTokenHash(ctx, hash)
}

func (f *faultyTokens) Create(ctx context.Context, rt *models.RefreshToken) error {
	if f.createErr != nil {
		return f.createErr
	}
	return f.RefreshTokenRepository.Create(ctx, rt)
}

func (f *faultyTokens) CountActiveInFamily(ctx context.Context, family string) (int64, error) {
	if f.countFamilyErr != nil {
		return 0, f.countFamilyErr
	}
	if f.zeroActive {
		return 0, nil
	}
	return f.RefreshTokenRepository.CountActiveInFamily(ctx, family)
}

func (f *faultyTokens) RevokeFamily(ctx context.Context, family string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return f.RefreshTokenRepository.RevokeFamily(ctx, family)
}

func (f *faultyTokens) RevokeFamilyForUser(ctx context.Context, family, userID string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return f.RefreshTokenRepository.RevokeFamilyForUser(ctx, family, userID)
}

func (f *faultyTokens) RevokeAllForUser(ctx context.Context, userID string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return f.RefreshTokenRepository.RevokeAllForUser(ctx, userID)
}

func (f *faultyTokens) RevokeAllForUserExceptFamily(ctx context.Context, userID, family string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return f.RefreshTokenRepository.RevokeAllForUserExceptFamily(ctx, userID, family)
}

func (f *faultyTokens) ListRecentFamiliesForUser(ctx context.Context, userID string, since time.Time) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.RefreshTokenRepository.ListRecentFamiliesForUser(ctx, userID, since)
}

// WithTx hands fn a wrapped transaction repository, so Create fails inside
// the rotation too.
func (f *faultyTokens) WithTx(ctx context.Context, fn func(models.RefreshTokenRepository) error) error {
	return f.RefreshTokenRepository.WithTx(ctx, func(tx models.RefreshTokenRepository) error {
		return fn(&faultyTokens{RefreshTokenRepository: tx, createErr: f.createErr})
	})
}

// faultyStore fails chosen session store calls.
type faultyStore struct {
	sessionstore.SessionStore
	sessionErr error // IsSessionBlocked
	userErr    error // IsUserBlocked
	blockErr   error // every Block* call
}

func (f *faultyStore) IsUserBlocked(ctx context.Context, userID string, issuedAt time.Time) (bool, error) {
	if f.userErr != nil {
		return false, f.userErr
	}
	return f.SessionStore.IsUserBlocked(ctx, userID, issuedAt)
}

func (f *faultyStore) IsSessionBlocked(ctx context.Context, id string) (bool, error) {
	if f.sessionErr != nil {
		return false, f.sessionErr
	}
	return f.SessionStore.IsSessionBlocked(ctx, id)
}

func (f *faultyStore) BlockToken(ctx context.Context, jti string, until time.Time) error {
	if f.blockErr != nil {
		return f.blockErr
	}
	return f.SessionStore.BlockToken(ctx, jti, until)
}

func (f *faultyStore) BlockSessions(ctx context.Context, ids []string, until time.Time) error {
	if f.blockErr != nil {
		return f.blockErr
	}
	return f.SessionStore.BlockSessions(ctx, ids, until)
}

func (f *faultyStore) BlockUser(ctx context.Context, userID string, until time.Time) error {
	if f.blockErr != nil {
		return f.blockErr
	}
	return f.SessionStore.BlockUser(ctx, userID, until)
}

// sessionHarness is an AuthHandler with a real refresh token repository
// (SQLite), an in-memory session store and the auth routes.
type sessionHarness struct {
	h       *AuthHandler
	users   *hookUserRepo
	tokens  *database.GORMRefreshTokenRepository
	store   sessionstore.SessionStore
	revoker *recordingRevoker
	router  *gin.Engine
	logs    *syncBuffer
	cfg     *config.AuthConfig
}

type harnessOpt func(*sessionHarness)

// withTokenRepo replaces the refresh token repository of the handler.
func withTokenRepo(wrap func(models.RefreshTokenRepository) models.RefreshTokenRepository) harnessOpt {
	return func(s *sessionHarness) { s.h.SetRefreshTokenRepo(wrap(s.tokens)) }
}

// withStore replaces the session store of the handler and the middleware.
func withStore(wrap func(sessionstore.SessionStore) sessionstore.SessionStore) harnessOpt {
	return func(s *sessionHarness) { s.store = wrap(s.store); s.h.SetSessionStore(s.store) }
}

func newSessionHarness(t *testing.T, mutate func(*config.AuthConfig), opts ...harnessOpt) *sessionHarness {
	t.Helper()
	cfg := testAuthConfig()
	cfg.RefreshTokenExpiration = 168 * time.Hour
	cfg.RefreshReuseGrace = 30 * time.Second
	cfg.SecureCookies = true
	if mutate != nil {
		mutate(cfg)
	}
	users := &hookUserRepo{mockUserRepo: newMockUserRepo(localUser("u1", "alice"), localUser("u2", "bob"))}
	h, logs, _ := newTestAuthHandler(users, cfg, nil)
	store := sessionstore.NewMemory()
	t.Cleanup(store.Stop)
	s := &sessionHarness{
		h: h, users: users, tokens: database.NewGORMRefreshTokenRepository(openRefreshDB(t)),
		store: store, revoker: &recordingRevoker{}, logs: logs, cfg: cfg,
	}
	h.SetRefreshTokenRepo(s.tokens)
	h.SetSessionStore(store)
	h.SetWebSocketRevoker(s.revoker)
	for _, o := range opts {
		o(s)
	}
	r := gin.New()
	r.POST("/api/v1/auth/login", h.Login)
	r.POST("/api/v1/auth/refresh", h.Refresh)
	r.POST("/api/v1/auth/logout", h.Logout)
	authed := r.Group("/api/v1/auth", middleware.AuthRequiredWithOptions(middleware.JWTAuthOptions{
		JWTSecret: testJWTSecret, SessionStore: s.store, FailClosedOnStoreError: true,
	}))
	authed.POST("/logout-all", h.LogoutAll)
	authed.GET("/me", h.GetCurrentUser)
	s.router = r
	return s
}

type reqOpt func(*http.Request)

func withCookie(raw string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: hostRefreshCookie, Value: raw}) }
}

func withBearer(token string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func withoutCSRF() reqOpt { return func(r *http.Request) { r.Header.Del("X-Requested-With") } }

func withContext(ctx context.Context) reqOpt {
	return func(r *http.Request) { *r = *r.WithContext(ctx) }
}

// post sends a POST with the CSRF header (unless withoutCSRF).
func (s *sessionHarness) post(t *testing.T, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func (s *sessionHarness) me(t *testing.T, token string) int {
	t.Helper()
	return bearerGet(t, s.router, "/api/v1/auth/me", token).Code
}

// login signs alice (or another user) in and returns the access token and
// the raw refresh token of the cookie.
func (s *sessionHarness) login(t *testing.T, username string) (string, string) {
	t.Helper()
	w := s.post(t, "/api/v1/auth/login", loginBody(t, username, testPassword))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp LoginResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	ck := refreshCookieOf(t, w)
	require.NotNil(t, ck, "the login sets the refresh cookie")
	return resp.Token, ck.Value
}

// refresh refreshes with raw and returns the answer, the new access token and
// the new raw refresh token ("" when the answer sets none).
func (s *sessionHarness) refresh(t *testing.T, raw string, opts ...reqOpt) (*httptest.ResponseRecorder, string, string) {
	t.Helper()
	w := s.post(t, "/api/v1/auth/refresh", "", append([]reqOpt{withCookie(raw)}, opts...)...)
	if w.Code != http.StatusOK {
		return w, "", ""
	}
	var resp RefreshResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	next := ""
	if ck := refreshCookieOf(t, w); ck != nil {
		next = ck.Value
	}
	return w, resp.Token, next
}

func (s *sessionHarness) row(t *testing.T, raw string) *models.RefreshToken {
	t.Helper()
	rt, err := s.tokens.FindByTokenHash(context.Background(), sha256Hex(raw))
	require.NoError(t, err)
	return rt
}

func (s *sessionHarness) rowCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, s.tokens.DB.Model(&models.RefreshToken{}).Count(&n).Error)
	return n
}

func (s *sessionHarness) setRow(t *testing.T, raw string, values map[string]any) {
	t.Helper()
	require.NoError(t, s.tokens.DB.Model(&models.RefreshToken{}).Where("token_hash = ?", sha256Hex(raw)).Updates(values).Error)
}

func (s *sessionHarness) familyActive(t *testing.T, family string) int64 {
	t.Helper()
	n, err := s.tokens.CountActiveInFamily(context.Background(), family)
	require.NoError(t, err)
	return n
}

func (s *sessionHarness) sessionBlocked(t *testing.T, family string) bool {
	t.Helper()
	blocked, err := s.store.IsSessionBlocked(context.Background(), family)
	require.NoError(t, err)
	return blocked
}

// hostRefreshCookie and hostBindingCookie are the names the harnesses (which
// run with SECURE_COOKIES=true) see.
const (
	hostRefreshCookie = "__Host-" + refreshTokenCookieName
	hostBindingCookie = "__Host-" + oidcBindingCookieName
)

// refreshCookieOf returns the refresh_token cookie that w sets, or nil.
func refreshCookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	var found *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == hostRefreshCookie {
			require.Nil(t, found, "one refresh cookie per answer")
			found = ck
		}
	}
	return found
}

// assertCookieCleared checks that w deletes the cookie with the attributes
// it was set with.
func assertCookieCleared(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	ck := refreshCookieOf(t, w)
	require.NotNil(t, ck, "the answer clears the cookie")
	assert.Empty(t, ck.Value)
	assert.Equal(t, -1, ck.MaxAge, "Max-Age=0 deletes the cookie")
	assertCookieAttributes(t, ck)
}

func assertCookieAttributes(t *testing.T, ck *http.Cookie) {
	t.Helper()
	assert.Equal(t, "/", ck.Path, "Path=/ so the __Host- prefix and a proxy that strips /api both work")
	assert.True(t, ck.HttpOnly)
	assert.True(t, ck.Secure)
	assert.Equal(t, http.SameSiteStrictMode, ck.SameSite)
	assert.Empty(t, ck.Domain, "host-only cookie")
}

func claimsOf(t *testing.T, token string) *middleware.Claims {
	t.Helper()
	claims, err := middleware.ValidateJWT(token, testJWTSecret)
	require.NoError(t, err)
	return claims
}

// A login with a refresh repository starts a session: an access token with
// the session ID and ACCESS_TOKEN_EXPIRATION, and an opaque refresh token in
// an httpOnly cookie that is stored only as its SHA-256 hash.
func TestLoginStartsARefreshSession(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	before := time.Now()
	w := s.post(t, "/api/v1/auth/login", loginBody(t, "alice", testPassword))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, loginResponseSchema, w.Body.Bytes()))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	var resp LoginResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	claims := claimsOf(t, resp.Token)
	require.NotEmpty(t, claims.SessionID, "the token names its session")
	assert.WithinDuration(t, before.Add(s.cfg.AccessTokenExpiration), claims.ExpiresAt.Time, 5*time.Second,
		"a session token lives ACCESS_TOKEN_EXPIRATION, not JWT_EXPIRATION")

	ck := refreshCookieOf(t, w)
	require.NotNil(t, ck)
	assertCookieAttributes(t, ck)
	assert.InDelta(t, s.cfg.SessionMaxLifetime.Seconds(), float64(ck.MaxAge), 5, "the cookie lives as long as the session")
	assert.NotContains(t, w.Body.String(), ck.Value, "the refresh token is only in the cookie")

	// Opaque: 256 random bits, base64url, not a JWT.
	assert.NotContains(t, ck.Value, ".", "not a JWT")
	raw, err := base64.RawURLEncoding.DecodeString(ck.Value)
	require.NoError(t, err)
	assert.Len(t, raw, 32)

	row := s.row(t, ck.Value)
	assert.Equal(t, claims.SessionID, row.FamilyID)
	assert.Equal(t, row.ID, row.FamilyID, "the first token of a session is its family")
	assert.Equal(t, "u1", row.UserID)
	assert.False(t, row.Revoked)
	var leaked int64
	require.NoError(t, s.tokens.DB.Model(&models.RefreshToken{}).Where("token_hash = ? OR user_agent = ?", ck.Value, ck.Value).Count(&leaked).Error)
	assert.Zero(t, leaked, "the raw token is never stored")
	assert.NotContains(t, s.logs.String(), ck.Value)
	assert.NotContains(t, s.logs.String(), resp.Token)

	assert.Equal(t, http.StatusOK, s.me(t, resp.Token))
}

// The session start is written to a DATETIME(3) column, and MySQL rounds a
// finer value to the millisecond: up to 0.5 ms later than the Go value. A
// session that started in the millisecond of a user block (logout-all) could
// then read as started after the block and escape the inclusive check on
// refresh. So a session starts on a whole millisecond, and the stored value
// is the Go value.
func TestSessionStartIsAWholeMillisecond(t *testing.T) {
	t.Parallel()
	for i := 0; i < 50; i++ {
		start := newRefreshSession().startedAt
		require.Equal(t, start.Truncate(time.Millisecond), start, "run %d: %s", i, start.Format(time.RFC3339Nano))
	}

	s := newSessionHarness(t, nil)
	for i := 0; i < 3; i++ {
		w := s.post(t, "/api/v1/auth/login", loginBody(t, "alice", testPassword))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		row := s.row(t, refreshCookieOf(t, w).Value)
		assert.Equal(t, row.SessionStartedAt.Truncate(time.Millisecond), row.SessionStartedAt,
			"login %d: the stored session start has no sub-millisecond part", i+1)
	}
}

// Two logins never share a refresh token or a session.
func TestLoginSessionsAreDistinct(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	a1, r1 := s.login(t, "alice")
	a2, r2 := s.login(t, "alice")
	assert.NotEqual(t, r1, r2)
	assert.NotEqual(t, claimsOf(t, a1).SessionID, claimsOf(t, a2).SessionID)
}

// A login keeps at most maxRefreshTokensPerUser active sessions: the oldest
// one ends, the new one works.
func TestLoginCapsTheActiveSessions(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, first := s.login(t, "alice")
	var last string
	for i := 0; i < maxRefreshTokensPerUser; i++ {
		_, last = s.login(t, "alice")
	}
	n, err := s.tokens.CountActiveForUser(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, int64(maxRefreshTokensPerUser), n)
	assert.True(t, s.row(t, first).Revoked, "the oldest session ended")
	assert.False(t, s.row(t, last).Revoked)
}

// The login fails closed when the session cannot be stored or was revoked
// during the login, and never hands out a cookie then.
func TestLoginSessionFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		wrap     func(models.RefreshTokenRepository) models.RefreshTokenRepository
		mutate   func(s *sessionHarness)
		name     string
		wantBody string
		wantCode int
	}{
		{name: "refresh token cannot be stored", wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`,
			wrap: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, createErr: errors.New("dial tcp: secret detail")}
			}},
		{name: "session revoked during the login", wantCode: http.StatusUnauthorized, wantBody: invalidCredentialsBody,
			wrap: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, zeroActive: true}
			}},
		{name: "session check fails", wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`,
			wrap: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, countFamilyErr: errors.New("dial tcp")}
			}},
		{name: "user disabled while the token was signed", wantCode: http.StatusUnauthorized, wantBody: invalidCredentialsBody,
			mutate: func(s *sessionHarness) {
				calls := 0
				s.users.setHook(func(context.Context) {
					calls++
					if calls == 2 { // the re-check after signing
						s.users.update("u1", func(u *models.User) { u.Disabled = true })
					}
				})
			}},
		{name: "user deleted while the token was signed", wantCode: http.StatusUnauthorized, wantBody: invalidCredentialsBody,
			mutate: func(s *sessionHarness) {
				calls := 0
				s.users.setHook(func(context.Context) {
					calls++
					if calls == 2 {
						s.users.remove("u1")
					}
				})
			}},
		{name: "re-check after signing fails", wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`,
			mutate: func(s *sessionHarness) {
				calls := 0
				s.users.setHook(func(context.Context) {
					calls++
					if calls == 2 {
						s.users.mu.Lock()
						s.users.findByIDErr = errors.New("dial tcp")
						s.users.mu.Unlock()
					}
				})
			}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var opts []harnessOpt
			if tt.wrap != nil {
				opts = append(opts, withTokenRepo(tt.wrap))
			}
			s := newSessionHarness(t, nil, opts...)
			if tt.mutate != nil {
				tt.mutate(s)
			}
			w := s.post(t, "/api/v1/auth/login", loginBody(t, "alice", testPassword))
			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			if ck := refreshCookieOf(t, w); ck != nil {
				assert.Empty(t, ck.Value, "no refresh token leaves the server")
			}
			n, err := s.tokens.CountActiveForUser(context.Background(), "u1")
			require.NoError(t, err)
			assert.Zero(t, n, "no usable session is left behind")
			assert.NotContains(t, w.Body.String(), "secret detail", "no internal detail reaches the client")
		})
	}
}

// A refresh rotates: the new refresh token and access token work, the old
// refresh token no longer gives a new cookie, and the access token carries
// the current role of the user.
func TestRefreshRotates(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	access, raw := s.login(t, "alice")
	sid := claimsOf(t, access).SessionID
	s.users.update("u1", func(u *models.User) { u.Role = models.RoleAdmin })

	before := time.Now()
	w, newAccess, next := s.refresh(t, raw)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, refreshResponseSchema, w.Body.Bytes()))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.NotEmpty(t, next)
	assert.NotEqual(t, raw, next)
	assertCookieAttributes(t, refreshCookieOf(t, w))

	claims := claimsOf(t, newAccess)
	assert.Equal(t, sid, claims.SessionID, "same session")
	assert.Equal(t, models.RoleAdmin, claims.Role, "the role is re-read, not copied")
	assert.WithinDuration(t, before.Add(s.cfg.AccessTokenExpiration), claims.ExpiresAt.Time, 5*time.Second)
	assert.Equal(t, http.StatusOK, s.me(t, newAccess))

	old := s.row(t, raw)
	assert.True(t, old.Revoked)
	require.NotNil(t, old.RotatedAt)
	successor := s.row(t, next)
	assert.Equal(t, sid, successor.FamilyID)
	assert.Equal(t, old.SessionStartedAt.UTC(), successor.SessionStartedAt.UTC(), "rotation keeps the session start")
	assert.False(t, successor.LastActivity.Before(before.Add(-time.Second)), "a refresh is the activity of the session")

	// The next rotation works with the successor.
	w, _, third := s.refresh(t, next)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, third)
	assert.NotContains(t, s.logs.String(), raw)
	assert.NotContains(t, s.logs.String(), next)
}

// Reuse of a consumed token inside the grace window gets one access token
// (no cookie); the second reuse revokes the family and blocks its session.
func TestRefreshReuseGraceWorksOnce(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	w, rotatedAccess, next := s.refresh(t, raw)
	require.Equal(t, http.StatusOK, w.Code)
	sid := claimsOf(t, rotatedAccess).SessionID

	w, graceAccess, cookie := s.refresh(t, raw)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, refreshResponseSchema, w.Body.Bytes()))
	assert.Empty(t, cookie, "the grace answer sets no cookie")
	assert.Nil(t, refreshCookieOf(t, w))
	assert.Equal(t, sid, claimsOf(t, graceAccess).SessionID)
	assert.Equal(t, int64(1), s.familyActive(t, sid), "the grace mints no refresh token")

	w, _, _ = s.refresh(t, raw)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, invalidSessionBody, w.Body.String())
	assertCookieCleared(t, w)
	assert.Zero(t, s.familyActive(t, sid), "the second reuse revokes the family")
	assert.True(t, s.sessionBlocked(t, sid))
	assert.Equal(t, http.StatusUnauthorized, s.me(t, graceAccess), "the access tokens of the family stop working")
	w, _, _ = s.refresh(t, next)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "the successor is revoked too")
}

// Reuse of a consumed token outside the grace window (or with the grace off)
// revokes the whole family and blocks its session, with the generic answer.
func TestRefreshReuseOutsideGraceRevokesTheFamily(t *testing.T) {
	t.Parallel()
	for _, graceOff := range []bool{false, true} {
		graceOff := graceOff
		name := "after the window"
		if graceOff {
			name = "grace off"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, func(c *config.AuthConfig) {
				if graceOff {
					c.RefreshReuseGrace = 0
				}
			})
			_, raw := s.login(t, "alice")
			_, _, next := s.refresh(t, raw)
			require.NotEmpty(t, next)
			access, _, next2 := s.refresh(t, next)
			require.Equal(t, http.StatusOK, access.Code)
			require.NotEmpty(t, next2)
			if !graceOff {
				s.setRow(t, raw, map[string]any{"rotated_at": time.Now().UTC().Add(-time.Minute)})
			}
			sid := s.row(t, raw).FamilyID

			w, _, _ := s.refresh(t, raw)
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.JSONEq(t, invalidSessionBody, w.Body.String())
			assertCookieCleared(t, w)
			assert.Zero(t, s.familyActive(t, sid))
			assert.True(t, s.sessionBlocked(t, sid))
			w, _, _ = s.refresh(t, next2)
			assert.Equal(t, http.StatusUnauthorized, w.Code, "the newest token of the family is revoked")
			assert.Contains(t, s.logs.String(), "Refresh token reuse detected")
		})
	}
}

// Every refused refresh answers the same 401 body and clears the cookie, so
// a client cannot tell an unknown, expired, consumed or revoked token, an
// ended session or an account state apart.
func TestRefreshRefusalsAreIndistinguishable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prepare     func(t *testing.T, s *sessionHarness, raw string) string // returns the cookie to send
		name        string
		wantRevoked bool // the session's tokens are revoked afterwards
	}{
		{name: "unknown token", prepare: func(*testing.T, *sessionHarness, string) string {
			return base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		}},
		{name: "malformed token", prepare: func(*testing.T, *sessionHarness, string) string { return "not-a-token" }},
		{name: "a JWT is not a refresh token", prepare: func(t *testing.T, _ *sessionHarness, _ string) string { return tokenFor(t, "u1") }},
		{name: "expired token", prepare: func(t *testing.T, s *sessionHarness, raw string) string {
			s.setRow(t, raw, map[string]any{"expires_at": time.Now().UTC().Add(-time.Second)})
			return raw
		}},
		{name: "revoked by logout", wantRevoked: true, prepare: func(t *testing.T, s *sessionHarness, raw string) string {
			s.setRow(t, raw, map[string]any{"revoked": true})
			return raw
		}},
		{name: "session past SESSION_MAX_LIFETIME", wantRevoked: true, prepare: func(t *testing.T, s *sessionHarness, raw string) string {
			s.setRow(t, raw, map[string]any{"session_started_at": time.Now().UTC().Add(-s.cfg.SessionMaxLifetime - time.Second)})
			return raw
		}},
		{name: "session idle past SESSION_IDLE_TIMEOUT", wantRevoked: true, prepare: func(t *testing.T, s *sessionHarness, raw string) string {
			s.setRow(t, raw, map[string]any{"last_activity": time.Now().UTC().Add(-s.cfg.SessionIdleTimeout - time.Second)})
			return raw
		}},
		{name: "session blocked", wantRevoked: true, prepare: func(t *testing.T, s *sessionHarness, raw string) string {
			require.NoError(t, s.store.BlockSession(context.Background(), s.row(t, raw).FamilyID, time.Now().Add(time.Hour)))
			return raw
		}},
		{name: "user disabled", wantRevoked: true, prepare: func(_ *testing.T, s *sessionHarness, raw string) string {
			s.users.update("u1", func(u *models.User) { u.Disabled = true })
			return raw
		}},
		{name: "user deleted", wantRevoked: true, prepare: func(_ *testing.T, s *sessionHarness, raw string) string {
			s.users.remove("u1")
			return raw
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, nil)
			_, raw := s.login(t, "alice")
			family := s.row(t, raw).FamilyID
			rowsBefore := s.rowCount(t)
			cookie := tt.prepare(t, s, raw)

			w, _, _ := s.refresh(t, cookie)

			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.JSONEq(t, invalidSessionBody, w.Body.String())
			assertCookieCleared(t, w)
			assert.Equal(t, rowsBefore, s.rowCount(t), "no token is minted")
			if tt.wantRevoked {
				assert.Zero(t, s.familyActive(t, family))
			}
			assert.NotContains(t, s.logs.String(), cookie)
		})
	}

	t.Run("missing cookie", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		w := s.post(t, "/api/v1/auth/refresh", "")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.JSONEq(t, invalidSessionBody, w.Body.String())
	})
}

// SESSION_MAX_LIFETIME counts from the login: rotation keeps the start, the
// refresh token never outlives the deadline and the access token is cut to
// it.
func TestRefreshNeverExtendsTheSession(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	start := time.Now().UTC().Add(-s.cfg.SessionMaxLifetime + 5*time.Minute)
	s.setRow(t, raw, map[string]any{"session_started_at": start})

	w, access, next := s.refresh(t, raw)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	deadline := start.Add(s.cfg.SessionMaxLifetime)
	successor := s.row(t, next)
	assert.WithinDuration(t, start, successor.SessionStartedAt, time.Second)
	assert.False(t, successor.ExpiresAt.After(deadline.Add(time.Second)), "the token ends with the session")
	assert.False(t, claimsOf(t, access).ExpiresAt.After(deadline.Add(time.Second)), "the access token ends with the session")
	ck := refreshCookieOf(t, w)
	assert.LessOrEqual(t, ck.MaxAge, int((5 * time.Minute).Seconds()))
	assert.Positive(t, ck.MaxAge)

	// After the deadline the session is over.
	s.setRow(t, next, map[string]any{"session_started_at": time.Now().UTC().Add(-s.cfg.SessionMaxLifetime - time.Second)})
	w, _, _ = s.refresh(t, next)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// SESSION_IDLE_TIMEOUT counts from the last use of the session (the last
// login or refresh): just inside it the refresh works and moves it forward.
func TestRefreshIdleTimeout(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	s.setRow(t, raw, map[string]any{"last_activity": time.Now().UTC().Add(-s.cfg.SessionIdleTimeout + time.Minute)})

	w, _, next := s.refresh(t, raw)
	require.Equal(t, http.StatusOK, w.Code)
	assert.WithinDuration(t, time.Now(), s.row(t, next).LastActivity, 5*time.Second)
}

// The refresh and logout endpoints authenticate with a cookie: a request
// without the X-Requested-With header, or a cross-site one, is refused before
// anything changes.
func TestCookieEndpointsRefuseCrossSiteRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []reqOpt
	}{
		{name: "no X-Requested-With header", opts: []reqOpt{withoutCSRF()}},
		{name: "no header, foreign origin", opts: []reqOpt{withoutCSRF(), withHeader("Origin", "https://evil.example")}},
		{name: "cross-site fetch metadata", opts: []reqOpt{withHeader("Sec-Fetch-Site", "cross-site")}},
	}
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/logout"} {
		for _, tt := range tests {
			path, tt := path, tt
			t.Run(path+" "+tt.name, func(t *testing.T) {
				t.Parallel()
				s := newSessionHarness(t, nil)
				access, raw := s.login(t, "alice")
				w := s.post(t, path, "", append([]reqOpt{withCookie(raw), withBearer(access)}, tt.opts...)...)
				assert.Equal(t, http.StatusForbidden, w.Code)
				assert.True(t, validateJSONSchema(t, errorSchema, w.Body.Bytes()))
				assert.Nil(t, refreshCookieOf(t, w), "the cookie is untouched")
				assert.False(t, s.row(t, raw).Revoked, "nothing was revoked or rotated")
				assert.Equal(t, http.StatusOK, s.me(t, access), "the session still works")
			})
		}
	}
}

// Login CSRF: a login that sets the session cookie needs the same-site marker
// and a JSON Content-Type, checked before the body, the user lookup, bcrypt
// and the throttle. A cross-site form (text/plain carrying a JSON body)
// cannot plant the attacker's session, and forged attempts do not use up the
// throttle budget of the victim's name.
func TestLoginRefusesForgedCrossSiteRequests(t *testing.T) {
	t.Parallel()
	// The body a cross-site <form enctype="text/plain"> produces from the
	// input name `{"username":"alice","password":"x","x":"` and value `"}`.
	forged := `{"username":"alice","password":"wrong-password","x":"="}`
	tests := []struct {
		name        string
		contentType string
		wantBody    string
		opts        []reqOpt
		wantCode    int
	}{
		{name: "forged text/plain form", contentType: "text/plain", opts: []reqOpt{withoutCSRF()},
			wantCode: http.StatusForbidden, wantBody: `{"error":"Cross-site request refused: send the X-Requested-With header"}`},
		{name: "JSON without the marker", contentType: "application/json", opts: []reqOpt{withoutCSRF()},
			wantCode: http.StatusForbidden, wantBody: `{"error":"Cross-site request refused: send the X-Requested-With header"}`},
		{name: "cross-site fetch metadata", contentType: "application/json", opts: []reqOpt{withHeader("Sec-Fetch-Site", "cross-site")},
			wantCode: http.StatusForbidden, wantBody: `{"error":"Cross-site request refused: send the X-Requested-With header"}`},
		{name: "marker but text/plain", contentType: "text/plain",
			wantCode: http.StatusUnsupportedMediaType, wantBody: `{"error":"Content-Type must be application/json"}`},
		{name: "marker but a form encoding", contentType: "application/x-www-form-urlencoded",
			wantCode: http.StatusUnsupportedMediaType, wantBody: `{"error":"Content-Type must be application/json"}`},
		{name: "marker but no Content-Type", contentType: "",
			wantCode: http.StatusUnsupportedMediaType, wantBody: `{"error":"Content-Type must be application/json"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, nil)
			var lookups int
			var mu sync.Mutex
			s.users.gate = func(op, _ string) {
				if op == "by_username" {
					mu.Lock()
					lookups++
					mu.Unlock()
				}
			}
			opts := append([]reqOpt{withHeader("Content-Type", tt.contentType)}, tt.opts...)

			// Ten forged attempts with a wrong password: more than the throttle
			// threshold if any of them reached the password check.
			for i := 0; i < 10; i++ {
				w := s.post(t, "/api/v1/auth/login", forged, opts...)
				require.Equal(t, tt.wantCode, w.Code, w.Body.String())
				assert.JSONEq(t, tt.wantBody, w.Body.String())
				assert.Nil(t, refreshCookieOf(t, w), "no cookie is set")
			}
			assert.Zero(t, s.rowCount(t), "no session is created")
			mu.Lock()
			assert.Zero(t, lookups, "refused before the user lookup")
			mu.Unlock()

			// The victim's own login still works: no throttle budget was used.
			_, raw := s.login(t, "alice")
			assert.NotEmpty(t, raw)
		})
	}

	t.Run("a JSON Content-Type with parameters is accepted", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		w := s.post(t, "/api/v1/auth/login", loginBody(t, "alice", testPassword), withHeader("Content-Type", "application/json; charset=utf-8"))
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.NotNil(t, refreshCookieOf(t, w))
	})

	// Without a refresh repository the login sets no cookie and is unchanged
	// (no marker, any Content-Type): its token is only in the JSON body,
	// which a cross-site page cannot read.
	t.Run("without refresh sessions the login is unchanged", func(t *testing.T) {
		t.Parallel()
		h, _, _ := newTestAuthHandler(newMockUserRepo(localUser("u1", "alice")), testAuthConfig(), nil)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody(t, "alice", testPassword)))
		req.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		newAuthRouter(h).ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Values("Set-Cookie"))
	})
}

// A client that leaves during the refresh gets 499 and nothing changes: the
// token stays active and can still be used.
func TestRefreshClientCancelRevokesNothing(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	rowsBefore := s.rowCount(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.users.setHook(func(context.Context) { cancel() })

	w, _, _ := s.refresh(t, raw, withContext(ctx))

	assert.Equal(t, statusClientClosedRequest, w.Code)
	assert.JSONEq(t, `{"error":"Client closed request"}`, w.Body.String())
	assert.Nil(t, refreshCookieOf(t, w))
	row := s.row(t, raw)
	assert.False(t, row.Revoked)
	assert.Nil(t, row.RotatedAt)
	assert.Equal(t, rowsBefore, s.rowCount(t))

	s.users.setHook(nil)
	w, _, next := s.refresh(t, raw)
	assert.Equal(t, http.StatusOK, w.Code, "the token still works")
	assert.NotEmpty(t, next)
}

// Server-side failures answer 500 (or 499 for a cancel) without detail and
// leave the token usable; the session store fails closed.
func TestRefreshServerFailures(t *testing.T) {
	t.Parallel()
	const internal = `{"error":"Internal server error"}`
	tests := []struct {
		tokens   func(models.RefreshTokenRepository) models.RefreshTokenRepository
		store    func(sessionstore.SessionStore) sessionstore.SessionStore
		users    func(r *mockUserRepo)
		name     string
		wantBody string
		wantCode int
	}{
		{name: "token lookup fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, findErr: errors.New("dial tcp: secret detail")}
			}},
		{name: "token lookup deadline", wantCode: http.StatusInternalServerError, wantBody: internal,
			tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, findErr: context.DeadlineExceeded}
			}},
		{name: "token lookup cancelled", wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`,
			tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, findErr: context.Canceled}
			}},
		{name: "session store fails (fail closed)", wantCode: http.StatusInternalServerError, wantBody: internal,
			store: func(st sessionstore.SessionStore) sessionstore.SessionStore {
				return &faultyStore{SessionStore: st, sessionErr: errors.New("dial tcp")}
			}},
		{name: "session store cancelled", wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`,
			store: func(st sessionstore.SessionStore) sessionstore.SessionStore {
				return &faultyStore{SessionStore: st, sessionErr: context.Canceled}
			}},
		{name: "user lookup fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			users: func(r *mockUserRepo) { r.findByIDErr = errors.New("dial tcp") }},
		{name: "rotation fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, createErr: errors.New("dial tcp")}
			}},
		{name: "session check after rotation fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, countFamilyErr: errors.New("dial tcp")}
			}},
		{name: "session revoked during the rotation", wantCode: http.StatusUnauthorized, wantBody: invalidSessionBody,
			tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, zeroActive: true}
			}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, nil)
			_, raw := s.login(t, "alice")
			if tt.tokens != nil {
				s.h.SetRefreshTokenRepo(tt.tokens(s.tokens))
			}
			if tt.store != nil {
				s.h.SetSessionStore(tt.store(s.store))
			}
			if tt.users != nil {
				s.users.mu.Lock()
				tt.users(s.users.mockUserRepo)
				s.users.mu.Unlock()
			}
			w, _, _ := s.refresh(t, raw)
			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.NotContains(t, w.Body.String(), "secret detail")
			if tt.wantCode == statusClientClosedRequest || tt.name == "token lookup fails" || tt.name == "user lookup fails" {
				assert.False(t, s.row(t, raw).Revoked, "a failed read changes nothing")
			}
		})
	}
}

// onNthFindByID runs fn at the n-th FindByID of the user repository.
func onNthFindByID(s *sessionHarness, n int, fn func()) {
	var mu sync.Mutex
	calls := 0
	s.users.setHook(func(context.Context) {
		mu.Lock()
		calls++
		hit := calls == n
		mu.Unlock()
		if hit {
			fn()
		}
	})
}

func setFindByIDErr(s *sessionHarness, err error) {
	s.users.mu.Lock()
	s.users.findByIDErr = err
	s.users.mu.Unlock()
}

// The grace answer needs an active user, a live and unblocked session and
// the one claim; anything else is a replay (401, family revoked) or, for a
// server failure, 500 with nothing revoked.
func TestRefreshGraceBranches(t *testing.T) {
	t.Parallel()
	const internal = `{"error":"Internal server error"}`
	dbErr := errors.New("dial tcp")
	tests := []struct {
		prepare     func(t *testing.T, s *sessionHarness, next string)
		name        string
		wantBody    string
		wantCode    int
		wantRevoked bool
	}{
		{name: "user disabled: replay", wantCode: http.StatusUnauthorized, wantBody: invalidSessionBody, wantRevoked: true,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) {
				s.users.update("u1", func(u *models.User) { u.Disabled = true })
			}},
		{name: "user deleted: replay", wantCode: http.StatusUnauthorized, wantBody: invalidSessionBody, wantRevoked: true,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) { s.users.remove("u1") }},
		{name: "session already revoked: replay", wantCode: http.StatusUnauthorized, wantBody: invalidSessionBody, wantRevoked: true,
			prepare: func(t *testing.T, s *sessionHarness, next string) { s.setRow(t, next, map[string]any{"revoked": true}) }},
		{name: "session blocked: replay", wantCode: http.StatusUnauthorized, wantBody: invalidSessionBody, wantRevoked: true,
			prepare: func(t *testing.T, s *sessionHarness, next string) {
				require.NoError(t, s.store.BlockSession(context.Background(), s.row(t, next).FamilyID, time.Now().Add(time.Hour)))
			}},
		{name: "session past SESSION_MAX_LIFETIME: replay", wantCode: http.StatusUnauthorized, wantBody: invalidSessionBody, wantRevoked: true,
			prepare: func(t *testing.T, s *sessionHarness, next string) {
				old := time.Now().UTC().Add(-s.cfg.SessionMaxLifetime - time.Minute)
				require.NoError(t, s.tokens.DB.Model(&models.RefreshToken{}).Where("family_id = ?", s.row(t, next).FamilyID).Update("session_started_at", old).Error)
			}},
		{name: "user lookup fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) { setFindByIDErr(s, dbErr) }},
		{name: "user lookup cancelled", wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) { setFindByIDErr(s, context.Canceled) }},
		{name: "family count fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) {
				s.h.SetRefreshTokenRepo(&faultyTokens{RefreshTokenRepository: s.tokens, countFamilyErr: dbErr})
			}},
		{name: "session store fails (fail closed)", wantCode: http.StatusInternalServerError, wantBody: internal,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) {
				s.h.SetSessionStore(&faultyStore{SessionStore: s.store, sessionErr: dbErr})
			}},
		{name: "claim fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) {
				s.h.SetRefreshTokenRepo(&faultyTokens{RefreshTokenRepository: s.tokens, claimErr: dbErr})
			}},
		{name: "user disabled while the grace token was signed", wantCode: http.StatusUnauthorized, wantBody: invalidSessionBody, wantRevoked: true,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) {
				onNthFindByID(s, 2, func() { s.users.update("u1", func(u *models.User) { u.Disabled = true }) })
			}},
		{name: "re-check of the grace token fails", wantCode: http.StatusInternalServerError, wantBody: internal,
			prepare: func(_ *testing.T, s *sessionHarness, _ string) {
				onNthFindByID(s, 2, func() { setFindByIDErr(s, dbErr) })
			}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, nil)
			_, raw := s.login(t, "alice")
			w, _, next := s.refresh(t, raw)
			require.Equal(t, http.StatusOK, w.Code)
			family := s.row(t, next).FamilyID
			tt.prepare(t, s, next)

			w, _, _ = s.refresh(t, raw)

			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.NotContains(t, w.Body.String(), "dial tcp")
			if tt.wantRevoked {
				assert.Zero(t, s.familyActive(t, family))
			} else {
				s.h.SetRefreshTokenRepo(s.tokens)
				assert.Equal(t, int64(1), s.familyActive(t, family), "a server failure revokes nothing")
			}
		})
	}
}

// The re-check after the rotation: a user disabled or deleted while the
// token was signed gets no token and loses every session; a failing re-check
// is a 500.
func TestRefreshRecheckAfterRotation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		apply    func(s *sessionHarness)
		name     string
		wantCode int
	}{
		{name: "disabled", wantCode: http.StatusUnauthorized, apply: func(s *sessionHarness) { s.users.update("u1", func(u *models.User) { u.Disabled = true }) }},
		{name: "deleted", wantCode: http.StatusUnauthorized, apply: func(s *sessionHarness) { s.users.remove("u1") }},
		{name: "repository error", wantCode: http.StatusInternalServerError, apply: func(s *sessionHarness) { setFindByIDErr(s, errors.New("dial tcp")) }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, nil)
			_, other := s.login(t, "alice")
			_, raw := s.login(t, "alice")
			onNthFindByID(s, 2, func() { tt.apply(s) }) // 1: refreshUser, 2: the re-check

			w, _, _ := s.refresh(t, raw)

			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			if ck := refreshCookieOf(t, w); ck != nil {
				assert.Empty(t, ck.Value, "no new refresh token leaves the server")
			}
			if tt.wantCode == http.StatusUnauthorized {
				assert.JSONEq(t, invalidSessionBody, w.Body.String())
				assert.True(t, s.row(t, other).Revoked, "every session of the user ends")
			}
		})
	}
}

// A replay whose family revoke fails answers 500 and gets the long session
// block, so the still active successor cannot refresh.
func TestRefreshReplayRevokeFailure(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, func(c *config.AuthConfig) { c.RefreshReuseGrace = 0 })
	_, raw := s.login(t, "alice")
	_, _, next := s.refresh(t, raw)
	require.NotEmpty(t, next)
	family := s.row(t, next).FamilyID
	s.h.SetRefreshTokenRepo(&faultyTokens{RefreshTokenRepository: s.tokens, revokeErr: errors.New("dial tcp")})

	w, _, _ := s.refresh(t, raw)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assertCookieCleared(t, w)
	assert.True(t, s.sessionBlocked(t, family))

	s.h.SetRefreshTokenRepo(s.tokens)
	w, _, _ = s.refresh(t, next)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "the blocked session cannot refresh")
}

// Without a refresh repository the endpoint is off.
func TestRefreshNotEnabled(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
	r := gin.New()
	r.POST("/api/v1/auth/refresh", h.Refresh)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.JSONEq(t, `{"error":"Refresh tokens are not enabled"}`, w.Body.String())
}

// Two tabs that refresh with the same cookie at the same time both get an
// access token (one rotation, one grace answer) and the session survives.
func TestRefreshConcurrentPairKeepsTheSession(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	sid := s.row(t, raw).FamilyID

	codes, cookies := concurrentRefreshes(t, s, raw, 2)

	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
	assert.Equal(t, 1, cookies, "exactly one rotation sets a new cookie")
	assert.Equal(t, int64(1), s.familyActive(t, sid), "one successor")
	assert.False(t, s.sessionBlocked(t, sid))
}

// Many concurrent refreshes with the same token mint one refresh token and
// at most one grace access token; the rest is a replay that ends the
// session.
func TestRefreshConcurrentBurstMintsOnce(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	sid := s.row(t, raw).FamilyID
	const n = 8

	codes, cookies := concurrentRefreshes(t, s, raw, n)

	// Which requests answer 200 depends on the interleaving: a replay can
	// revoke the family before the rotation or the grace answer has passed its
	// session re-check. What never changes: one successor row, at most one
	// rotation answer and one grace answer, and the family ends revoked.
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		} else {
			assert.Equal(t, http.StatusUnauthorized, c)
		}
	}
	assert.LessOrEqual(t, ok, 2, "at most one rotation and one grace answer")
	assert.LessOrEqual(t, cookies, 1, "at most one new cookie")
	assert.Zero(t, s.familyActive(t, sid), "the extra reuse revoked the family")
	assert.True(t, s.sessionBlocked(t, sid))
	var rows int64
	require.NoError(t, s.tokens.DB.Model(&models.RefreshToken{}).Where("family_id = ?", sid).Count(&rows).Error)
	assert.Equal(t, int64(2), rows, "the login token and one successor")
}

// concurrentRefreshes sends n refreshes with raw at once. It returns the
// status codes (sorted) and how many answers set a new refresh token.
func concurrentRefreshes(t *testing.T, s *sessionHarness, raw string, n int) ([]int, int) {
	t.Helper()
	start := make(chan struct{})
	codes := make([]int, n)
	setCookie := make([]bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			req.AddCookie(&http.Cookie{Name: hostRefreshCookie, Value: raw})
			w := httptest.NewRecorder()
			s.router.ServeHTTP(w, req)
			codes[i] = w.Code
			for _, ck := range w.Result().Cookies() {
				if ck.Name == hostRefreshCookie && ck.Value != "" {
					setCookie[i] = true
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	minted := 0
	for _, set := range setCookie {
		if set {
			minted++
		}
	}
	sortInts(codes)
	return codes, minted
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// The raw refresh token is 256 random bits, base64url, never repeated.
func TestGenerateRefreshToken(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, err := generateRefreshToken()
		require.NoError(t, err)
		require.Len(t, raw, refreshTokenEncodedLen)
		b, err := base64.RawURLEncoding.DecodeString(raw)
		require.NoError(t, err)
		require.Len(t, b, 32)
		require.False(t, seen[raw])
		seen[raw] = true
		assert.Equal(t, sha256Hex(raw), hashRefreshToken(raw))
	}
}

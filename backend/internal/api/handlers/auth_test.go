package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	testJWTSecret = "test-jwt-secret-0123456789"
	testPassword  = "Correct-Horse-9"
)

// testPasswordHash is a MinCost hash of testPassword (fast tests; the cost
// of a stored hash does not change the login logic).
var testPasswordHash = sync.OnceValue(func() string {
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(hash)
})

func minCostHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	return string(hash)
}

func testAuthConfig() *config.AuthConfig {
	return &config.AuthConfig{
		Enabled:               true,
		JWTSecret:             testJWTSecret,
		JWTExpiration:         2 * time.Hour,
		AccessTokenExpiration: 15 * time.Minute,
		SessionIdleTimeout:    30 * time.Minute,
		SessionMaxLifetime:    12 * time.Hour,
		AdminUsername:         "admin",
	}
}

func notFoundErr() error { return dberrors.NewDatabaseError("find", dberrors.ErrNotFound) }

// mockUserRepo is an in-memory models.UserRepository for the auth handler
// tests. Usernames match exactly.
type mockUserRepo struct {
	findByUsernameErr error
	findByIDErr       error
	createErr         error
	// afterFindByUsername runs after each FindByUsername, outside the lock:
	// a test changes the stored user between the lookup and the re-read.
	afterFindByUsername func(r *mockUserRepo)
	users               map[string]*models.User // by ID
	mu                  sync.Mutex
	creates             int
}

func newMockUserRepo(users ...models.User) *mockUserRepo {
	r := &mockUserRepo{users: map[string]*models.User{}}
	for i := range users {
		u := users[i]
		r.users[u.ID] = &u
	}
	return r
}

func (r *mockUserRepo) Create(_ context.Context, user *models.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates++
	if r.createErr != nil {
		return r.createErr
	}
	for _, u := range r.users {
		if u.Username == user.Username {
			return dberrors.NewDatabaseError("create", dberrors.ErrDuplicateKey)
		}
	}
	if user.Role == "" {
		user.Role = models.RoleUser
	}
	if user.AuthProvider == "" {
		user.AuthProvider = models.AuthProviderLocal
	}
	if err := user.Validate(); err != nil {
		return dberrors.NewDatabaseError("create", errors.Join(dberrors.ErrValidation, err))
	}
	if user.ID == "" {
		user.ID = "id-" + user.Username
	}
	user.CreatedAt = time.Now().UTC()
	user.UpdatedAt = user.CreatedAt
	cp := *user
	r.users[cp.ID] = &cp
	return nil
}

func (r *mockUserRepo) FindByID(_ context.Context, id string) (*models.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.findByIDErr != nil {
		return nil, r.findByIDErr
	}
	u, ok := r.users[id]
	if !ok {
		return nil, notFoundErr()
	}
	cp := *u
	return &cp, nil
}

func (r *mockUserRepo) FindByUsername(_ context.Context, username string) (*models.User, error) {
	user, err := r.findByUsername(username)
	if r.afterFindByUsername != nil {
		r.afterFindByUsername(r)
	}
	return user, err
}

func (r *mockUserRepo) findByUsername(username string) (*models.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.findByUsernameErr != nil {
		return nil, r.findByUsernameErr
	}
	for _, u := range r.users {
		if u.Username == username {
			cp := *u
			return &cp, nil
		}
	}
	return nil, notFoundErr()
}

// update changes the stored user with ID id.
func (r *mockUserRepo) update(id string, fn func(u *models.User)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.users[id]; ok {
		fn(u)
	}
}

func (r *mockUserRepo) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.users, id)
}

func (r *mockUserRepo) byUsername(t *testing.T, username string) *models.User {
	t.Helper()
	u, err := r.findByUsername(username)
	require.NoError(t, err)
	return u
}

func (r *mockUserRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.users)
}

func (r *mockUserRepo) FindByIDs(context.Context, []string) (map[string]*models.User, error) {
	return nil, dberrors.ErrNotImplemented
}

func (r *mockUserRepo) FindByExternalID(context.Context, string, string) (*models.User, error) {
	return nil, dberrors.ErrNotImplemented
}

func (r *mockUserRepo) UpdatePassword(context.Context, string, string) error {
	return dberrors.ErrNotImplemented
}

func (r *mockUserRepo) UpdateProfile(context.Context, string, models.UserProfileUpdate) error {
	return dberrors.ErrNotImplemented
}

func (r *mockUserRepo) Delete(context.Context, string) error { return dberrors.ErrNotImplemented }

func (r *mockUserRepo) ListPaged(context.Context, models.UserFilter, int, int) ([]models.User, int64, error) {
	return nil, 0, dberrors.ErrNotImplemented
}

func (r *mockUserRepo) Count(context.Context) (int64, error) { return 0, dberrors.ErrNotImplemented }

func (r *mockUserRepo) UpdateRole(context.Context, string, string, string) (string, error) {
	return "", dberrors.ErrNotImplemented
}

func (r *mockUserRepo) SetDisabled(context.Context, string, string, bool) error {
	return dberrors.ErrNotImplemented
}

func (r *mockUserRepo) DeleteGuarded(context.Context, string, string) error {
	return dberrors.ErrNotImplemented
}

var _ models.UserRepository = (*mockUserRepo)(nil)

// recordingCompare wraps bcrypt.CompareHashAndPassword and records the hash
// and the password length of each call.
type recordingCompare struct {
	hashes [][]byte
	pwLens []int
	mu     sync.Mutex
}

func (rc *recordingCompare) compare(hash, password []byte) error {
	rc.mu.Lock()
	rc.hashes = append(rc.hashes, append([]byte(nil), hash...))
	rc.pwLens = append(rc.pwLens, len(password))
	rc.mu.Unlock()
	return bcrypt.CompareHashAndPassword(hash, password)
}

func (rc *recordingCompare) calls() [][]byte {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([][]byte(nil), rc.hashes...)
}

// syncBuffer is a bytes.Buffer safe for concurrent writes (log capture).
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestAuthHandler returns a handler whose logs go to the returned buffer
// and whose bcrypt compares are recorded.
func newTestAuthHandler(repo models.UserRepository, cfg *config.AuthConfig, oidcCfg *config.OIDCConfig) (*AuthHandler, *syncBuffer, *recordingCompare) {
	h := NewAuthHandler(repo, cfg, oidcCfg)
	logs := &syncBuffer{}
	h.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rc := &recordingCompare{}
	h.compareHash = rc.compare
	return h, logs, rc
}

func newAuthRouter(h *AuthHandler, mw ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(mw...)
	r.POST("/api/v1/auth/login", h.Login)
	r.POST("/api/v1/auth/register", h.Register)
	authed := r.Group("/api/v1/auth", middleware.AuthRequiredWithOptions(middleware.JWTAuthOptions{JWTSecret: testJWTSecret}))
	authed.GET("/me", h.GetCurrentUser)
	// /me-open has no auth middleware: the handler sees no user ID.
	r.GET("/api/v1/auth/me-open", h.GetCurrentUser)
	return r
}

func postJSON(t *testing.T, r http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func loginBody(t *testing.T, username, password string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"username": username, "password": password})
	require.NoError(t, err)
	return string(b)
}

func localUser(id, username string) models.User {
	return models.User{
		ID:           id,
		Username:     username,
		PasswordHash: testPasswordHash(),
		DisplayName:  "Display " + username,
		Email:        username + "@example.com",
		Role:         models.RoleDevOps,
		AuthProvider: models.AuthProviderLocal,
	}
}

const invalidCredentialsBody = `{"error":"Invalid username or password"}`

func TestLoginSuccess(t *testing.T) {
	t.Parallel()
	repo := newMockUserRepo(localUser("u1", "alice"))
	cfg := testAuthConfig()
	h, logs, rc := newTestAuthHandler(repo, cfg, nil)
	r := newAuthRouter(h)

	before := time.Now()
	w := postJSON(t, r, "/api/v1/auth/login", loginBody(t, "alice", testPassword))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, loginResponseSchema, w.Body.Bytes()))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "no-cache", w.Header().Get("Pragma"))
	assert.NotContains(t, w.Body.String(), "password")
	assert.NotContains(t, w.Body.String(), "$2a$")

	var resp LoginResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "u1", resp.User.ID)
	assert.Equal(t, "alice", resp.User.Username)

	claims, err := middleware.ValidateJWT(resp.Token, testJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, "u1", claims.UserID)
	assert.Equal(t, "alice", claims.Username)
	assert.Equal(t, models.RoleDevOps, claims.Role)
	assert.Equal(t, models.AuthProviderLocal, claims.AuthProvider)
	assert.Empty(t, claims.SessionID, "no refresh session in this login")
	// Outside a refresh session the token lives JWTExpiration.
	require.NotNil(t, claims.ExpiresAt)
	assert.WithinDuration(t, before.Add(cfg.JWTExpiration), claims.ExpiresAt.Time, 5*time.Second)

	require.Len(t, rc.calls(), 1)
	assert.Equal(t, []byte(testPasswordHash()), rc.calls()[0])
	assert.Contains(t, logs.String(), "Login succeeded")
	assert.NotContains(t, logs.String(), testPassword)
	assert.NotContains(t, logs.String(), resp.Token)
}

// The token carries the role of the re-read user: a role change during the
// password check applies to the token.
func TestLoginUsesTheReReadUser(t *testing.T) {
	t.Parallel()
	repo := newMockUserRepo(localUser("u1", "alice"))
	repo.afterFindByUsername = func(r *mockUserRepo) {
		r.update("u1", func(u *models.User) { u.Role = models.RoleUser })
	}
	h, _, _ := newTestAuthHandler(repo, testAuthConfig(), nil)

	w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, "alice", testPassword))

	require.Equal(t, http.StatusOK, w.Code)
	var resp LoginResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	claims, err := middleware.ValidateJWT(resp.Token, testJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, models.RoleUser, claims.Role)
	assert.Equal(t, models.RoleUser, resp.User.Role)
}

// Every credential failure gives the same 401 and body after exactly one
// bcrypt compare. When no real hash can be checked the compare runs against
// the dummy hash, so the answer takes about the same time.
func TestLoginCredentialFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()

	long72 := strings.Repeat("a", maxPasswordBytes)
	tests := []struct {
		user      *models.User
		mutate    func(r *mockUserRepo)
		oidc      *config.OIDCConfig
		name      string
		username  string
		password  string
		wantDummy bool
		reason    string
	}{
		{name: "unknown user", username: "nobody", password: testPassword, wantDummy: true, reason: loginReasonUserNotFound},
		{name: "wrong password", username: "alice", password: "wrong-password", reason: loginReasonPasswordMismatch},
		{name: "empty password hash", username: "alice", password: testPassword, wantDummy: true, reason: loginReasonNoPassword,
			mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.PasswordHash = "" }) }},
		{name: "malformed password hash", username: "alice", password: testPassword, wantDummy: true, reason: loginReasonHashInvalid,
			mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.PasswordHash = "not-a-bcrypt-hash" }) }},
		{name: "oidc user with a hash", username: "alice", password: testPassword, wantDummy: true, reason: loginReasonNotLocal,
			mutate: func(r *mockUserRepo) {
				r.update("u1", func(u *models.User) { u.AuthProvider = models.AuthProviderOIDC })
			}},
		{name: "disabled user, correct password", username: "alice", password: testPassword, reason: loginReasonDisabled,
			mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.Disabled = true }) }},
		{name: "disabled user, wrong password", username: "alice", password: "wrong-password", reason: loginReasonPasswordMismatch,
			mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.Disabled = true }) }},
		{name: "oidc only, not a service account", username: "alice", password: testPassword, reason: loginReasonLocalLoginNotAllowed,
			oidc: &config.OIDCConfig{Enabled: true}},
		{name: "password over 72 bytes whose prefix is the password", username: "long", password: long72 + "b", wantDummy: true, reason: loginReasonPasswordTooLong},
		{name: "username over the column size", username: strings.Repeat("x", models.MaxUsernameLen+1), password: testPassword, wantDummy: true, reason: loginReasonUserNotFound},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			long := localUser("u2", "long")
			long.PasswordHash = minCostHash(t, long72)
			repo := newMockUserRepo(localUser("u1", "alice"), long)
			if tt.mutate != nil {
				tt.mutate(repo)
			}
			h, logs, rc := newTestAuthHandler(repo, testAuthConfig(), tt.oidc)

			w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, tt.username, tt.password))

			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.JSONEq(t, invalidCredentialsBody, w.Body.String())
			assert.Empty(t, w.Header().Get("Retry-After"))
			calls := rc.calls()
			require.Len(t, calls, 1, "exactly one bcrypt compare on every credential failure")
			if tt.wantDummy {
				assert.Equal(t, dummyPasswordHash(), calls[0], "compare against the dummy hash")
			} else {
				assert.NotEqual(t, dummyPasswordHash(), calls[0])
			}
			assert.LessOrEqual(t, rc.pwLens[0], maxPasswordBytes)
			out := logs.String()
			assert.Contains(t, out, `"reason":"`+tt.reason+`"`)
			assert.Contains(t, out, "Login failed")
			assert.NotContains(t, out, tt.password)
		})
	}
}

// The re-read after the password check catches a delete or a password
// change that committed during the check.
func TestLoginReReadFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mutate   func(r *mockUserRepo)
		name     string
		wantBody string
		wantCode int
	}{
		{name: "user deleted", wantCode: http.StatusUnauthorized, wantBody: invalidCredentialsBody,
			mutate: func(r *mockUserRepo) { r.remove("u1") }},
		{name: "password changed", wantCode: http.StatusUnauthorized, wantBody: invalidCredentialsBody,
			mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.PasswordHash = "$2a$04$other" }) }},
		{name: "disabled", wantCode: http.StatusUnauthorized, wantBody: invalidCredentialsBody,
			mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.Disabled = true }) }},
		{name: "re-read fails", wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`,
			mutate: func(r *mockUserRepo) {
				r.mu.Lock()
				r.findByIDErr = errors.New("connection reset: secret detail")
				r.mu.Unlock()
			}},
		{name: "re-read cancelled", wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`,
			mutate: func(r *mockUserRepo) {
				r.mu.Lock()
				r.findByIDErr = context.Canceled
				r.mu.Unlock()
			}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo(localUser("u1", "alice"))
			repo.afterFindByUsername = tt.mutate
			h, logs, _ := newTestAuthHandler(repo, testAuthConfig(), nil)

			w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, "alice", testPassword))

			assert.Equal(t, tt.wantCode, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.NotContains(t, w.Body.String(), "secret detail")
			assert.NotContains(t, logs.String(), testPassword)
		})
	}
}

func TestLoginAllowedAccounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		oidc   *config.OIDCConfig
		name   string
		user   models.User
		passwd string
	}{
		{name: "exactly 72 byte password", passwd: strings.Repeat("a", maxPasswordBytes), user: func() models.User {
			u := localUser("u1", "alice")
			hash, _ := bcrypt.GenerateFromPassword([]byte(strings.Repeat("a", maxPasswordBytes)), bcrypt.MinCost)
			u.PasswordHash = string(hash)
			return u
		}()},
		{name: "empty auth provider counts as local", passwd: testPassword, user: func() models.User {
			u := localUser("u1", "alice")
			u.AuthProvider = ""
			return u
		}()},
		{name: "oidc only, service account", passwd: testPassword, oidc: &config.OIDCConfig{Enabled: true}, user: func() models.User {
			u := localUser("u1", "alice")
			u.ServiceAccount = true
			return u
		}()},
		{name: "oidc with local auth allowed", passwd: testPassword, oidc: &config.OIDCConfig{Enabled: true, LocalAuth: true}, user: localUser("u1", "alice")},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _, _ := newTestAuthHandler(newMockUserRepo(tt.user), testAuthConfig(), tt.oidc)
			w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, "alice", tt.passwd))
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	}
}

func TestLoginRequestErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		lookupErr  error
		name       string
		body       string
		wantBody   string
		wantCode   int
		wantNoCall bool
	}{
		{name: "malformed JSON", body: `{"username":`, wantCode: http.StatusBadRequest, wantBody: `{"error":"Invalid request format"}`, wantNoCall: true},
		{name: "missing password", body: `{"username":"alice"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"Invalid request format"}`, wantNoCall: true},
		{name: "missing username", body: `{"password":"x"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"Invalid request format"}`, wantNoCall: true},
		{name: "body over the size limit", body: `{"username":"alice","password":"` + strings.Repeat("p", 2048) + `"}`,
			wantCode: http.StatusRequestEntityTooLarge, wantBody: `{"error":"request body too large"}`, wantNoCall: true},
		{name: "lookup fails", body: `{"username":"alice","password":"x"}`, lookupErr: errors.New("dial tcp: secret detail"),
			wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`, wantNoCall: true},
		{name: "lookup deadline exceeded", body: `{"username":"alice","password":"x"}`, lookupErr: context.DeadlineExceeded,
			wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`, wantNoCall: true},
		{name: "lookup cancelled", body: `{"username":"alice","password":"x"}`, lookupErr: context.Canceled,
			wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`, wantNoCall: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo(localUser("u1", "alice"))
			repo.findByUsernameErr = tt.lookupErr
			h, _, rc := newTestAuthHandler(repo, testAuthConfig(), nil)
			r := newAuthRouter(h, middleware.MaxBodySize(1024))

			w := postJSON(t, r, "/api/v1/auth/login", tt.body)

			assert.Equal(t, tt.wantCode, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			if tt.wantNoCall {
				assert.Empty(t, rc.calls())
			}
			// Server-side failures do not count towards the throttle.
			assert.Equal(t, 0, h.throttle.size())
		})
	}
}

func TestLoginTokenSigningFailure(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestAuthHandler(newMockUserRepo(localUser("u1", "alice")), testAuthConfig(), nil)
	h.signToken = func(middleware.GenerateTokenOptions) (string, error) {
		return "", errors.New("signing failed: secret detail")
	}

	w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, "alice", testPassword))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
}

// After loginFailureThreshold credential failures for one username the next
// attempt gets 429 with Retry-After, also with the correct password, and the
// same answer for a username that does not exist.
func TestLoginThrottlesRepeatedFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		username string
		// lastUsername is the username of the throttled attempt (case
		// variant of username).
		lastUsername string
	}{
		{name: "existing user", username: "alice", lastUsername: "alice"},
		{name: "existing user, other case", username: "alice", lastUsername: "ALICE"},
		{name: "unknown user", username: "nobody", lastUsername: "nobody"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo(localUser("u1", "alice"), localUser("u2", "ALICE"))
			h, logs, rc := newTestAuthHandler(repo, testAuthConfig(), nil)
			r := newAuthRouter(h)

			for i := 0; i < loginFailureThreshold; i++ {
				w := postJSON(t, r, "/api/v1/auth/login", loginBody(t, tt.username, "wrong-password"))
				require.Equal(t, http.StatusUnauthorized, w.Code, "attempt %d", i+1)
			}
			compares := len(rc.calls())

			w := postJSON(t, r, "/api/v1/auth/login", loginBody(t, tt.lastUsername, testPassword))

			assert.Equal(t, http.StatusTooManyRequests, w.Code)
			assert.JSONEq(t, `{"error":"Too many failed login attempts. Try again later."}`, w.Body.String())
			retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
			require.NoError(t, err)
			assert.Greater(t, retry, 0)
			assert.LessOrEqual(t, retry, int(loginFailureWindow/time.Second))
			assert.Equal(t, compares, len(rc.calls()), "a throttled attempt runs no compare")
			assert.Contains(t, logs.String(), "Login throttled")
		})
	}
}

// A successful login clears the failure counter of the username.
func TestLoginSuccessClearsTheThrottle(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestAuthHandler(newMockUserRepo(localUser("u1", "alice")), testAuthConfig(), nil)
	r := newAuthRouter(h)

	for round := 0; round < 2; round++ {
		for i := 0; i < loginFailureThreshold-1; i++ {
			w := postJSON(t, r, "/api/v1/auth/login", loginBody(t, "alice", "wrong-password"))
			require.Equal(t, http.StatusUnauthorized, w.Code, "round %d attempt %d", round, i+1)
		}
		w := postJSON(t, r, "/api/v1/auth/login", loginBody(t, "alice", testPassword))
		require.Equal(t, http.StatusOK, w.Code, "round %d", round)
	}
	assert.Equal(t, 0, h.throttle.size())
}

// Concurrent logins share the throttle without a data race (run with -race).
func TestLoginConcurrentFailures(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestAuthHandler(newMockUserRepo(localUser("u1", "alice")), testAuthConfig(), nil)
	h.compareHash = func([]byte, []byte) error { return bcrypt.ErrMismatchedHashAndPassword }
	r := newAuthRouter(h)

	const n = 20
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = postJSON(t, r, "/api/v1/auth/login", loginBody(t, "alice", "wrong-password")).Code
		}(i)
	}
	wg.Wait()

	unauthorized := 0
	for _, code := range codes {
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusTooManyRequests}, code)
		if code == http.StatusUnauthorized {
			unauthorized++
		}
	}
	// Requests that passed the check before the lock was set still answer
	// 401; at least the threshold did, so the username is locked now.
	assert.GreaterOrEqual(t, unauthorized, loginFailureThreshold)
	_, blocked := h.throttle.check(loginThrottleKey("alice"))
	assert.True(t, blocked)
}

func TestRegisterDisabledByDefault(t *testing.T) {
	t.Parallel()
	repo := newMockUserRepo()
	h, _, _ := newTestAuthHandler(repo, testAuthConfig(), nil)

	w := postJSON(t, newAuthRouter(h), "/api/v1/auth/register", `{"username":"newuser","password":"long-enough-1"}`)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.JSONEq(t, `{"error":"Not found"}`, w.Body.String())
	assert.Equal(t, 0, repo.count())
}

func selfRegistrationConfig() *config.AuthConfig {
	cfg := testAuthConfig()
	cfg.SelfRegistration = true
	return cfg
}

// Register always creates a local user with the user role. Role, disabled,
// provider, service account and hash fields in the body are ignored.
func TestRegisterCreatesAPlainUser(t *testing.T) {
	t.Parallel()
	repo := newMockUserRepo()
	h, logs, _ := newTestAuthHandler(repo, selfRegistrationConfig(), nil)

	body := `{"username":"newuser","password":"long-enough-1","email":"new@example.com",
		"role":"admin","disabled":true,"auth_provider":"oidc","service_account":true,
		"password_hash":"$2a$04$x","id":"chosen-id","external_id":"sub"}`
	w := postJSON(t, newAuthRouter(h), "/api/v1/auth/register", body)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, userSchema, w.Body.Bytes()))
	assert.NotContains(t, w.Body.String(), "$2a$")

	stored := repo.byUsername(t, "newuser")
	assert.Equal(t, models.RoleUser, stored.Role)
	assert.False(t, stored.Disabled)
	assert.False(t, stored.ServiceAccount)
	assert.Equal(t, models.AuthProviderLocal, stored.AuthProvider)
	assert.Nil(t, stored.ExternalID)
	assert.NotEqual(t, "chosen-id", stored.ID)
	assert.Equal(t, "newuser", stored.DisplayName, "display name defaults to the username")
	assert.Equal(t, "new@example.com", stored.Email)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte("long-enough-1")))
	cost, err := bcrypt.Cost([]byte(stored.PasswordHash))
	require.NoError(t, err)
	assert.Equal(t, passwordHashCost, cost)
	assert.NotContains(t, logs.String(), "long-enough-1")
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		wantBody string
		wantCode int
	}{
		{name: "malformed JSON", body: `{"username":`, wantCode: http.StatusBadRequest, wantBody: `{"error":"Invalid request format"}`},
		{name: "missing password", body: `{"username":"newuser"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"Invalid request format"}`},
		{name: "body over the size limit", body: `{"username":"newuser","password":"` + strings.Repeat("p", 2048) + `"}`,
			wantCode: http.StatusRequestEntityTooLarge, wantBody: `{"error":"request body too large"}`},
		{name: "username too short", body: `{"username":"ab","password":"long-enough-1"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgInvalidUsername + `"}`},
		{name: "username too long", body: `{"username":"` + strings.Repeat("a", models.MaxUsernameLen+1) + `","password":"long-enough-1"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgInvalidUsername + `"}`},
		{name: "username with a space", body: `{"username":"new user","password":"long-enough-1"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgInvalidUsername + `"}`},
		{name: "username with leading dot", body: `{"username":".newuser","password":"long-enough-1"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgInvalidUsername + `"}`},
		{name: "username non-ASCII", body: `{"username":"nëwuser","password":"long-enough-1"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgInvalidUsername + `"}`},
		{name: "password too short", body: `{"username":"newuser","password":"short12"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgPasswordTooShort + `"}`},
		{name: "password over 72 bytes", body: `{"username":"newuser","password":"` + strings.Repeat("a", maxPasswordBytes+1) + `"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgPasswordTooLong + `"}`},
		{name: "invalid email", body: `{"username":"newuser","password":"long-enough-1","email":"not-an-email"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgInvalidEmail + `"}`},
		{name: "email too long", body: `{"username":"newuser","password":"long-enough-1","email":"` + strings.Repeat("a", 250) + `@example.com"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgInvalidEmail + `"}`},
		{name: "display name too long", body: `{"username":"newuser","password":"long-enough-1","display_name":"` + strings.Repeat("d", maxDisplayNameLen+1) + `"}`, wantCode: http.StatusBadRequest, wantBody: `{"error":"` + msgDisplayNameTooLong + `"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo()
			h, _, _ := newTestAuthHandler(repo, selfRegistrationConfig(), nil)
			r := newAuthRouter(h, middleware.MaxBodySize(1024))

			w := postJSON(t, r, "/api/v1/auth/register", tt.body)

			assert.Equal(t, tt.wantCode, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.Equal(t, 0, repo.count())
		})
	}
}

func TestRegisterAcceptsBoundaryValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "3 character username", body: `{"username":"abc","password":"long-enough-1"}`},
		{name: "max length username", body: `{"username":"` + strings.Repeat("a", models.MaxUsernameLen) + `","password":"long-enough-1"}`},
		{name: "username with . _ - @", body: `{"username":"a.b_c-d@e","password":"long-enough-1"}`},
		{name: "8 character password", body: `{"username":"newuser","password":"12345678"}`},
		{name: "72 byte password", body: `{"username":"newuser","password":"` + strings.Repeat("a", maxPasswordBytes) + `"}`},
		{name: "display name", body: `{"username":"newuser","password":"long-enough-1","display_name":"New User"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _, _ := newTestAuthHandler(newMockUserRepo(), selfRegistrationConfig(), nil)
			w := postJSON(t, newAuthRouter(h), "/api/v1/auth/register", tt.body)
			assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		})
	}
}

func TestRegisterRepositoryErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		createErr error
		name      string
		wantBody  string
		wantCode  int
	}{
		{name: "duplicate username", createErr: dberrors.NewDatabaseError("create", dberrors.ErrDuplicateKey), wantCode: http.StatusConflict, wantBody: `{"error":"User already exists"}`},
		{name: "database failure", createErr: errors.New("dial tcp: secret detail"), wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`},
		{name: "write deadline exceeded", createErr: context.DeadlineExceeded, wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo()
			repo.createErr = tt.createErr
			h, _, _ := newTestAuthHandler(repo, selfRegistrationConfig(), nil)

			w := postJSON(t, newAuthRouter(h), "/api/v1/auth/register", `{"username":"newuser","password":"long-enough-1"}`)

			assert.Equal(t, tt.wantCode, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
		})
	}
}

// A client that has already left gets 499 before the bcrypt work.
func TestRegisterCancelledRequest(t *testing.T) {
	t.Parallel()
	repo := newMockUserRepo()
	h, _, _ := newTestAuthHandler(repo, selfRegistrationConfig(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"username":"newuser","password":"long-enough-1"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	newAuthRouter(h).ServeHTTP(w, req)

	assert.Equal(t, statusClientClosedRequest, w.Code)
	assert.Equal(t, 0, repo.creates)
}

// openUserDB returns a SQLite database with the users table.
func openUserDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.User{}))
	return db
}

// A taken username reaches the client as 409 through the real repository.
func TestRegisterDuplicateUsernameOnSQLite(t *testing.T) {
	t.Parallel()
	repo := database.NewGORMUserRepository(openUserDB(t))
	h, _, _ := newTestAuthHandler(repo, selfRegistrationConfig(), nil)
	r := newAuthRouter(h)
	body := `{"username":"newuser","password":"long-enough-1"}`

	require.Equal(t, http.StatusCreated, postJSON(t, r, "/api/v1/auth/register", body).Code)
	w := postJSON(t, r, "/api/v1/auth/register", body)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.JSONEq(t, `{"error":"User already exists"}`, w.Body.String())
}

func bearerGet(t *testing.T, r http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func tokenFor(t *testing.T, userID string) string {
	t.Helper()
	token, err := middleware.GenerateToken(userID, "alice", models.RoleUser, testJWTSecret, time.Hour)
	require.NoError(t, err)
	return token
}

func TestGetCurrentUser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		findErr  error
		mutate   func(r *mockUserRepo)
		name     string
		path     string
		userID   string
		wantBody string
		wantCode int
	}{
		{name: "success", path: "/api/v1/auth/me", userID: "u1", wantCode: http.StatusOK},
		{name: "no user in the context", path: "/api/v1/auth/me-open", wantCode: http.StatusUnauthorized, wantBody: `{"error":"Authentication required"}`},
		{name: "user deleted", path: "/api/v1/auth/me", userID: "gone", wantCode: http.StatusNotFound, wantBody: `{"error":"User not found"}`},
		{name: "user disabled", path: "/api/v1/auth/me", userID: "u1", wantCode: http.StatusUnauthorized, wantBody: `{"error":"Account disabled"}`,
			mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.Disabled = true }) }},
		{name: "repository error", path: "/api/v1/auth/me", userID: "u1", findErr: errors.New("dial tcp: secret detail"),
			wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`},
		{name: "deadline exceeded", path: "/api/v1/auth/me", userID: "u1", findErr: context.DeadlineExceeded,
			wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`},
		{name: "cancelled", path: "/api/v1/auth/me", userID: "u1", findErr: context.Canceled,
			wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo(localUser("u1", "alice"))
			repo.findByIDErr = tt.findErr
			if tt.mutate != nil {
				tt.mutate(repo)
			}
			h, _, _ := newTestAuthHandler(repo, testAuthConfig(), nil)
			token := ""
			if tt.userID != "" {
				token = tokenFor(t, tt.userID)
			}

			w := bearerGet(t, newAuthRouter(h), tt.path, token)

			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			if tt.wantBody != "" {
				assert.JSONEq(t, tt.wantBody, w.Body.String())
				return
			}
			assert.True(t, validateJSONSchema(t, userSchema, w.Body.Bytes()))
			assert.NotContains(t, w.Body.String(), "$2a$")
			var u models.User
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &u))
			assert.Equal(t, "u1", u.ID)
			assert.Equal(t, "alice", u.Username)
		})
	}
}

func TestEnsureAdminUser(t *testing.T) {
	t.Parallel()
	const adminPassword = "Admin-Pass-123"
	tests := []struct {
		lookupErr  error
		createErr  error
		seed       []models.User
		name       string
		password   string
		username   string
		wantErr    string
		wantLog    string
		wantUsers  int
		wantCreate bool
	}{
		{name: "no password: no admin", password: "", wantUsers: 0},
		{name: "creates the admin", password: adminPassword, wantUsers: 1, wantCreate: true, wantLog: "Admin user created"},
		{name: "custom admin username", username: "root-admin", password: adminPassword, wantUsers: 1, wantCreate: true},
		{name: "existing admin is kept", password: adminPassword, wantUsers: 1,
			seed: []models.User{{ID: "a1", Username: "admin", Role: models.RoleAdmin, PasswordHash: "keep-hash", AuthProvider: models.AuthProviderLocal}}},
		{name: "existing non-admin is left alone", password: adminPassword, wantUsers: 1, wantLog: "not an admin",
			seed: []models.User{{ID: "a1", Username: "admin", Role: models.RoleUser, PasswordHash: "keep-hash", AuthProvider: models.AuthProviderLocal}}},
		{name: "existing disabled admin is left alone", password: adminPassword, wantUsers: 1, wantLog: "disabled",
			seed: []models.User{{ID: "a1", Username: "admin", Role: models.RoleAdmin, Disabled: true, PasswordHash: "keep-hash", AuthProvider: models.AuthProviderLocal}}},
		{name: "duplicate key from another replica", password: adminPassword, createErr: dberrors.NewDatabaseError("create", dberrors.ErrDuplicateKey), wantLog: "another replica"},
		{name: "password over 72 bytes", password: strings.Repeat("p", maxPasswordBytes+1), wantErr: "ADMIN_PASSWORD must be at most 72 bytes"},
		{name: "password too short", password: "short12", wantErr: "ADMIN_PASSWORD must be at least 8 characters"},
		{name: "empty username", username: " ", password: adminPassword, wantErr: "ADMIN_USERNAME"},
		{name: "lookup fails", password: adminPassword, lookupErr: errors.New("dial tcp"), wantErr: "look up the admin user"},
		{name: "create fails", password: adminPassword, createErr: errors.New("dial tcp"), wantErr: "create the admin user"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo(tt.seed...)
			repo.findByUsernameErr = tt.lookupErr
			repo.createErr = tt.createErr
			cfg := testAuthConfig()
			cfg.AdminPassword = tt.password
			if tt.username != "" {
				cfg.AdminUsername = tt.username
			}
			h, logs, _ := newTestAuthHandler(repo, cfg, nil)

			err := h.EnsureAdminUser(context.Background())

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				if tt.password != "" {
					assert.NotContains(t, err.Error(), tt.password)
				}
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantUsers, repo.count())
			if tt.password != "" {
				assert.NotContains(t, logs.String(), tt.password)
			}
			if tt.wantLog != "" {
				assert.Contains(t, logs.String(), tt.wantLog)
			}
			for _, s := range tt.seed {
				got := repo.byUsername(t, s.Username)
				assert.Equal(t, s.PasswordHash, got.PasswordHash, "password never overwritten")
				assert.Equal(t, s.Role, got.Role, "role never changed")
			}
			if tt.wantCreate {
				name := cfg.AdminUsername
				admin := repo.byUsername(t, name)
				assert.Equal(t, models.RoleAdmin, admin.Role)
				assert.Equal(t, models.AuthProviderLocal, admin.AuthProvider)
				assert.True(t, admin.ServiceAccount, "break-glass account")
				assert.False(t, admin.Disabled)
				require.NoError(t, bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(tt.password)))
				cost, err := bcrypt.Cost([]byte(admin.PasswordHash))
				require.NoError(t, err)
				assert.Equal(t, passwordHashCost, cost)
			}
		})
	}
}

// A second run keeps the admin and its password, also when ADMIN_PASSWORD
// changed in between.
func TestEnsureAdminUserIsIdempotent(t *testing.T) {
	t.Parallel()
	repo := newMockUserRepo()
	cfg := testAuthConfig()
	cfg.AdminPassword = "First-Pass-123"
	h, _, _ := newTestAuthHandler(repo, cfg, nil)
	require.NoError(t, h.EnsureAdminUser(context.Background()))
	first := repo.byUsername(t, "admin")

	cfg.AdminPassword = "Second-Pass-456"
	require.NoError(t, h.EnsureAdminUser(context.Background()))

	assert.Equal(t, 1, repo.count())
	again := repo.byUsername(t, "admin")
	assert.Equal(t, first.PasswordHash, again.PasswordHash)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(again.PasswordHash), []byte("First-Pass-123")))
}

// Two replicas that start at the same time both succeed and create one admin.
func TestEnsureAdminUserConcurrentReplicas(t *testing.T) {
	t.Parallel()
	repo := database.NewGORMUserRepository(openUserDB(t))
	cfg := testAuthConfig()
	cfg.AdminPassword = "Admin-Pass-123"

	const replicas = 4
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, _, _ := newTestAuthHandler(repo, cfg, nil)
			errs[i] = h.EnsureAdminUser(context.Background())
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		assert.NoError(t, err)
	}
	n, err := repo.Count(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestDummyPasswordHash(t *testing.T) {
	t.Parallel()
	hash := dummyPasswordHash()
	cost, err := bcrypt.Cost(hash)
	require.NoError(t, err)
	assert.Equal(t, passwordHashCost, cost, "same cost as a stored hash")
	assert.GreaterOrEqual(t, passwordHashCost, bcrypt.DefaultCost)
	assert.Equal(t, hash, dummyPasswordHash(), "computed once")
}

type fakeRefreshTokenRepo struct{ models.RefreshTokenRepository }
type fakeSessionStore struct{ sessionstore.SessionStore }
type fakeRevoker struct{ websocket.ClientRevoker }

// The setters accept nil and a value; a login works either way and issues
// only an access token.
func TestAuthHandlerSetters(t *testing.T) {
	t.Parallel()
	for _, set := range []bool{false, true} {
		h, _, _ := newTestAuthHandler(newMockUserRepo(localUser("u1", "alice")), testAuthConfig(), nil)
		h.SetRefreshTokenRepo(nil)
		h.SetSessionStore(nil)
		h.SetWebSocketRevoker(nil)
		assert.Nil(t, h.refreshTokenRepo)
		assert.Nil(t, h.sessionStore)
		assert.Nil(t, h.wsRevoker)
		if set {
			h.SetRefreshTokenRepo(&fakeRefreshTokenRepo{})
			h.SetSessionStore(&fakeSessionStore{})
			h.SetWebSocketRevoker(&fakeRevoker{})
			assert.NotNil(t, h.refreshTokenRepo)
			assert.NotNil(t, h.sessionStore)
			assert.NotNil(t, h.wsRevoker)
		}
		w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, "alice", testPassword))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Values("Set-Cookie"))
	}
}

// NewAuthHandler accepts a nil OIDC config and a nil logger falls back to
// the default logger.
func TestNewAuthHandlerDefaults(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(newMockUserRepo(localUser("u1", "alice")), testAuthConfig(), nil)
	require.NotNil(t, h.throttle)
	assert.NotNil(t, h.log())
	w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, "alice", testPassword))
	assert.Equal(t, http.StatusOK, w.Code)
}

package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const wantInvalidKeyBody = `{"error":"Invalid API key"}`

// keyRepoFake is an in-memory models.APIKeyRepository for the key branch of
// CombinedAuth. Methods the branch does not use panic through the nil
// embedded interface.
type keyRepoFake struct {
	models.APIKeyRepository
	prefixErr      error
	lastUsedErr    error
	lastUsedGate   chan struct{} // when set, UpdateLastUsed waits for it to close
	keys           map[string]*models.APIKey
	lastUsedCtxErr []error
	mu             sync.Mutex
	prefixCalls    int
	lastUsedCalls  int
}

func newKeyRepoFake(keys ...*models.APIKey) *keyRepoFake {
	f := &keyRepoFake{keys: map[string]*models.APIKey{}}
	for _, k := range keys {
		f.keys[k.ID] = k
	}
	return f
}

func (f *keyRepoFake) FindByPrefix(_ context.Context, prefix string) ([]*models.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefixCalls++
	if f.prefixErr != nil {
		return nil, f.prefixErr
	}
	var out []*models.APIKey
	for _, k := range f.keys {
		if k.Prefix == prefix {
			cp := *k
			out = append(out, &cp)
		}
	}
	if len(out) == 0 {
		return nil, dberrors.NewDatabaseError("find_by_prefix", dberrors.ErrNotFound)
	}
	return out, nil
}

func (f *keyRepoFake) UpdateLastUsed(ctx context.Context, _, _ string, _ time.Time) error {
	f.mu.Lock()
	f.lastUsedCalls++
	gate := f.lastUsedGate
	err := f.lastUsedErr
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	f.lastUsedCtxErr = append(f.lastUsedCtxErr, ctx.Err())
	f.mu.Unlock()
	return err
}

func (f *keyRepoFake) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, id)
}

func (f *keyRepoFake) calls() (prefix, lastUsed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prefixCalls, f.lastUsedCalls
}

// userRepoFake is an in-memory models.UserRepository for the key branch.
type userRepoFake struct {
	models.UserRepository
	findErr error
	users   map[string]*models.User
	mu      sync.Mutex
}

func newUserRepoFake(users ...*models.User) *userRepoFake {
	f := &userRepoFake{users: map[string]*models.User{}}
	for _, u := range users {
		f.users[u.ID] = u
	}
	return f
}

func (f *userRepoFake) FindByID(_ context.Context, id string) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findErr != nil {
		return nil, f.findErr
	}
	u, ok := f.users[id]
	if !ok {
		return nil, dberrors.NewDatabaseError("find", dberrors.ErrNotFound)
	}
	cp := *u
	return &cp, nil
}

func (f *userRepoFake) update(id string, fn func(*models.User)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.users[id])
}

// issuedKey creates a key of userID and returns the client form and the record.
func issuedKey(t *testing.T, id, userID string, expires *time.Time) (string, *models.APIKey) {
	t.Helper()
	raw, prefix, hash, err := models.GenerateAPIKey()
	require.NoError(t, err)
	return models.FormatAPIKey(raw), &models.APIKey{ID: id, UserID: userID, Name: "ci", KeyHash: hash, Prefix: prefix, ExpiresAt: expires}
}

func inHours(h int) *time.Time {
	t := time.Now().Add(time.Duration(h) * time.Hour)
	return &t
}

// keyRouter returns a router whose GET /protected sits behind CombinedAuth
// and echoes the context values of the request.
func keyRouter(deps APIKeyAuthDeps) *gin.Engine {
	if deps.JWTSecret == "" {
		deps.JWTSecret = testSecret
	}
	r := gin.New()
	r.GET("/protected", CombinedAuth(deps), func(c *gin.Context) {
		_, hasExpiry := GetTokenExpiryFromContext(c)
		c.JSON(http.StatusOK, gin.H{
			"user_id":    GetUserIDFromContext(c),
			"username":   GetUsernameFromContext(c),
			"role":       GetRoleFromContext(c),
			"method":     GetAuthMethodFromContext(c),
			"key_id":     GetAPIKeyIDFromContext(c),
			"jti":        GetJTIFromContext(c),
			"sid":        GetSessionIDFromContext(c),
			"has_expiry": hasExpiry,
		})
	})
	return r
}

func keyDeps(keys *keyRepoFake, users *userRepoFake) APIKeyAuthDeps {
	return APIKeyAuthDeps{JWTSecret: testSecret, APIKeyRepo: keys, UserRepo: users}
}

func TestCombinedAuthAPIKeySuccess(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	keys := newKeyRepoFake(rec)
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleDevOps})
	r := keyRouter(keyDeps(keys, users))

	w := doGet(r, "/protected", map[string]string{"X-API-Key": client})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"user_id":"u1","username":"alice","role":"devops","method":"api_key","key_id":"k1","jti":"","sid":"","has_expiry":true}`, w.Body.String(),
		"same context keys as the JWT path, an auth-method marker, and no jti or session id")
	assert.NotContains(t, w.Body.String(), client)

	t.Run("header name is case-insensitive", func(t *testing.T) {
		t.Parallel()
		w := doGet(r, "/protected", map[string]string{"x-api-key": client})
		assert.Equal(t, http.StatusOK, w.Code)
	})
	t.Run("another Authorization scheme does not stop the key", func(t *testing.T) {
		t.Parallel()
		w := doGet(r, "/protected", map[string]string{"Authorization": "Basic dXNlcjpwYXNz", "X-API-Key": client})
		assert.Equal(t, http.StatusOK, w.Code)
	})
	t.Run("a key without expiry authenticates", func(t *testing.T) {
		t.Parallel()
		c2, rec2 := issuedKey(t, "k2", "u1", nil)
		r2 := keyRouter(keyDeps(newKeyRepoFake(rec2), users))
		w := doGet(r2, "/protected", map[string]string{"X-API-Key": c2})
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"has_expiry":false`)
	})
}

func TestCombinedAuthAPIKeyRefusals(t *testing.T) {
	t.Parallel()
	goodClient, goodRec := issuedKey(t, "good", "u1", inHours(24))
	expiredClient, expiredRec := issuedKey(t, "expired", "u1", func() *time.Time { x := time.Now().Add(-time.Second); return &x }())
	disabledClient, disabledRec := issuedKey(t, "dis", "u2", inHours(24))
	orphanClient, orphanRec := issuedKey(t, "orphan", "nobody", inHours(24))
	oddRoleClient, oddRoleRec := issuedKey(t, "odd", "u3", inHours(24))
	// Two keys that share a prefix: only the one whose full hash matches wins.
	twinClient, twinRec := issuedKey(t, "twin", "u1", inHours(24))
	other := *twinRec
	other.ID = "twin-other"
	other.KeyHash = models.HashAPIKey(strings.Repeat("0", 64))
	otherPrefixTail := models.FormatAPIKey(twinRec.Prefix + strings.Repeat("0", 64-models.APIKeyPrefixLen))

	keys := newKeyRepoFake(goodRec, expiredRec, disabledRec, orphanRec, oddRoleRec, twinRec, &other)
	users := newUserRepoFake(
		&models.User{ID: "u1", Username: "alice", Role: models.RoleUser},
		&models.User{ID: "u2", Username: "bob", Role: models.RoleUser, Disabled: true},
		&models.User{ID: "u3", Username: "carol", Role: "superuser"},
	)
	r := keyRouter(keyDeps(keys, users))

	unknownClient := models.FormatAPIKey(strings.Repeat("ab", 32))

	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{name: "unknown key", headers: map[string]string{"X-API-Key": unknownClient}, want: wantInvalidKeyBody},
		{name: "expired key", headers: map[string]string{"X-API-Key": expiredClient}, want: wantInvalidKeyBody},
		{name: "owner disabled", headers: map[string]string{"X-API-Key": disabledClient}, want: wantInvalidKeyBody},
		{name: "owner deleted", headers: map[string]string{"X-API-Key": orphanClient}, want: wantInvalidKeyBody},
		{name: "owner has an unknown role", headers: map[string]string{"X-API-Key": oddRoleClient}, want: wantInvalidKeyBody},
		{name: "same prefix, other tail", headers: map[string]string{"X-API-Key": otherPrefixTail}, want: wantInvalidKeyBody},
		{name: "no sk_ prefix", headers: map[string]string{"X-API-Key": strings.TrimPrefix(goodClient, "sk_")}, want: wantInvalidKeyBody},
		{name: "wrong length", headers: map[string]string{"X-API-Key": goodClient[:len(goodClient)-1]}, want: wantInvalidKeyBody},
		{name: "upper-case", headers: map[string]string{"X-API-Key": "sk_" + strings.ToUpper(strings.TrimPrefix(goodClient, "sk_"))}, want: wantInvalidKeyBody},
		{name: "empty value", headers: map[string]string{"X-API-Key": ""}, want: wantInvalidKeyBody},
		{name: "garbage", headers: map[string]string{"X-API-Key": "not a key"}, want: wantInvalidKeyBody},
		{name: "no credentials", headers: nil, want: `{"error":"Authorization required"}`},
		{name: "key in the Authorization header", headers: map[string]string{"Authorization": "ApiKey " + goodClient}, want: `{"error":"Authorization required"}`},
		{name: "Basic scheme with the key as password", headers: map[string]string{"Authorization": "Basic " + goodClient}, want: `{"error":"Authorization required"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := doGet(r, "/protected", tt.headers)
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.JSONEq(t, tt.want, w.Body.String(), "one generic body for every reason")
			assert.NotContains(t, w.Body.String(), goodClient)
		})
	}

	t.Run("a twin with the same prefix does not hide the right key", func(t *testing.T) {
		t.Parallel()
		w := doGet(r, "/protected", map[string]string{"X-API-Key": twinClient})
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"key_id":"twin"`)
	})

	t.Run("the key in the query string is ignored", func(t *testing.T) {
		t.Parallel()
		for _, q := range []string{"api_key", "apikey", "key", "x-api-key", "X-API-Key"} {
			w := doGet(r, "/protected?"+q+"="+goodClient, nil)
			assert.Equal(t, http.StatusUnauthorized, w.Code, q)
			assert.JSONEq(t, `{"error":"Authorization required"}`, w.Body.String(), q)
		}
	})

	t.Run("two X-API-Key headers are refused", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.Header.Add("X-API-Key", goodClient)
		req.Header.Add("X-API-Key", goodClient)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.JSONEq(t, wantInvalidKeyBody, w.Body.String())
	})
}

// TestCombinedAuthBearerAndKeyTogether pins the rule: a request that carries
// a Bearer Authorization header and an X-API-Key header is refused with 400
// before either credential is checked, whatever either of them is.
func TestCombinedAuthBearerAndKeyTogether(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	keys := newKeyRepoFake(rec)
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	r := keyRouter(keyDeps(keys, users))
	jwt, err := GenerateToken("u1", "alice", "user", testSecret, time.Hour)
	require.NoError(t, err)

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "valid JWT and valid key", headers: map[string]string{"Authorization": "Bearer " + jwt, "X-API-Key": client}},
		{name: "garbage JWT and valid key", headers: map[string]string{"Authorization": "Bearer garbage", "X-API-Key": client}},
		{name: "valid JWT and garbage key", headers: map[string]string{"Authorization": "Bearer " + jwt, "X-API-Key": "garbage"}},
		{name: "valid JWT and empty key header", headers: map[string]string{"Authorization": "Bearer " + jwt, "X-API-Key": ""}},
		{name: "lower-case scheme", headers: map[string]string{"Authorization": "bearer " + jwt, "X-API-Key": client}},
		{name: "scheme without a token", headers: map[string]string{"Authorization": "Bearer", "X-API-Key": client}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := doGet(r, "/protected", tt.headers)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.JSONEq(t, `{"error":"Send either an Authorization Bearer token or an X-API-Key header, not both"}`, w.Body.String())
		})
	}
	prefixCalls, _ := keys.calls()
	assert.Zero(t, prefixCalls, "neither credential is looked up")

	t.Run("a Bearer request alone is still the JWT path", func(t *testing.T) {
		t.Parallel()
		w := doGet(r, "/protected", map[string]string{"Authorization": "Bearer " + jwt})
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"method":"jwt"`)
	})
}

func TestCombinedAuthAPIKeyRevocationIsImmediate(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	keys := newKeyRepoFake(rec)
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	r := keyRouter(keyDeps(keys, users))
	h := map[string]string{"X-API-Key": client}

	require.Equal(t, http.StatusOK, doGet(r, "/protected", h).Code)
	keys.remove("k1")
	w := doGet(r, "/protected", h)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "a deleted key stops working at once, no cache")
	assert.JSONEq(t, wantInvalidKeyBody, w.Body.String())

	// The owner is disabled after a successful request.
	client2, rec2 := issuedKey(t, "k2", "u1", inHours(24))
	keys2 := newKeyRepoFake(rec2)
	r2 := keyRouter(keyDeps(keys2, users))
	require.Equal(t, http.StatusOK, doGet(r2, "/protected", map[string]string{"X-API-Key": client2}).Code)
	users.update("u1", func(u *models.User) { u.Disabled = true })
	assert.Equal(t, http.StatusUnauthorized, doGet(r2, "/protected", map[string]string{"X-API-Key": client2}).Code)
}

// TestCombinedAuthAPIKeyRoleIsTheOwnersCurrentRole: a key has no role of its
// own, so it can never exceed its owner's role, and a demoted owner's keys are
// demoted with the owner.
func TestCombinedAuthAPIKeyRoleIsTheOwnersCurrentRole(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	keys := newKeyRepoFake(rec)
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleAdmin})
	r := keyRouter(keyDeps(keys, users))
	h := map[string]string{"X-API-Key": client}

	assert.Contains(t, doGet(r, "/protected", h).Body.String(), `"role":"admin"`)
	users.update("u1", func(u *models.User) { u.Role = models.RoleUser })
	assert.Contains(t, doGet(r, "/protected", h).Body.String(), `"role":"user"`, "the demotion applies to the existing key")

	// A role that a client tries to send does not matter.
	w := doGet(r, "/protected", map[string]string{"X-API-Key": client, "X-Role": "admin", "X-User-ID": "u9"})
	assert.Contains(t, w.Body.String(), `"role":"user"`)
	assert.Contains(t, w.Body.String(), `"user_id":"u1"`)

	// The role gate sees the owner's role.
	gate := gin.New()
	gate.GET("/admin", CombinedAuth(keyDeps(keys, users)), RequireAdmin(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	assert.Equal(t, http.StatusForbidden, doGet(gate, "/admin", h).Code)
	users.update("u1", func(u *models.User) { u.Role = models.RoleAdmin })
	assert.Equal(t, http.StatusNoContent, doGet(gate, "/admin", h).Code)
}

func TestCombinedAuthAPIKeyStoreErrors(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	user := &models.User{ID: "u1", Username: "alice", Role: models.RoleUser}
	dbErr := dberrors.NewDatabaseError("find", errors.New("dial tcp 10.0.0.5:3306: connection refused"))

	tests := []struct {
		name       string
		keyErr     error
		userErr    error
		ctxCancel  bool
		failClosed bool
		wantCode   int
		wantBody   string
	}{
		{name: "key lookup DB error, strict", keyErr: dbErr, failClosed: true, wantCode: 500, wantBody: `{"error":"Internal server error"}`},
		{name: "key lookup DB error, lenient", keyErr: dbErr, wantCode: 401, wantBody: wantInvalidKeyBody},
		{name: "user lookup DB error, strict", userErr: dbErr, failClosed: true, wantCode: 500, wantBody: `{"error":"Internal server error"}`},
		{name: "user lookup DB error, lenient", userErr: dbErr, wantCode: 401, wantBody: wantInvalidKeyBody},
		{name: "key lookup deadline, strict", keyErr: dberrors.NewDatabaseError("find", context.DeadlineExceeded), failClosed: true, wantCode: 500, wantBody: `{"error":"Internal server error"}`},
		{name: "key lookup deadline, lenient", keyErr: dberrors.NewDatabaseError("find", context.DeadlineExceeded), wantCode: 500, wantBody: `{"error":"Internal server error"}`},
		{name: "user lookup deadline, lenient", userErr: context.DeadlineExceeded, wantCode: 500, wantBody: `{"error":"Internal server error"}`},
		{name: "key lookup cancelled, strict", keyErr: context.Canceled, failClosed: true, wantCode: 499, wantBody: `{"error":"Client closed request"}`},
		{name: "key lookup cancelled, lenient", keyErr: context.Canceled, wantCode: 499, wantBody: `{"error":"Client closed request"}`},
		{name: "user lookup cancelled, lenient", userErr: context.Canceled, wantCode: 499, wantBody: `{"error":"Client closed request"}`},
		{name: "cancelled request context", ctxCancel: true, wantCode: 499, wantBody: `{"error":"Client closed request"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			keys := newKeyRepoFake(rec)
			keys.prefixErr = tt.keyErr
			users := newUserRepoFake(user)
			users.findErr = tt.userErr
			deps := keyDeps(keys, users)
			deps.FailClosedOnStoreError = tt.failClosed
			r := keyRouter(deps)

			ctx := context.Background()
			if tt.ctxCancel {
				// The fake repository ignores the context, so make the
				// lookup itself report the cancellation like a driver does.
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				keys.prefixErr = ctx.Err()
			}
			w := doGetCtx(ctx, r, "/protected", map[string]string{"X-API-Key": client})
			assert.Equal(t, tt.wantCode, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.NotContains(t, w.Body.String(), "10.0.0.5", "no internal error text")
			assert.NotContains(t, w.Body.String(), client)
		})
	}
}

// TestCombinedAuthAPIKeyIgnoresTheSessionStore: a key does not use the JWT
// blocklists (revoking a key is deleting it), so a failing store changes
// nothing for it in either mode.
func TestCombinedAuthAPIKeyIgnoresTheSessionStore(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	memory := sessionstore.NewMemory()
	t.Cleanup(memory.Stop)
	for _, strict := range []bool{false, true} {
		deps := keyDeps(newKeyRepoFake(rec), users)
		deps.SessionStore = failingStore{SessionStore: memory, err: assert.AnError}
		deps.FailClosedOnStoreError = strict
		w := doGet(keyRouter(deps), "/protected", map[string]string{"X-API-Key": client})
		assert.Equal(t, http.StatusOK, w.Code, "strict=%v", strict)
	}
}

func TestCombinedAuthAPIKeyLastUsed(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})

	t.Run("written once per key per minute", func(t *testing.T) {
		t.Parallel()
		keys := newKeyRepoFake(rec)
		mw := NewAPIKeyAuthenticator(keyDeps(keys, users))
		r := keyRouterFor(mw)
		for i := 0; i < 5; i++ {
			require.Equal(t, http.StatusOK, doGet(r, "/protected", map[string]string{"X-API-Key": client}).Code)
		}
		mw.lastUsed.wait()
		_, n := keys.calls()
		assert.Equal(t, 1, n)
		for _, e := range keys.lastUsedCtxErr {
			assert.NoError(t, e, "the write ran with a live context")
		}
	})

	t.Run("written again after the interval", func(t *testing.T) {
		t.Parallel()
		keys := newKeyRepoFake(rec)
		mw := NewAPIKeyAuthenticator(keyDeps(keys, users))
		clock := time.Now()
		mw.lastUsed.now = func() time.Time { return clock }
		r := keyRouterFor(mw)
		require.Equal(t, http.StatusOK, doGet(r, "/protected", map[string]string{"X-API-Key": client}).Code)
		clock = clock.Add(2 * time.Minute)
		require.Equal(t, http.StatusOK, doGet(r, "/protected", map[string]string{"X-API-Key": client}).Code)
		mw.lastUsed.wait()
		_, n := keys.calls()
		assert.Equal(t, 2, n)
	})

	t.Run("a failed write never fails the request", func(t *testing.T) {
		t.Parallel()
		for _, werr := range []error{
			dberrors.NewDatabaseError("update_last_used", dberrors.ErrNotFound),
			errors.New("connection reset"),
			context.DeadlineExceeded,
		} {
			keys := newKeyRepoFake(rec)
			keys.lastUsedErr = werr
			mw := NewAPIKeyAuthenticator(keyDeps(keys, users))
			w := doGet(keyRouterFor(mw), "/protected", map[string]string{"X-API-Key": client})
			mw.lastUsed.wait()
			assert.Equal(t, http.StatusOK, w.Code, "%v", werr)
		}
	})

	t.Run("a stalled write does not hold the request", func(t *testing.T) {
		t.Parallel()
		keys := newKeyRepoFake(rec)
		keys.lastUsedGate = make(chan struct{})
		mw := NewAPIKeyAuthenticator(keyDeps(keys, users))
		done := make(chan int, 1)
		go func() { done <- doGet(keyRouterFor(mw), "/protected", map[string]string{"X-API-Key": client}).Code }()
		select {
		case code := <-done:
			assert.Equal(t, http.StatusOK, code)
		case <-time.After(5 * time.Second):
			t.Fatal("the request waited for the last-used write")
		}
		close(keys.lastUsedGate)
		mw.lastUsed.wait()
	})

	t.Run("a client that leaves does not cancel the write", func(t *testing.T) {
		t.Parallel()
		keys := newKeyRepoFake(rec)
		keys.lastUsedGate = make(chan struct{})
		mw := NewAPIKeyAuthenticator(keyDeps(keys, users))
		ctx, cancel := context.WithCancel(context.Background())
		require.Equal(t, http.StatusOK, doGetCtx(ctx, keyRouterFor(mw), "/protected", map[string]string{"X-API-Key": client}).Code)
		cancel()
		close(keys.lastUsedGate)
		mw.lastUsed.wait()
		require.Len(t, keys.lastUsedCtxErr, 1)
		assert.NoError(t, keys.lastUsedCtxErr[0])
	})
}

func keyRouterFor(a *APIKeyAuthenticator) *gin.Engine {
	r := gin.New()
	r.GET("/protected", a.middleware(AuthRequiredWithOptions(JWTAuthOptions{JWTSecret: testSecret})), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": GetUserIDFromContext(c)})
	})
	return r
}

func TestLastUsedUpdaterBounds(t *testing.T) {
	t.Parallel()

	t.Run("the table of keys stays bounded", func(t *testing.T) {
		t.Parallel()
		repo := newKeyRepoFake()
		u := newLastUsedUpdater()
		clock := time.Now()
		u.now = func() time.Time { return clock }
		u.mu.Lock()
		for i := 0; i < lastUsedMaxEntries; i++ {
			u.seen[fmt.Sprintf("k%d", i)] = clock
		}
		u.mu.Unlock()
		clock = clock.Add(2 * lastUsedInterval)
		u.touch(context.Background(), repo, "u", "fresh")
		u.wait()
		u.mu.Lock()
		size := len(u.seen)
		u.mu.Unlock()
		assert.Equal(t, 1, size, "the old entries are pruned, the fresh one stays")
		_, n := repo.calls()
		assert.Equal(t, 1, n)
	})

	t.Run("a full table skips the write and never blocks", func(t *testing.T) {
		t.Parallel()
		repo := newKeyRepoFake()
		u := newLastUsedUpdater()
		clock := time.Now()
		u.now = func() time.Time { return clock }
		u.mu.Lock()
		for i := 0; i < lastUsedMaxEntries; i++ {
			u.seen[fmt.Sprintf("k%d", i)] = clock
		}
		u.mu.Unlock()
		u.touch(context.Background(), repo, "u", "new")
		u.wait()
		_, n := repo.calls()
		assert.Zero(t, n)
	})

	t.Run("no more than the allowed writes run at once", func(t *testing.T) {
		t.Parallel()
		repo := newKeyRepoFake()
		repo.lastUsedGate = make(chan struct{})
		u := newLastUsedUpdater()
		for i := 0; i < lastUsedMaxInFlight*3; i++ {
			u.touch(context.Background(), repo, "u", fmt.Sprintf("k%d", i))
		}
		time.Sleep(50 * time.Millisecond)
		_, n := repo.calls()
		assert.Equal(t, lastUsedMaxInFlight, n)
		close(repo.lastUsedGate)
		u.wait()
	})

	t.Run("a nil updater and a nil repository do nothing", func(t *testing.T) {
		t.Parallel()
		var u *lastUsedUpdater
		u.touch(context.Background(), nil, "u", "k")
		u.wait()
		newLastUsedUpdater().touch(context.Background(), nil, "u", "k")
	})
}

// TestAPIKeyAuthLogsNeverContainTheKey runs failing requests and a failing
// last-used write and checks that no log line has the key.
func TestAPIKeyAuthLogsNeverContainTheKey(t *testing.T) {
	var buf syncBuf
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	raw := strings.TrimPrefix(client, "sk_")
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	keys := newKeyRepoFake(rec)
	keys.lastUsedErr = errors.New("update failed")
	mw := NewAPIKeyAuthenticator(keyDeps(keys, users))
	r := keyRouterFor(mw)
	require.Equal(t, http.StatusOK, doGet(r, "/protected", map[string]string{"X-API-Key": client}).Code)
	mw.lastUsed.wait()

	failing := newKeyRepoFake(rec)
	failing.prefixErr = errors.New("db down")
	assert.Equal(t, http.StatusUnauthorized, doGet(keyRouterFor(NewAPIKeyAuthenticator(keyDeps(failing, users))), "/protected", map[string]string{"X-API-Key": client}).Code)
	assert.Equal(t, http.StatusUnauthorized, doGet(r, "/protected", map[string]string{"X-API-Key": "sk_" + strings.Repeat("c", 64)}).Code)

	logs := buf.String()
	assert.NotEmpty(t, logs, "the failures are logged")
	assert.NotContains(t, logs, raw)
	assert.NotContains(t, logs, rec.KeyHash)
	assert.NotContains(t, logs, strings.Repeat("c", 64))
	assert.Contains(t, logs, "k1", "the key ID is logged")
}

type syncBuf struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRequireSessionAuth(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleAdmin})
	var hits atomic.Int32
	r := gin.New()
	r.POST("/session-only", CombinedAuth(keyDeps(newKeyRepoFake(rec), users)), RequireSessionAuth(), func(c *gin.Context) {
		hits.Add(1)
		c.Status(http.StatusNoContent)
	})
	jwt, err := GenerateToken("u1", "alice", "admin", testSecret, time.Hour)
	require.NoError(t, err)

	post := func(h map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/session-only", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := post(map[string]string{"X-API-Key": client})
	assert.Equal(t, http.StatusForbidden, w.Code, "an API key is not a login session")
	assert.JSONEq(t, `{"error":"This action requires a login session"}`, w.Body.String())
	assert.Zero(t, hits.Load())

	assert.Equal(t, http.StatusNoContent, post(map[string]string{"Authorization": "Bearer " + jwt}).Code)
	assert.Equal(t, int32(1), hits.Load())

	// Without any auth middleware the request has no method: pass (the route
	// has its own auth), do not break open APIs.
	open := gin.New()
	open.POST("/x", RequireSessionAuth(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w2 := httptest.NewRecorder()
	open.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/x", nil))
	assert.Equal(t, http.StatusNoContent, w2.Code)
}

func TestAPIKeyContextGetters(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.Empty(t, GetAPIKeyIDFromContext(c))
	c.Set(contextKeyAPIKeyID, 5)
	assert.Empty(t, GetAPIKeyIDFromContext(c), "a value of the wrong type reads as empty")
	c.Set(contextKeyAPIKeyID, "k1")
	assert.Equal(t, "k1", GetAPIKeyIDFromContext(c))
}

func TestAPIKeyAuthenticatorDirect(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	a := NewAPIKeyAuthenticator(keyDeps(newKeyRepoFake(rec), users))

	p, fail := a.Authenticate(context.Background(), client)
	require.Nil(t, fail)
	assert.Equal(t, "k1", p.Key.ID)
	assert.Equal(t, "u1", p.User.ID)
	assert.Equal(t, models.RoleUser, p.Role)

	p, fail = a.Authenticate(context.Background(), "sk_"+strings.Repeat("d", 64))
	require.Nil(t, p)
	require.NotNil(t, fail)
	assert.Equal(t, http.StatusUnauthorized, fail.Status)
	assert.Equal(t, "Invalid API key", fail.Message)
}

// ownerReadTimeRepo records when the owner was read.
type ownerReadTimeRepo struct {
	*userRepoFake
	readAt atomic.Pointer[time.Time]
}

func (r *ownerReadTimeRepo) FindByID(ctx context.Context, id string) (*models.User, error) {
	now := time.Now()
	r.readAt.Store(&now)
	return r.userRepoFake.FindByID(ctx, id)
}

// TestAPIKeyIssueTimeIsStampedBeforeTheOwnerRead: a socket opened with a key is
// covered by a user block that is written after the owner was read, so the
// issue time of the key credential must not be later than that read.
func TestAPIKeyIssueTimeIsStampedBeforeTheOwnerRead(t *testing.T) {
	t.Parallel()
	client, rec := issuedKey(t, "k1", "u1", nil)
	users := &ownerReadTimeRepo{userRepoFake: newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})}
	var issued time.Time
	var ok bool
	r := gin.New()
	r.GET("/protected", CombinedAuth(APIKeyAuthDeps{JWTSecret: testSecret, APIKeyRepo: newKeyRepoFake(rec), UserRepo: users}), func(c *gin.Context) {
		issued, ok = GetTokenIssuedAtFromContext(c)
		c.Status(http.StatusNoContent)
	})

	before := time.Now()
	w := doGet(r, "/protected", map[string]string{"X-API-Key": client})

	require.Equal(t, http.StatusNoContent, w.Code)
	require.True(t, ok, "a key request has an issue time")
	readAt := users.readAt.Load()
	require.NotNil(t, readAt)
	assert.False(t, issued.After(*readAt), "issued %v after the owner read %v", issued, *readAt)
	assert.False(t, issued.Before(before.Truncate(time.Millisecond)), "and not before the request")
}

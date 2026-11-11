package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/database"
	"backend/internal/models"
	"backend/internal/websocket"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	keyTestMaxActive   = 3
	keyTestMaxLifetime = 90
)

// apiKeyRevoker records DisconnectAPIKey calls (the other methods are not
// used by the key handler).
type apiKeyRevoker struct {
	keys []string
	mu   sync.Mutex
}

func (r *apiKeyRevoker) DisconnectUser(string) int  { return 0 }
func (r *apiKeyRevoker) DisconnectToken(string) int { return 0 }
func (r *apiKeyRevoker) DisconnectAPIKey(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, id)
	return 1
}

func (r *apiKeyRevoker) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

// failingAPIKeyRepo wraps a repository and fails the listed operations.
type failingAPIKeyRepo struct {
	models.APIKeyRepository
	err error
	ops map[string]bool
}

func (f failingAPIKeyRepo) fail(op string) bool { return f.ops[op] }

func (f failingAPIKeyRepo) CreateLimited(ctx context.Context, k *models.APIKey, n int, now time.Time) error {
	if f.fail("create") {
		return f.err
	}
	return f.APIKeyRepository.CreateLimited(ctx, k, n, now)
}

func (f failingAPIKeyRepo) ListPaged(ctx context.Context, fl models.APIKeyFilter, l, o int) ([]*models.APIKey, int64, error) {
	if f.fail("list") {
		return nil, 0, f.err
	}
	return f.APIKeyRepository.ListPaged(ctx, fl, l, o)
}

func (f failingAPIKeyRepo) FindByIDs(ctx context.Context, ids []string) ([]*models.APIKey, error) {
	if f.fail("find") {
		return nil, f.err
	}
	return f.APIKeyRepository.FindByIDs(ctx, ids)
}

func (f failingAPIKeyRepo) Delete(ctx context.Context, u, id string) error {
	if f.fail("delete") {
		return f.err
	}
	return f.APIKeyRepository.Delete(ctx, u, id)
}

// keyEnv is a router over real repositories on SQLite: CombinedAuth, the key
// endpoints and a /whoami route behind CombinedAuth.
type keyEnv struct {
	db      *gorm.DB
	keys    *database.GORMAPIKeyRepository
	users   *database.GORMUserRepository
	revoker *apiKeyRevoker
	router  *gin.Engine
	handler *APIKeyHandler
}

var keyTestUsers = []models.User{
	{ID: "u-alice", Username: "alice", Role: models.RoleUser},
	{ID: "u-bob", Username: "bob", Role: models.RoleUser},
	{ID: "u-dev", Username: "dev", Role: models.RoleDevOps},
	{ID: "u-root", Username: "root", Role: models.RoleAdmin},
	{ID: "u-off", Username: "off", Role: models.RoleUser, Disabled: true},
}

func newKeyEnv(t *testing.T, wrap func(models.APIKeyRepository) models.APIKeyRepository) *keyEnv {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.APIKey{}))

	env := &keyEnv{db: db, keys: database.NewGORMAPIKeyRepository(db), users: database.NewGORMUserRepository(db), revoker: &apiKeyRevoker{}}
	for i := range keyTestUsers {
		u := keyTestUsers[i]
		require.NoError(t, env.users.Create(context.Background(), &u))
		if u.Disabled {
			require.NoError(t, db.Model(&models.User{}).Where("id = ?", u.ID).Update("disabled", true).Error)
		}
	}

	var repo models.APIKeyRepository = env.keys
	if wrap != nil {
		repo = wrap(repo)
	}
	env.handler = NewAPIKeyHandler(repo, env.users, APIKeyOptions{MaxLifetimeDays: keyTestMaxLifetime, MaxActivePerUser: keyTestMaxActive})
	env.handler.SetWebSocketRevoker(env.revoker)

	r := gin.New()
	r.Use(middleware.CombinedAuth(middleware.APIKeyAuthDeps{JWTSecret: testJWTSecret, APIKeyRepo: env.keys, UserRepo: env.users}))
	r.POST("/api/v1/api-keys", env.handler.CreateAPIKey)
	r.GET("/api/v1/api-keys", env.handler.ListAPIKeys)
	r.DELETE("/api/v1/api-keys/:id", env.handler.DeleteAPIKey)
	r.GET("/whoami", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": middleware.GetUserIDFromContext(c), "role": middleware.GetRoleFromContext(c), "method": middleware.GetAuthMethodFromContext(c)})
	})
	env.router = r
	return env
}

// jwtFor returns the headers of a JWT of the test user.
func jwtFor(t *testing.T, userID string) map[string]string {
	t.Helper()
	for _, u := range keyTestUsers {
		if u.ID == userID {
			tok, err := middleware.GenerateToken(u.ID, u.Username, u.Role, testJWTSecret, time.Hour)
			require.NoError(t, err)
			return map[string]string{"Authorization": "Bearer " + tok}
		}
	}
	tok, err := middleware.GenerateToken(userID, "ghost", models.RoleUser, testJWTSecret, time.Hour)
	require.NoError(t, err)
	return map[string]string{"Authorization": "Bearer " + tok}
}

func (e *keyEnv) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
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

type createdKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	RawKey    string     `json:"raw_key"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// create posts a key as the user and returns the decoded answer.
func (e *keyEnv) create(t *testing.T, userID, body string) createdKey {
	t.Helper()
	w := e.do(http.MethodPost, "/api/v1/api-keys", body, jwtFor(t, userID))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var k createdKey
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &k))
	return k
}

func (e *keyEnv) countKeys(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, e.db.Model(&models.APIKey{}).Count(&n).Error)
	return n
}

// seedKey stores a key of userID directly (no limit, any expiry).
func (e *keyEnv) seedKey(t *testing.T, id, userID string, expires *time.Time) string {
	t.Helper()
	raw, prefix, hash, err := models.GenerateAPIKey()
	require.NoError(t, err)
	require.NoError(t, e.keys.Create(context.Background(), &models.APIKey{ID: id, UserID: userID, Name: "seed " + id, KeyHash: hash, Prefix: prefix, ExpiresAt: expires}))
	return models.FormatAPIKey(raw)
}

const apiKeyCreatedSchema = `{
  "type": "object",
  "required": ["id", "name", "prefix", "raw_key", "created_at", "expires_at"],
  "properties": {
    "id": {"type": "string", "minLength": 1},
    "name": {"type": "string"},
    "prefix": {"type": "string", "pattern": "^[0-9a-f]{16}$"},
    "raw_key": {"type": "string", "pattern": "^sk_[0-9a-f]{64}$"},
    "created_at": {"type": "string"},
    "expires_at": {"type": "string"}
  },
  "additionalProperties": false
}`

const apiKeyListSchema = `{
  "type": "object",
  "required": ["data", "total", "page", "pageSize"],
  "properties": {
    "data": {"type": "array", "items": {
      "type": "object",
      "required": ["id", "user_id", "name", "prefix", "created_at"],
      "properties": {"id": {"type": "string"}, "user_id": {"type": "string"}, "name": {"type": "string"}, "prefix": {"type": "string"},
        "created_at": {"type": "string"}, "last_used_at": {"type": "string"}, "expires_at": {"type": "string"}},
      "additionalProperties": false
    }},
    "total": {"type": "integer"}, "page": {"type": "integer"}, "pageSize": {"type": "integer"}
  }
}`

func assertSchema(t *testing.T, schema, body string) {
	t.Helper()
	res, err := gojsonschema.Validate(gojsonschema.NewStringLoader(schema), gojsonschema.NewStringLoader(body))
	require.NoError(t, err)
	assert.True(t, res.Valid(), "%v\n%s", res.Errors(), body)
}

// captureDefaultLogs sends the default slog logger to buf and returns the
// function that restores it. The caller must not run in parallel with other
// tests (it changes a global).
func captureDefaultLogs(buf *syncBuffer) (restore func()) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(old) }
}

func errText(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	return m["error"]
}

func TestAPIKeyCreateByRole(t *testing.T) {
	t.Parallel()
	for _, user := range []struct{ id, role string }{{"u-alice", models.RoleUser}, {"u-dev", models.RoleDevOps}, {"u-root", models.RoleAdmin}} {
		user := user
		t.Run(user.role, func(t *testing.T) {
			t.Parallel()
			env := newKeyEnv(t, nil)
			w := env.do(http.MethodPost, "/api/v1/api-keys", `{"name":"ci runner","expires_in_days":30}`, jwtFor(t, user.id))
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			assertSchema(t, apiKeyCreatedSchema, w.Body.String())
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), "the answer carries the only copy of the key")
			assert.Equal(t, "no-cache", w.Header().Get("Pragma"))

			var k createdKey
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &k))
			assert.Equal(t, "ci runner", k.Name)
			assert.Equal(t, k.RawKey[len("sk_"):len("sk_")+models.APIKeyPrefixLen], k.Prefix)
			require.NotNil(t, k.ExpiresAt)
			assert.WithinDuration(t, time.Now().AddDate(0, 0, 30), *k.ExpiresAt, time.Minute)

			// Only the hash is stored: no column holds the key.
			stored, err := env.keys.FindByID(t.Context(), user.id, k.ID)
			require.NoError(t, err)
			raw := strings.TrimPrefix(k.RawKey, "sk_")
			assert.Equal(t, models.HashAPIKey(raw), stored.KeyHash)
			assert.Equal(t, user.id, stored.UserID, "the owner is the caller")
			var rows []map[string]any
			require.NoError(t, env.db.Raw("SELECT * FROM api_keys").Scan(&rows).Error)
			require.NotEmpty(t, rows)
			assert.NotContains(t, fmt.Sprint(rows), raw)

			// The new key authenticates, with the owner's role.
			who := env.do(http.MethodGet, "/whoami", "", map[string]string{"X-API-Key": k.RawKey})
			require.Equal(t, http.StatusOK, who.Code)
			assert.JSONEq(t, fmt.Sprintf(`{"user_id":%q,"role":%q,"method":"api_key"}`, user.id, user.role), who.Body.String())
		})
	}
}

// TestAPIKeyRawKeyShownOnce: the raw key is in the POST answer only. It is
// never in the list, in an error, in the logs or in the JSON of the model.
func TestAPIKeyRawKeyShownOnce(t *testing.T) {
	env := newKeyEnv(t, nil)
	var logs syncBuffer
	restore := captureDefaultLogs(&logs)
	defer restore()

	k := env.create(t, "u-alice", `{"name":"ci","expires_in_days":10}`)
	raw := strings.TrimPrefix(k.RawKey, "sk_")

	for _, path := range []string{"/api/v1/api-keys", "/api/v1/api-keys?pageSize=100", "/api/v1/api-keys?all=true"} {
		for _, user := range []string{"u-alice", "u-root"} {
			w := env.do(http.MethodGet, path, "", jwtFor(t, user))
			assert.NotContains(t, w.Body.String(), raw, path)
			assert.NotContains(t, w.Body.String(), "raw_key", path)
			assert.NotContains(t, w.Body.String(), "key_hash", path)
			assert.NotContains(t, w.Body.String(), models.HashAPIKey(raw), path)
		}
	}
	del := env.do(http.MethodDelete, "/api/v1/api-keys/"+k.ID, "", jwtFor(t, "u-alice"))
	assert.Equal(t, http.StatusNoContent, del.Code)
	assert.Empty(t, del.Body.String())

	out := logs.String()
	assert.NotContains(t, out, raw, "the key is never logged")
	assert.NotContains(t, out, models.HashAPIKey(raw))
	assert.Contains(t, out, k.ID, "creation and deletion are logged with the key ID")
	assert.Contains(t, out, "u-alice", "and the owner ID")
	assert.NotContains(t, out, "ci runner")
}

func TestAPIKeyCreateValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	day := func(n int) string { return now.AddDate(0, 0, n).Format("2006-01-02") }
	name300 := strings.Repeat("n", 300)

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{name: "ok: expires_in_days", body: `{"name":"ok","expires_in_days":1}`, wantCode: 201},
		{name: "ok: the maximum lifetime", body: fmt.Sprintf(`{"name":"ok","expires_in_days":%d}`, keyTestMaxLifetime), wantCode: 201},
		{name: "ok: RFC3339", body: fmt.Sprintf(`{"name":"ok","expires_at":%q}`, rfc(48*time.Hour)), wantCode: 201},
		{name: "ok: date only", body: fmt.Sprintf(`{"name":"ok","expires_at":%q}`, day(5)), wantCode: 201},
		{name: "ok: name with allowed punctuation", body: `{"name":"CI (prod) v1.2_a-b:c/d@e+f","expires_in_days":5}`, wantCode: 201},
		{name: "ok: letters of other alphabets", body: `{"name":"nyckel åäö","expires_in_days":5}`, wantCode: 201},
		{name: "ok: 100 characters", body: `{"name":"` + strings.Repeat("n", 100) + `","expires_in_days":5}`, wantCode: 201},
		{name: "ok: empty expires_at next to expires_in_days", body: `{"name":"ok","expires_at":"","expires_in_days":5}`, wantCode: 201},

		{name: "no name", body: `{"expires_in_days":5}`, wantCode: 400},
		{name: "empty name", body: `{"name":"","expires_in_days":5}`, wantCode: 400},
		{name: "blank name", body: `{"name":"   ","expires_in_days":5}`, wantCode: 400},
		{name: "101 characters", body: `{"name":"` + strings.Repeat("n", 101) + `","expires_in_days":5}`, wantCode: 400},
		{name: "300 characters", body: `{"name":"` + name300 + `","expires_in_days":5}`, wantCode: 400},
		{name: "101 multi-byte characters", body: `{"name":"` + strings.Repeat("å", 101) + `","expires_in_days":5}`, wantCode: 400},
		{name: "markup in the name", body: `{"name":"<script>x</script>","expires_in_days":5}`, wantCode: 400},
		{name: "newline in the name", body: `{"name":"a\nb","expires_in_days":5}`, wantCode: 400},
		{name: "control character in the name", body: `{"name":"a\u0000b","expires_in_days":5}`, wantCode: 400},
		{name: "bidi override in the name", body: `{"name":"a‮b","expires_in_days":5}`, wantCode: 400},
		{name: "quote in the name", body: `{"name":"a\"b","expires_in_days":5}`, wantCode: 400},
		{name: "name is not a string", body: `{"name":5,"expires_in_days":5}`, wantCode: 400},

		{name: "no expiry", body: `{"name":"ok"}`, wantCode: 400},
		{name: "both expiry forms", body: fmt.Sprintf(`{"name":"ok","expires_at":%q,"expires_in_days":5}`, rfc(48*time.Hour)), wantCode: 400},
		{name: "zero days", body: `{"name":"ok","expires_in_days":0}`, wantCode: 400},
		{name: "negative days", body: `{"name":"ok","expires_in_days":-3}`, wantCode: 400},
		{name: "over the maximum lifetime (days)", body: fmt.Sprintf(`{"name":"ok","expires_in_days":%d}`, keyTestMaxLifetime+1), wantCode: 400},
		{name: "huge number of days", body: `{"name":"ok","expires_in_days":2147483647}`, wantCode: 400},
		{name: "days overflow", body: `{"name":"ok","expires_in_days":9223372036854775807}`, wantCode: 400},
		{name: "expiry in the past", body: fmt.Sprintf(`{"name":"ok","expires_at":%q}`, rfc(-time.Hour)), wantCode: 400},
		{name: "expiry now", body: fmt.Sprintf(`{"name":"ok","expires_at":%q}`, now.Format(time.RFC3339)), wantCode: 400},
		{name: "date in the past", body: fmt.Sprintf(`{"name":"ok","expires_at":%q}`, day(-1)), wantCode: 400},
		{name: "expiry over the maximum lifetime", body: fmt.Sprintf(`{"name":"ok","expires_at":%q}`, day(keyTestMaxLifetime+2)), wantCode: 400},
		{name: "never expires", body: `{"name":"ok","expires_at":"never"}`, wantCode: 400},
		{name: "unparsable date", body: `{"name":"ok","expires_at":"tomorrow"}`, wantCode: 400},
		{name: "null expiry", body: `{"name":"ok","expires_at":null}`, wantCode: 400},

		{name: "a role field is refused, not ignored", body: `{"name":"ok","expires_in_days":5,"role":"admin"}`, wantCode: 400},
		{name: "an owner field is refused", body: `{"name":"ok","expires_in_days":5,"user_id":"u-root"}`, wantCode: 400},
		{name: "invalid JSON", body: `{"name":`, wantCode: 400},
		{name: "empty body", body: ``, wantCode: 400},
		{name: "array body", body: `[]`, wantCode: 400},
		{name: "trailing data", body: `{"name":"ok","expires_in_days":5}{"x":1}`, wantCode: 400},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newKeyEnv(t, nil)
			w := env.do(http.MethodPost, "/api/v1/api-keys", tt.body, jwtFor(t, "u-alice"))
			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			if tt.wantCode == http.StatusBadRequest {
				assertSchema(t, errorSchema, w.Body.String())
				assert.Zero(t, env.countKeys(t), "a refused request creates no key")
				assert.NotContains(t, w.Header().Get("Cache-Control"), "no-store")
			}
		})
	}
}

func TestAPIKeyCreateLimit(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	var ids []string
	for i := 0; i < keyTestMaxActive; i++ {
		ids = append(ids, env.create(t, "u-alice", `{"name":"k","expires_in_days":5}`).ID)
	}
	w := env.do(http.MethodPost, "/api/v1/api-keys", `{"name":"one too many","expires_in_days":5}`, jwtFor(t, "u-alice"))
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, fmt.Sprintf("Maximum number of active API keys reached (%d)", keyTestMaxActive), errText(t, w))
	assert.NotContains(t, w.Body.String(), "raw_key")
	assert.Equal(t, int64(keyTestMaxActive), env.countKeys(t))

	// The limit is per user, expired keys do not count and a delete frees a slot.
	env.create(t, "u-bob", `{"name":"k","expires_in_days":5}`)
	past := time.Now().Add(-time.Hour)
	env.seedKey(t, "old", "u-bob", &past)
	env.create(t, "u-bob", `{"name":"k2","expires_in_days":5}`)

	require.Equal(t, http.StatusNoContent, env.do(http.MethodDelete, "/api/v1/api-keys/"+ids[0], "", jwtFor(t, "u-alice")).Code)
	env.create(t, "u-alice", `{"name":"again","expires_in_days":5}`)
}

func TestAPIKeyCreateConcurrentStaysWithinLimit(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = env.do(http.MethodPost, "/api/v1/api-keys", `{"name":"k","expires_in_days":5}`, jwtFor(t, "u-alice")).Code
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, keyTestMaxActive, created)
	assert.Equal(t, int64(keyTestMaxActive), env.countKeys(t))
}

// TestAPIKeyManagementNeedsALoginSession: a key cannot create, list or delete
// keys, or a stolen key could mint a successor that survives its revocation.
func TestAPIKeyManagementNeedsALoginSession(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	k := env.create(t, "u-root", `{"name":"admin key","expires_in_days":5}`)
	other := env.seedKey(t, "other", "u-bob", nil)
	_ = other
	hdr := map[string]string{"X-API-Key": k.RawKey}
	before := env.countKeys(t)

	tests := []struct{ name, method, path, body string }{
		{"create", http.MethodPost, "/api/v1/api-keys", `{"name":"successor","expires_in_days":5}`},
		{"list", http.MethodGet, "/api/v1/api-keys", ""},
		{"list all", http.MethodGet, "/api/v1/api-keys?all=true", ""},
		{"delete itself", http.MethodDelete, "/api/v1/api-keys/" + k.ID, ""},
		{"delete another key", http.MethodDelete, "/api/v1/api-keys/other", ""},
	}
	for _, tt := range tests {
		w := env.do(tt.method, tt.path, tt.body, hdr)
		assert.Equal(t, http.StatusForbidden, w.Code, tt.name)
		assert.Equal(t, "API keys can only be managed with a login session", errText(t, w), tt.name)
		assert.NotContains(t, w.Body.String(), "raw_key")
	}
	assert.Equal(t, before, env.countKeys(t))
	assert.Empty(t, env.revoker.calls())
	// The key still works for what it may do.
	assert.Equal(t, http.StatusOK, env.do(http.MethodGet, "/whoami", "", hdr).Code)
}

func TestAPIKeyCreateAccount(t *testing.T) {
	t.Parallel()

	t.Run("no authentication in the context", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		r := gin.New()
		r.POST("/k", env.handler.CreateAPIKey)
		r.GET("/k", env.handler.ListAPIKeys)
		r.DELETE("/k/:id", env.handler.DeleteAPIKey)
		for _, c := range []struct{ m, p, b string }{{"POST", "/k", `{"name":"x","expires_in_days":1}`}, {"GET", "/k", ""}, {"DELETE", "/k/x", ""}} {
			req := httptest.NewRequest(c.m, c.p, strings.NewReader(c.b))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusUnauthorized, w.Code, c.m)
		}
		assert.Zero(t, env.countKeys(t))
	})

	t.Run("a user that is disabled", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		w := env.do(http.MethodPost, "/api/v1/api-keys", `{"name":"x","expires_in_days":1}`, jwtFor(t, "u-off"))
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Zero(t, env.countKeys(t))
	})

	t.Run("a user that was deleted", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		w := env.do(http.MethodPost, "/api/v1/api-keys", `{"name":"x","expires_in_days":1}`, jwtFor(t, "u-ghost"))
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Zero(t, env.countKeys(t))
	})
}

func TestAPIKeyListScopes(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	for i := 0; i < 3; i++ {
		env.seedKey(t, fmt.Sprintf("a%d", i), "u-alice", nil)
		time.Sleep(2 * time.Millisecond)
	}
	env.seedKey(t, "b0", "u-bob", nil)
	env.seedKey(t, "r0", "u-root", nil)

	ids := func(t *testing.T, w *httptest.ResponseRecorder) (got []string, total int, pageSize int) {
		t.Helper()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertSchema(t, apiKeyListSchema, w.Body.String())
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Total    int `json:"total"`
			PageSize int `json:"pageSize"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		for _, d := range body.Data {
			got = append(got, d.ID)
		}
		return got, body.Total, body.PageSize
	}

	tests := []struct {
		name      string
		user      string
		query     string
		wantIDs   []string
		wantTotal int
		wantCode  int
	}{
		{name: "a user sees only the own keys, newest first", user: "u-alice", wantIDs: []string{"a2", "a1", "a0"}, wantTotal: 3},
		{name: "another user sees only theirs", user: "u-bob", wantIDs: []string{"b0"}, wantTotal: 1},
		{name: "a user without keys", user: "u-dev", wantIDs: nil, wantTotal: 0},
		{name: "admin without all sees the own keys", user: "u-root", wantIDs: []string{"r0"}, wantTotal: 1},
		{name: "admin all", user: "u-root", query: "?all=true", wantIDs: []string{"r0", "b0", "a2", "a1", "a0"}, wantTotal: 5},
		{name: "admin of one user", user: "u-root", query: "?user_id=u-alice", wantIDs: []string{"a2", "a1", "a0"}, wantTotal: 3},
		{name: "admin of an unknown user", user: "u-root", query: "?user_id=nobody", wantIDs: nil, wantTotal: 0},
		{name: "admin all, page 2", user: "u-root", query: "?all=true&pageSize=2&page=2", wantIDs: []string{"a2", "a1"}, wantTotal: 5},
		{name: "admin all, last page", user: "u-root", query: "?all=true&pageSize=2&page=3", wantIDs: []string{"a0"}, wantTotal: 5},
		{name: "admin all, past the end", user: "u-root", query: "?all=true&pageSize=2&page=9", wantIDs: nil, wantTotal: 5},
		{name: "own user_id is fine", user: "u-alice", query: "?user_id=u-alice", wantIDs: []string{"a2", "a1", "a0"}, wantTotal: 3},
		{name: "all=false is the own list", user: "u-alice", query: "?all=false", wantIDs: []string{"a2", "a1", "a0"}, wantTotal: 3},

		{name: "a user may not list all", user: "u-alice", query: "?all=true", wantCode: 403},
		{name: "devops may not list all", user: "u-dev", query: "?all=true", wantCode: 403},
		{name: "a user may not list another user", user: "u-alice", query: "?user_id=u-bob", wantCode: 403},
		{name: "devops may not list another user", user: "u-dev", query: "?user_id=u-alice", wantCode: 403},
		{name: "all is not a boolean", user: "u-root", query: "?all=maybe", wantCode: 400},
		{name: "user_id too long", user: "u-root", query: "?user_id=" + strings.Repeat("x", 200), wantCode: 400},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := env.do(http.MethodGet, "/api/v1/api-keys"+tt.query, "", jwtFor(t, tt.user))
			if tt.wantCode != 0 {
				assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
				assertSchema(t, errorSchema, w.Body.String())
				return
			}
			got, total, _ := ids(t, w)
			assert.Equal(t, tt.wantIDs, got)
			assert.Equal(t, tt.wantTotal, total)
		})
	}

	t.Run("page size is clamped and defaulted", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			query string
			want  int
		}{
			{"", 25}, {"?pageSize=1000", 100}, {"?pageSize=100", 100}, {"?pageSize=101", 100}, {"?pageSize=0", 25}, {"?pageSize=-5", 25},
			{"?pageSize=abc", 25}, {"?pageSize=7", 7}, {"?page=0", 25}, {"?page=-1", 25}, {"?page=abc", 25},
		} {
			_, _, pageSize := ids(t, env.do(http.MethodGet, "/api/v1/api-keys"+c.query, "", jwtFor(t, "u-alice")))
			assert.Equal(t, c.want, pageSize, c.query)
		}
	})

	t.Run("page defaults to 1", func(t *testing.T) {
		t.Parallel()
		w := env.do(http.MethodGet, "/api/v1/api-keys?page=0&pageSize=2", "", jwtFor(t, "u-alice"))
		got, _, _ := ids(t, w)
		assert.Equal(t, []string{"a2", "a1"}, got)
		assert.Contains(t, w.Body.String(), `"page":1`)
	})

	t.Run("an empty list is an empty array", func(t *testing.T) {
		t.Parallel()
		w := env.do(http.MethodGet, "/api/v1/api-keys", "", jwtFor(t, "u-dev"))
		assert.Contains(t, w.Body.String(), `"data":[]`)
	})
}

func TestAPIKeyDelete(t *testing.T) {
	t.Parallel()

	t.Run("the owner deletes the own key, which stops working at once", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		k := env.create(t, "u-alice", `{"name":"ci","expires_in_days":5}`)
		hdr := map[string]string{"X-API-Key": k.RawKey}
		require.Equal(t, http.StatusOK, env.do(http.MethodGet, "/whoami", "", hdr).Code)

		w := env.do(http.MethodDelete, "/api/v1/api-keys/"+k.ID, "", jwtFor(t, "u-alice"))
		require.Equal(t, http.StatusNoContent, w.Code)
		assert.Equal(t, []string{k.ID}, env.revoker.calls(), "its sockets are disconnected through DisconnectAPIKey")

		after := env.do(http.MethodGet, "/whoami", "", hdr)
		assert.Equal(t, http.StatusUnauthorized, after.Code, "no cache: the next request is refused")
		assert.Zero(t, env.countKeys(t))
	})

	t.Run("a key of another user is not found, like a missing key", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		env.seedKey(t, "bobs", "u-bob", nil)
		for _, caller := range []string{"u-alice", "u-dev"} {
			other := env.do(http.MethodDelete, "/api/v1/api-keys/bobs", "", jwtFor(t, caller))
			missing := env.do(http.MethodDelete, "/api/v1/api-keys/does-not-exist", "", jwtFor(t, caller))
			assert.Equal(t, http.StatusNotFound, other.Code, caller)
			assert.Equal(t, http.StatusNotFound, missing.Code)
			assert.Equal(t, missing.Body.String(), other.Body.String(), "the answers cannot be told apart")
		}
		assert.Equal(t, int64(1), env.countKeys(t), "the key is still there")
		assert.Empty(t, env.revoker.calls(), "no socket was closed")
	})

	t.Run("an admin deletes the key of another user", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		env.seedKey(t, "bobs", "u-bob", nil)
		w := env.do(http.MethodDelete, "/api/v1/api-keys/bobs", "", jwtFor(t, "u-root"))
		require.Equal(t, http.StatusNoContent, w.Code)
		assert.Equal(t, []string{"bobs"}, env.revoker.calls())
		assert.Zero(t, env.countKeys(t))

		missing := env.do(http.MethodDelete, "/api/v1/api-keys/bobs", "", jwtFor(t, "u-root"))
		assert.Equal(t, http.StatusNotFound, missing.Code)
		assert.Equal(t, "API key not found", errText(t, missing))
	})

	t.Run("an admin deletes the own key", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		env.seedKey(t, "mine", "u-root", nil)
		assert.Equal(t, http.StatusNoContent, env.do(http.MethodDelete, "/api/v1/api-keys/mine", "", jwtFor(t, "u-root")).Code)
	})

	t.Run("odd IDs", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		env.seedKey(t, "k1", "u-alice", nil)
		for _, id := range []string{strings.Repeat("x", 500), "k1%20", "%27%20OR%201=1", "k1%00", "..%2fk1"} {
			w := env.do(http.MethodDelete, "/api/v1/api-keys/"+id, "", jwtFor(t, "u-alice"))
			assert.Equal(t, http.StatusNotFound, w.Code, id)
		}
		assert.Equal(t, int64(1), env.countKeys(t))
	})

	t.Run("works without a revoker", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		env.handler.SetWebSocketRevoker(nil)
		env.seedKey(t, "k1", "u-alice", nil)
		assert.Equal(t, http.StatusNoContent, env.do(http.MethodDelete, "/api/v1/api-keys/k1", "", jwtFor(t, "u-alice")).Code)
	})

	t.Run("works with a nil hub as the revoker", func(t *testing.T) {
		t.Parallel()
		env := newKeyEnv(t, nil)
		var hub *websocket.Hub
		env.handler.SetWebSocketRevoker(hub)
		env.seedKey(t, "k1", "u-alice", nil)
		assert.Equal(t, http.StatusNoContent, env.do(http.MethodDelete, "/api/v1/api-keys/k1", "", jwtFor(t, "u-alice")).Code)
	})
}

func TestAPIKeyStoreErrors(t *testing.T) {
	t.Parallel()
	secret := errors.New("Error 1045: Access denied for user 'app'@'10.1.2.3' (using password: YES)")
	failOp := func(op string, err error) func(models.APIKeyRepository) models.APIKeyRepository {
		return func(r models.APIKeyRepository) models.APIKeyRepository {
			return failingAPIKeyRepo{APIKeyRepository: r, err: err, ops: map[string]bool{op: true}}
		}
	}
	const create = `{"name":"k","expires_in_days":5}`
	tests := []struct {
		name     string
		op       string
		err      error
		caller   string
		method   string
		path     string
		body     string
		wantCode int
	}{
		{"create, database error", "create", dberrors.NewDatabaseError("create", secret), "u-alice", "POST", "/api/v1/api-keys", create, 500},
		{"create, cancelled", "create", context.Canceled, "u-alice", "POST", "/api/v1/api-keys", create, 499},
		{"create, deadline", "create", context.DeadlineExceeded, "u-alice", "POST", "/api/v1/api-keys", create, 500},
		{"list, database error", "list", dberrors.NewDatabaseError("list", secret), "u-alice", "GET", "/api/v1/api-keys", "", 500},
		{"list, cancelled", "list", context.Canceled, "u-alice", "GET", "/api/v1/api-keys", "", 499},
		{"delete, database error", "delete", dberrors.NewDatabaseError("delete", secret), "u-alice", "DELETE", "/api/v1/api-keys/k1", "", 500},
		{"admin delete, lookup error", "find", dberrors.NewDatabaseError("find", secret), "u-root", "DELETE", "/api/v1/api-keys/k1", "", 500},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newKeyEnv(t, failOp(tt.op, tt.err))
			env.seedKey(t, "k1", "u-alice", nil)
			w := env.do(tt.method, tt.path, tt.body, jwtFor(t, tt.caller))
			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), "Access denied")
			assert.NotContains(t, w.Body.String(), "10.1.2.3")
			if tt.wantCode == 500 {
				assert.Equal(t, "Internal server error", errText(t, w))
			}
			assert.Empty(t, env.revoker.calls(), "a failed delete closes no socket")
		})
	}
}

// TestAPIKeyCreateIgnoresACancelledClient: the key is committed even when the
// client has gone, so the caller does not get a 499 for a key that exists.
func TestAPIKeyCreateIgnoresACancelledClient(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/api-keys", strings.NewReader(`{"name":"k","expires_in_days":5}`))
	req.Header.Set("Authorization", jwtFor(t, "u-alice")["Authorization"])
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, int64(1), env.countKeys(t))
}

func TestAPIKeyHandlerDefaults(t *testing.T) {
	t.Parallel()
	h := NewAPIKeyHandler(nil, nil, APIKeyOptions{})
	assert.Equal(t, DefaultAPIKeyMaxLifetimeDays, h.opts.MaxLifetimeDays)
	assert.Equal(t, DefaultAPIKeyMaxActive, h.opts.MaxActivePerUser)
	assert.Equal(t, 365, DefaultAPIKeyMaxLifetimeDays)
	assert.Equal(t, 25, DefaultAPIKeyMaxActive)
	h = NewAPIKeyHandler(nil, nil, APIKeyOptions{MaxLifetimeDays: -4, MaxActivePerUser: -1})
	assert.Equal(t, DefaultAPIKeyMaxLifetimeDays, h.opts.MaxLifetimeDays, "a negative value falls back to the default")
	assert.Equal(t, DefaultAPIKeyMaxActive, h.opts.MaxActivePerUser)
}

// TestAPIKeyExpiredAndOwnerStates covers the authentication of the keys that
// the endpoints created, end to end through CombinedAuth.
func TestAPIKeyExpiredAndOwnerStates(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	hdr := func(k string) map[string]string { return map[string]string{"X-API-Key": k} }

	t.Run("an expired key", func(t *testing.T) {
		t.Parallel()
		past := time.Now().Add(-time.Minute)
		w := env.do(http.MethodGet, "/whoami", "", hdr(env.seedKey(t, "exp", "u-alice", &past)))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "Invalid API key", errText(t, w))
	})
	t.Run("a key of a disabled owner", func(t *testing.T) {
		t.Parallel()
		w := env.do(http.MethodGet, "/whoami", "", hdr(env.seedKey(t, "off", "u-off", nil)))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "Invalid API key", errText(t, w))
	})
	t.Run("a key does not exceed the owner's role", func(t *testing.T) {
		t.Parallel()
		env2 := newKeyEnv(t, nil)
		k := env2.create(t, "u-alice", `{"name":"k","expires_in_days":5}`)
		w := env2.do(http.MethodGet, "/whoami", "", hdr(k.RawKey))
		assert.Contains(t, w.Body.String(), `"role":"user"`)
		_, err := env2.users.UpdateRole(t.Context(), "u-root", "u-alice", models.RoleDevOps)
		require.NoError(t, err)
		assert.Contains(t, env2.do(http.MethodGet, "/whoami", "", hdr(k.RawKey)).Body.String(), `"role":"devops"`, "the role is the owner's current role")
		_, err = env2.users.UpdateRole(t.Context(), "u-root", "u-alice", models.RoleUser)
		require.NoError(t, err)
		assert.Contains(t, env2.do(http.MethodGet, "/whoami", "", hdr(k.RawKey)).Body.String(), `"role":"user"`, "a demotion takes effect on the existing key")
	})
}

// TestAPIKeyDeleteClosesItsWebSocket is the end-to-end check of the
// revocation: a socket opened with a key is closed with 1008 when the owner
// deletes the key, through the real hub.
func TestAPIKeyDeleteClosesItsWebSocket(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()
	env.handler.SetWebSocketRevoker(hub)

	wsHandler := NewWebSocketHandler(hub, "*")
	wsHandler.SetIdentityResolver(func(c *gin.Context) websocket.ClientIdentity {
		id := websocket.ClientIdentity{UserID: middleware.GetUserIDFromContext(c), APIKeyID: middleware.GetAPIKeyIDFromContext(c)}
		if exp, ok := middleware.GetTokenExpiryFromContext(c); ok {
			id.ExpiresAt = exp
		}
		return id
	})
	env.router.GET("/ws", wsHandler.HandleWebSocket)
	server := httptest.NewServer(env.router)
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	k := env.create(t, "u-alice", `{"name":"stream","expires_in_days":5}`)
	other := env.create(t, "u-alice", `{"name":"other","expires_in_days":5}`)
	dial := func(key string) *gorilla.Conn {
		conn, _, err := gorilla.DefaultDialer.Dial(wsURL, http.Header{"X-API-Key": []string{key}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	withKey := dial(k.RawKey)
	withOther := dial(other.RawKey)
	require.Eventually(t, func() bool { return hub.ClientCount() == 2 }, 3*time.Second, 10*time.Millisecond)

	// Without a key or with a bad one the upgrade is refused (no identity).
	_, resp, err := gorilla.DefaultDialer.Dial(wsURL, http.Header{"X-API-Key": []string{"sk_" + strings.Repeat("e", 64)}})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	require.Equal(t, http.StatusNoContent, env.do(http.MethodDelete, "/api/v1/api-keys/"+k.ID, "", jwtFor(t, "u-alice")).Code)

	require.NoError(t, withKey.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, _, err = withKey.ReadMessage()
	var closeErr *gorilla.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, gorilla.ClosePolicyViolation, closeErr.Code)

	require.Eventually(t, func() bool { return hub.ClientCount() == 1 }, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, withOther.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, _, err = withOther.ReadMessage()
	var netErr interface{ Timeout() bool }
	require.ErrorAs(t, err, &netErr)
	assert.True(t, netErr.Timeout(), "the socket of the other key stays open")
}

func TestAPIKeyListHidesHashInModelJSON(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	env.seedKey(t, "k1", "u-alice", nil)
	w := env.do(http.MethodGet, "/api/v1/api-keys", "", jwtFor(t, "u-alice"))
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	for _, d := range m["data"].([]any) {
		for k := range d.(map[string]any) {
			assert.Contains(t, []string{"id", "user_id", "name", "prefix", "created_at", "last_used_at", "expires_at"}, k)
		}
	}
}

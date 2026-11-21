package routes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const itemBody = `{"name":"widget","price":3}`

// idempotencyOn turns the module on in cfg.
func idempotencyOn(cfg *config.Config) *config.Config {
	cfg.Idempotency = config.IdempotencyConfig{Enabled: true, TTL: time.Hour, LockTimeout: time.Minute}
	return cfg
}

// newIdempotencyRouter builds an auth-off router over a mock item repository
// and, with the module on, the GORM idempotency repository on SQLite.
func newIdempotencyRouter(t *testing.T, cfg *config.Config) (*gin.Engine, *handlers.MockRepository) {
	t.Helper()
	router := gin.New()
	healthChecker := health.New()
	healthChecker.SetReady(true)
	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Shutdown)

	repo := handlers.NewMockRepository()
	deps := Deps{Config: cfg, Repository: repo, HealthChecker: healthChecker, Hub: hub}
	if cfg.Idempotency.Enabled {
		db := openAuthDB(t)
		require.NoError(t, db.AutoMigrate(&models.IdempotencyKey{}))
		deps.Idempotency = database.NewGORMIdempotencyRepository(db)
	}
	res, err := Setup(router, deps)
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)
	return router, repo
}

func itemCount(t *testing.T, router http.Handler) int {
	t.Helper()
	w := doFrom(router, testPeer, http.MethodGet, "/api/v1/items?limit=100", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return strings.Count(w.Body.String(), `"name":"widget"`)
}

func routeTable(router *gin.Engine) []string {
	var out []string
	for _, r := range router.Routes() {
		out = append(out, r.Method+" "+r.Path)
	}
	sort.Strings(out)
	return out
}

func TestIdempotencyOffChangesNothing(t *testing.T) {
	t.Parallel()
	router, _ := newIdempotencyRouter(t, testConfig(100, 10, nil))

	hdr := map[string]string{middleware.IdempotencyKeyHeader: "same-key"}
	first := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", itemBody, hdr)
	second := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", itemBody, hdr)
	assert.Equal(t, http.StatusCreated, first.Code)
	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Empty(t, second.Header().Get(middleware.IdempotencyReplayedHeader))
	assert.Equal(t, 2, itemCount(t, router), "with the module off the key is ignored: two items")

	w := doFrom(router, testPeer, http.MethodOptions, "/api/v1/items", "", nil)
	assert.NotContains(t, w.Header().Get("Access-Control-Allow-Headers"), "Idempotency")
	assert.Equal(t, "X-Total-Count", w.Header().Get("Access-Control-Expose-Headers"))
}

func TestIdempotencyRouteTableIsTheSameOnAndOff(t *testing.T) {
	t.Parallel()
	off, _ := newIdempotencyRouter(t, testConfig(100, 10, nil))
	on, _ := newIdempotencyRouter(t, idempotencyOn(testConfig(100, 10, nil)))
	assert.Equal(t, routeTable(off), routeTable(on), "the module adds middleware, never a route")

	offAuth := newAuthEnv(t, nil)
	onAuth := newIdempotencyAuthEnv(t)
	assert.Equal(t, routeTable(offAuth.router), routeTable(onAuth.router))
}

func TestIdempotencyOnAuthOffReplays(t *testing.T) {
	t.Parallel()
	router, _ := newIdempotencyRouter(t, idempotencyOn(testConfig(100, 10, nil)))
	hdr := map[string]string{middleware.IdempotencyKeyHeader: "same-key"}

	first := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", itemBody, hdr)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	assert.Empty(t, first.Header().Get(middleware.IdempotencyReplayedHeader))

	second := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", itemBody, hdr)
	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, "true", second.Header().Get(middleware.IdempotencyReplayedHeader))
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, 1, itemCount(t, router), "the item was created once")

	other := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", `{"name":"other","price":1}`, hdr)
	assert.Equal(t, http.StatusUnprocessableEntity, other.Code)

	noKey := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", itemBody, nil)
	assert.Equal(t, http.StatusCreated, noKey.Code)
	assert.Equal(t, 2, itemCount(t, router), "a request without a key is never deduplicated")

	pre := doFrom(router, testPeer, http.MethodOptions, "/api/v1/items", "", nil)
	assert.Contains(t, pre.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")
	assert.Contains(t, pre.Header().Get("Access-Control-Expose-Headers"), "Idempotency-Replayed")
	assert.Contains(t, pre.Header().Get("Access-Control-Expose-Headers"), "X-Total-Count")
}

func TestIdempotencyAuthOffOversizedBodyIs413(t *testing.T) {
	t.Parallel()
	router, _ := newIdempotencyRouter(t, idempotencyOn(testConfig(100, 10, nil)))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(`{"name":"`+strings.Repeat("a", 2<<20)+`"}`))
	req.ContentLength = -1
	req.RemoteAddr = testPeer
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(middleware.IdempotencyKeyHeader, "big")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestSetupRefusesIdempotencyWithoutItsRepository(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mutate func(*config.Config)
		name   string
	}{
		{name: "no repository", mutate: func(*config.Config) {}},
		{name: "zero TTL", mutate: func(c *config.Config) { c.Idempotency.TTL = 0 }},
		{name: "zero lock timeout", mutate: func(c *config.Config) { c.Idempotency.LockTimeout = 0 }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := idempotencyOn(testConfig(100, 10, nil))
			tt.mutate(cfg)
			router := gin.New()
			deps := Deps{Config: cfg, Repository: handlers.NewMockRepository(), HealthChecker: health.New()}
			if tt.name != "no repository" {
				deps.Idempotency = database.NewGORMIdempotencyRepository(openAuthDB(t))
			}
			_, err := Setup(router, deps)
			require.ErrorIs(t, err, ErrIdempotencyDepsMissing)
			assert.Empty(t, router.Routes(), "nothing is registered")
		})
	}

	t.Run("a typed nil repository", func(t *testing.T) {
		t.Parallel()
		var repo *database.GORMIdempotencyRepository
		_, err := Setup(gin.New(), Deps{Config: idempotencyOn(testConfig(100, 10, nil)), Repository: handlers.NewMockRepository(), Idempotency: repo})
		require.ErrorIs(t, err, ErrIdempotencyDepsMissing)
	})

	t.Run("off needs none", func(t *testing.T) {
		t.Parallel()
		_, err := Setup(gin.New(), Deps{Config: testConfig(100, 10, nil), Repository: handlers.NewMockRepository(), HealthChecker: health.New()})
		require.NoError(t, err)
	})
}

// newIdempotencyAuthEnv is newAuthEnv with the module on.
func newIdempotencyAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	cfg := idempotencyOn(authOn())
	return newAuthEnv(t, cfg, func(env *authEnv, deps *AuthDeps) {
		require.NoError(t, env.db.AutoMigrate(&models.IdempotencyKey{}))
		env.idem = database.NewGORMIdempotencyRepository(env.db)
	})
}

func TestIdempotencyAuthOn(t *testing.T) {
	t.Parallel()
	env := newIdempotencyAuthEnv(t)
	alice := env.createUser(t, "alice", models.RoleAdmin)
	bob := env.createUser(t, "bob", models.RoleUser)
	aliceTok, bobTok := env.token(t, alice), env.token(t, bob)
	hdr := func(tok string) map[string]string {
		h := bearer(tok)
		h[middleware.IdempotencyKeyHeader] = "same-key"
		return h
	}

	t.Run("a repeat is replayed", func(t *testing.T) {
		first := env.do(t, http.MethodPost, "/api/v1/items", itemBody, hdr(aliceTok))
		require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
		second := env.do(t, http.MethodPost, "/api/v1/items", itemBody, hdr(aliceTok))
		assert.Equal(t, http.StatusCreated, second.Code)
		assert.Equal(t, "true", second.Header().Get(middleware.IdempotencyReplayedHeader))
		assert.Equal(t, first.Body.String(), second.Body.String())
	})

	t.Run("another user has the same key for himself", func(t *testing.T) {
		w := env.do(t, http.MethodPost, "/api/v1/items", itemBody, hdr(bobTok))
		assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.Empty(t, w.Header().Get(middleware.IdempotencyReplayedHeader))
	})

	t.Run("no credentials: 401, the key is never looked at", func(t *testing.T) {
		w := env.do(t, http.MethodPost, "/api/v1/items", itemBody, map[string]string{middleware.IdempotencyKeyHeader: "x"})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("an API key user shares the scope of the user", func(t *testing.T) {
		raw, _ := env.apiKey(t, alice)
		w := env.do(t, http.MethodPost, "/api/v1/items", itemBody, map[string]string{"X-API-Key": raw, middleware.IdempotencyKeyHeader: "same-key"})
		assert.Equal(t, "true", w.Header().Get(middleware.IdempotencyReplayedHeader))
	})

	t.Run("auth routes ignore the key", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			w := env.do(t, http.MethodPost, "/api/v1/auth/login", `{"username":"alice","password":"wrong-password"}`,
				map[string]string{middleware.IdempotencyKeyHeader: "login-key", "X-Requested-With": "XMLHttpRequest"})
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Empty(t, w.Header().Get(middleware.IdempotencyReplayedHeader))
		}
		var n int64
		require.NoError(t, env.db.Model(&models.IdempotencyKey{}).Where("idempotency_key = ?", "login-key").Count(&n).Error)
		assert.Zero(t, n)
	})

	t.Run("users are covered", func(t *testing.T) {
		h := hdr(aliceTok)
		h[middleware.IdempotencyKeyHeader] = "user-key"
		body := fmt.Sprintf(`{"username":"carol","password":%q,"role":"user"}`, testPassword)
		first := env.do(t, http.MethodPost, "/api/v1/users", body, h)
		require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
		second := env.do(t, http.MethodPost, "/api/v1/users", body, h)
		assert.Equal(t, http.StatusCreated, second.Code)
		assert.Equal(t, "true", second.Header().Get(middleware.IdempotencyReplayedHeader))
		assert.Equal(t, first.Body.String(), second.Body.String())
	})

	// The response of an API key create holds the raw key, which is shown once
	// and never stored. The module must not keep it in the table.
	t.Run("API key creation ignores the key and stores nothing", func(t *testing.T) {
		h := hdr(aliceTok)
		bodies := map[string]bool{}
		for i := 0; i < 2; i++ {
			w := env.do(t, http.MethodPost, "/api/v1/api-keys", `{"name":"ci","expires_in_days":7}`, h)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			assert.Empty(t, w.Header().Get(middleware.IdempotencyReplayedHeader))
			bodies[w.Body.String()] = true
		}
		assert.Len(t, bodies, 2, "two different keys were created")
		var n int64
		require.NoError(t, env.db.Model(&models.IdempotencyKey{}).Where("body LIKE ?", "%raw_key%").Count(&n).Error)
		assert.Zero(t, n, "no raw key in the table")
	})

	t.Run("only the requests that ran are audited", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, env.res.AuditRecorder.Shutdown(ctx))
		var types []string
		for _, e := range env.auditEntries(t) {
			types = append(types, e.EntityType)
		}
		assert.ElementsMatch(t, []string{middleware.AuditEntityItem, middleware.AuditEntityItem, middleware.AuditEntityUser, middleware.AuditEntityAPIKey, middleware.AuditEntityAPIKey}, types,
			"alice's and bob's first item, the user and the two keys: no replay and no 401 writes an entry")
	})
}

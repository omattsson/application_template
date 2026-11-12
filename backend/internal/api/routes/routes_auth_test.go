package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authRoutes are the routes that exist only with AUTH_ENABLED=true.
var authRoutes = []struct{ method, path string }{
	{http.MethodPost, "/api/v1/auth/login"},
	{http.MethodPost, "/api/v1/auth/register"},
	{http.MethodPost, "/api/v1/auth/refresh"},
	{http.MethodPost, "/api/v1/auth/logout"},
	{http.MethodPost, "/api/v1/auth/logout-all"},
	{http.MethodGet, "/api/v1/auth/me"},
	{http.MethodGet, "/api/v1/auth/oidc/login"},
	{http.MethodGet, "/api/v1/auth/oidc/callback"},
	{http.MethodGet, "/api/v1/api-keys"},
	{http.MethodPost, "/api/v1/api-keys"},
	{http.MethodDelete, "/api/v1/api-keys/00000000-0000-4000-8000-000000000001"},
	{http.MethodGet, "/api/v1/users"},
	{http.MethodPost, "/api/v1/users"},
	{http.MethodGet, "/api/v1/audit-logs"},
}

// TestAuthOffItemsIgnoreCredentials is Review Focus 1: with auth off no auth
// middleware runs, so Items is served whatever credentials a request carries.
func TestAuthOffItemsIgnoreCredentials(t *testing.T) {
	t.Parallel()

	credentials := []struct {
		name    string
		headers map[string]string
	}{
		{name: "no header"},
		{name: "garbage bearer", headers: map[string]string{"Authorization": "Bearer garbage"}},
		{name: "garbage api key", headers: map[string]string{"X-API-Key": "garbage"}},
		{name: "bearer and api key", headers: map[string]string{"Authorization": "Bearer garbage", "X-API-Key": "sk_x"}},
		{name: "basic scheme", headers: map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}},
	}

	for _, tt := range credentials {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, _ := newTestRouter(t, testConfig(100, 10, nil))

			w := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", `{"name":"widget","price":3}`, tt.headers)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var created models.Item
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
			id := fmt.Sprint(created.ID)

			w = doFrom(router, testPeer, http.MethodGet, "/api/v1/items", "", tt.headers)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			w = doFrom(router, testPeer, http.MethodGet, "/api/v1/items/"+id, "", tt.headers)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			w = doFrom(router, testPeer, http.MethodPut, "/api/v1/items/"+id,
				fmt.Sprintf(`{"name":"gadget","price":4,"version":%d}`, created.Version), tt.headers)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			w = doFrom(router, testPeer, http.MethodDelete, "/api/v1/items/"+id, "", tt.headers)
			assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			w = doFrom(router, testPeer, http.MethodGet, "/api/v1/ping", "", tt.headers)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	}
}

// TestAuthOffAuthRoutesAreNotRegistered: with auth off the auth, API key,
// user and audit routes do not exist (404).
func TestAuthOffAuthRoutesAreNotRegistered(t *testing.T) {
	t.Parallel()
	router, _ := newTestRouter(t, testConfig(100, 10, nil))

	for _, r := range authRoutes {
		w := doFrom(router, testPeer, r.method, r.path, `{}`, map[string]string{"X-Requested-With": "x"})
		assert.Equal(t, http.StatusNotFound, w.Code, "%s %s", r.method, r.path)
	}
}

// TestAuthModesPublicRoutes: health and ui-config are open in both modes.
func TestAuthModesPublicRoutes(t *testing.T) {
	t.Parallel()

	routers := map[string]*gin.Engine{}
	off, _ := newTestRouter(t, testConfig(100, 10, nil))
	routers["off"] = off
	routers["on"] = newAuthEnv(t, nil).router

	for mode, router := range routers {
		for _, path := range []string{"/health/live", "/health/ready", "/health", "/api/v1/ui-config"} {
			w := doFrom(router, testPeer, http.MethodGet, path, "", nil)
			assert.Equal(t, http.StatusOK, w.Code, "%s: %s", mode, path)
		}
	}
}

// TestUIConfigAuthFields: ui-config tells the UI which login forms exist.
func TestUIConfigAuthFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		auth      bool
		oidc      bool
		localAuth bool
		want      string
	}{
		{name: "auth off", want: `{"title":"Auth Test","auth_enabled":false,"oidc_enabled":false,"local_auth_enabled":false}`},
		{name: "auth on", auth: true, want: `{"title":"Auth Test","auth_enabled":true,"oidc_enabled":false,"local_auth_enabled":true}`},
		{name: "oidc only", auth: true, oidc: true, want: `{"title":"Auth Test","auth_enabled":true,"oidc_enabled":true,"local_auth_enabled":false}`},
		{name: "oidc and local", auth: true, oidc: true, localAuth: true, want: `{"title":"Auth Test","auth_enabled":true,"oidc_enabled":true,"local_auth_enabled":true}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var router *gin.Engine
			if tt.auth {
				cfg := authOn()
				cfg.OIDC = config.OIDCConfig{Enabled: tt.oidc, LocalAuth: tt.localAuth}
				router = newAuthEnv(t, cfg, withOIDC()).router
			} else {
				cfg := testConfig(100, 10, nil)
				cfg.App.Title = "Auth Test"
				// OIDC settings without auth are ignored.
				cfg.OIDC = config.OIDCConfig{Enabled: true, LocalAuth: true}
				router, _ = newTestRouter(t, cfg)
			}
			w := doFrom(router, testPeer, http.MethodGet, "/api/v1/ui-config", "", nil)
			require.Equal(t, http.StatusOK, w.Code)
			assert.JSONEq(t, tt.want, w.Body.String())
		})
	}
}

// withOIDC injects the fake identity provider (no network).
func withOIDC() envOption {
	return func(_ *authEnv, d *AuthDeps) { d.OIDCProvider = fakeOIDC{} }
}

// TestAuthOnAccess is the access table of the auth-on router.
func TestAuthOnAccess(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	user := env.createUser(t, "alice", models.RoleUser)
	devops := env.createUser(t, "dave", models.RoleDevOps)
	admin := env.createUser(t, "root", models.RoleAdmin)
	userKey, _ := env.apiKey(t, user)
	adminKey, _ := env.apiKey(t, admin)

	creds := map[string]map[string]string{
		"none":           nil,
		"garbage bearer": {"Authorization": "Bearer garbage"},
		"garbage key":    {"X-API-Key": "garbage"},
		"user":           bearer(env.token(t, user)),
		"devops":         bearer(env.token(t, devops)),
		"admin":          bearer(env.token(t, admin)),
		"user key":       {"X-API-Key": userKey},
		"admin key":      {"X-API-Key": adminKey},
	}

	tests := []struct {
		method, path, body, cred string
		want                     int
	}{
		{http.MethodGet, "/api/v1/items", "", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/items", "", "garbage bearer", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/items", "", "garbage key", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/items", "", "user", http.StatusOK},
		{http.MethodGet, "/api/v1/items", "", "user key", http.StatusOK},
		{http.MethodPost, "/api/v1/items", `{"name":"w","price":1}`, "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/items", `{"name":"w","price":1}`, "user", http.StatusCreated},
		{http.MethodGet, "/api/v1/ping", "", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/ping", "", "user", http.StatusOK},
		{http.MethodGet, "/api/v1/auth/me", "", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/auth/me", "", "user", http.StatusOK},
		{http.MethodGet, "/api/v1/users", "", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/users", "", "user", http.StatusForbidden},
		{http.MethodGet, "/api/v1/users", "", "devops", http.StatusForbidden},
		{http.MethodGet, "/api/v1/users", "", "admin", http.StatusOK},
		{http.MethodGet, "/api/v1/users", "", "admin key", http.StatusForbidden},
		{http.MethodGet, "/api/v1/audit-logs", "", "user", http.StatusForbidden},
		{http.MethodGet, "/api/v1/audit-logs", "", "devops", http.StatusForbidden},
		{http.MethodGet, "/api/v1/audit-logs", "", "admin", http.StatusOK},
		{http.MethodGet, "/api/v1/audit-logs", "", "admin key", http.StatusForbidden},
		{http.MethodGet, "/api/v1/api-keys", "", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/api-keys", "", "user", http.StatusOK},
		{http.MethodGet, "/api/v1/api-keys?all=true", "", "user", http.StatusForbidden},
		{http.MethodGet, "/api/v1/api-keys?all=true", "", "admin", http.StatusOK},
		{http.MethodGet, "/api/v1/api-keys", "", "user key", http.StatusForbidden},
		{http.MethodPost, "/api/v1/api-keys", `{"name":"k","expires_in_days":1}`, "user key", http.StatusForbidden},
		{http.MethodPost, "/api/v1/api-keys", `{"name":"k","expires_in_days":1}`, "user", http.StatusCreated},
		{http.MethodPost, "/api/v1/auth/logout-all", "", "user key", http.StatusForbidden},
		{http.MethodPost, "/api/v1/auth/logout-all", "", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/ui-config", "", "none", http.StatusOK},
		{http.MethodGet, "/health/live", "", "none", http.StatusOK},
		{http.MethodGet, "/health/ready", "", "none", http.StatusOK},
	}

	for _, tt := range tests {
		w := env.do(t, tt.method, tt.path, tt.body, creds[tt.cred])
		assert.Equal(t, tt.want, w.Code, "%s %s as %s: %s", tt.method, tt.path, tt.cred, w.Body.String())
	}
}

// TestAuthOnLoginLogoutFlow: a login gives a token that works until logout;
// after logout the same token is refused.
func TestAuthOnLoginLogoutFlow(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	env.createUser(t, "alice", models.RoleUser)

	tok, cookie := env.login(t, "alice")
	assert.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/items", "", bearer(tok)).Code)

	// Refresh works with the cookie and the CSRF header.
	h := map[string]string{"X-Requested-With": "x", "Cookie": cookie.Name + "=" + cookie.Value}
	w := env.do(t, http.MethodPost, "/api/v1/auth/refresh", "", h)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Logout: the handler authenticates itself (public route).
	lh := bearer(tok)
	lh["X-Requested-With"] = "x"
	w = env.do(t, http.MethodPost, "/api/v1/auth/logout", "", lh)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = env.do(t, http.MethodGet, "/api/v1/items", "", bearer(tok))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestAuthOnCSRFAndSessionRules: the cookie endpoints need the same-site
// marker; the OIDC routes (top-level navigations) do not; logout-all refuses
// an API key.
func TestAuthOnCSRFAndSessionRules(t *testing.T) {
	t.Parallel()
	cfg := authOn()
	cfg.OIDC = config.OIDCConfig{Enabled: true, LocalAuth: true, StateTTL: 5 * time.Minute, FrontendPath: "/auth/callback"}
	env := newAuthEnv(t, cfg, withOIDC())
	env.createUser(t, "alice", models.RoleUser)

	// No X-Requested-With: 403 on login (it sets the cookie), refresh, logout.
	for _, p := range []string{"/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/logout"} {
		w := env.do(t, http.MethodPost, p, `{"username":"alice","password":"`+testPassword+`"}`, nil)
		assert.Equal(t, http.StatusForbidden, w.Code, p)
	}

	// The OIDC login is a navigation without the header: it redirects (it is
	// not behind the same-site check, and the refresh repository is set, so it
	// is not 501).
	w := env.do(t, http.MethodGet, "/api/v1/auth/oidc/login", "", nil)
	assert.Equal(t, http.StatusFound, w.Code, w.Body.String())
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), "https://idp.example.com/authorize"))
	// The callback without a state is the generic 400, not 403.
	w = env.do(t, http.MethodGet, "/api/v1/auth/oidc/callback", "", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// TestAuthOnSameOIDCConfig: the password login reads the OIDC settings of
// the config: with OIDC on and OIDC_LOCAL_AUTH=false only a service account
// can log in with a password.
func TestAuthOnSameOIDCConfig(t *testing.T) {
	t.Parallel()
	cfg := authOn()
	cfg.OIDC = config.OIDCConfig{Enabled: true, LocalAuth: false, StateTTL: 5 * time.Minute, FrontendPath: "/auth/callback"}
	env := newAuthEnv(t, cfg, withOIDC())
	env.createUser(t, "alice", models.RoleUser)
	svc := env.createUser(t, "robot", models.RoleAdmin)
	svc.ServiceAccount = true
	require.NoError(t, env.db.Save(svc).Error)

	h := map[string]string{"X-Requested-With": "x"}
	w := env.do(t, http.MethodPost, "/api/v1/auth/login", `{"username":"alice","password":"`+testPassword+`"}`, h)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	w = env.do(t, http.MethodPost, "/api/v1/auth/login", `{"username":"robot","password":"`+testPassword+`"}`, h)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestAuthOnFailsClosedOnStoreError is R20 through the real router: a
// session store that cannot be read gives 500 and the handler never runs.
func TestAuthOnFailsClosedOnStoreError(t *testing.T) {
	t.Parallel()
	store := newFailingStore(t)
	env := newAuthEnv(t, nil, func(_ *authEnv, d *AuthDeps) { d.SessionStore = store })
	user := env.createUser(t, "alice", models.RoleUser)
	tok := env.token(t, user)

	store.setFail(true)
	w := env.do(t, http.MethodPost, "/api/v1/items", `{"name":"w","price":1}`, bearer(tok))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
	var n int64
	require.NoError(t, env.db.Model(&models.Item{}).Count(&n).Error)
	assert.Zero(t, n, "the request was not admitted")

	store.setFail(false)
	w = env.do(t, http.MethodPost, "/api/v1/items", `{"name":"w","price":1}`, bearer(tok))
	assert.Equal(t, http.StatusCreated, w.Code)
}

// TestAuthOnAPIKeyFailsClosedOnLookupError: the API key branch of the
// production wiring fails closed too (500, not 401).
func TestAuthOnAPIKeyFailsClosedOnLookupError(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil, func(e *authEnv, d *AuthDeps) { d.APIKeys = failingKeys{APIKeyRepository: e.keys} })
	user := env.createUser(t, "alice", models.RoleUser)
	raw, _ := env.apiKey(t, user)

	w := env.do(t, http.MethodGet, "/api/v1/items", "", map[string]string{"X-API-Key": raw})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
}

// TestSetupRefusesAuthWithoutDeps: with auth on, Setup refuses to build the
// router without the auth dependencies (a nil session store would skip every
// revocation check).
func TestSetupRefusesAuthWithoutDeps(t *testing.T) {
	t.Parallel()
	db := openAuthDB(t)
	full := func() *AuthDeps {
		st := sessionstore.NewMemory()
		t.Cleanup(st.Stop)
		return &AuthDeps{
			Users: database.NewGORMUserRepository(db), RefreshTokens: database.NewGORMRefreshTokenRepository(db),
			APIKeys: database.NewGORMAPIKeyRepository(db), AuditLogs: database.NewGORMAuditLogRepository(db),
			SessionStore: st,
		}
	}

	tests := []struct {
		name   string
		mutate func(*AuthDeps) *AuthDeps
	}{
		{name: "no auth deps", mutate: func(*AuthDeps) *AuthDeps { return nil }},
		{name: "nil session store", mutate: func(d *AuthDeps) *AuthDeps { d.SessionStore = nil; return d }},
		{name: "typed nil session store", mutate: func(d *AuthDeps) *AuthDeps { d.SessionStore = (*failingStore)(nil); return d }},
		{name: "nil users", mutate: func(d *AuthDeps) *AuthDeps { d.Users = nil; return d }},
		{name: "nil refresh tokens", mutate: func(d *AuthDeps) *AuthDeps { d.RefreshTokens = nil; return d }},
		{name: "nil api keys", mutate: func(d *AuthDeps) *AuthDeps { d.APIKeys = nil; return d }},
		{name: "nil audit logs", mutate: func(d *AuthDeps) *AuthDeps { d.AuditLogs = nil; return d }},
	}

	for _, tt := range tests {
		router := gin.New()
		res, err := Setup(router, Deps{
			Config: authOn(), Repository: handlers.NewMockRepository(), HealthChecker: health.New(),
			Hub: websocket.NewHub(), Auth: tt.mutate(full()),
		})
		require.Error(t, err, tt.name)
		assert.ErrorIs(t, err, ErrAuthDepsMissing, tt.name)
		assert.Nil(t, res, tt.name)
		assert.Empty(t, router.Routes(), "%s: no route is registered", tt.name)
	}

	// A config is required in both modes.
	_, err := Setup(gin.New(), Deps{HealthChecker: health.New(), Hub: websocket.NewHub()})
	assert.Error(t, err)
}

// TestAuthOnLoginRateLimit: the 11th login within a minute from one IP gets
// 429 (LOGIN_RATE_LIMIT default 10); register and the two OIDC routes have the
// same per-IP limit, each with its own budget. Refresh has no dedicated limit
// (see TestAuthOnRefreshIsOnlyUnderTheAPILimit). Another IP is unaffected.
func TestAuthOnLoginRateLimit(t *testing.T) {
	t.Parallel()

	routes := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/auth/login"},
		{http.MethodPost, "/api/v1/auth/register"},
		{http.MethodGet, "/api/v1/auth/oidc/login"},
		{http.MethodGet, "/api/v1/auth/oidc/callback"},
	}

	for _, r := range routes {
		r := r
		t.Run(r.path, func(t *testing.T) {
			t.Parallel()
			cfg := authOn()
			cfg.Server.LoginRateLimit = 10 // the LOGIN_RATE_LIMIT default
			env := newAuthEnv(t, cfg)
			h := map[string]string{"X-Requested-With": "x"}
			for i := 0; i < 10; i++ {
				// A new unknown name each time: the per-username throttle
				// (5 failures) must not answer 429 first.
				body := fmt.Sprintf(`{"username":"nobody%d","password":"wrong-password"}`, i)
				w := env.do(t, r.method, r.path, body, h)
				require.NotEqual(t, http.StatusTooManyRequests, w.Code, "request %d", i+1)
			}
			w := env.do(t, r.method, r.path, `{"username":"nobody","password":"wrong-password"}`, h)
			assert.Equal(t, http.StatusTooManyRequests, w.Code, "the 11th request")
			assert.JSONEq(t, `{"error":"rate limit exceeded"}`, w.Body.String())

			w = doFrom(env.router, "198.51.100.7:1000", r.method, r.path, `{"username":"x","password":"wrong-password"}`, h)
			assert.NotEqual(t, http.StatusTooManyRequests, w.Code, "another IP has its own budget")
		})
	}
}

// TestAuthOnRefreshIsOnlyUnderTheAPILimit: refresh has no dedicated limiter
// (a web UI with several tabs refreshes often, and the refresh needs a valid
// token anyway). It is under the v1 API limiter: the 11th refresh in a minute
// is not refused, the 101st request is (RATE_LIMIT default 100).
func TestAuthOnRefreshIsOnlyUnderTheAPILimit(t *testing.T) {
	t.Parallel()
	cfg := authOn()
	cfg.Server.RateLimit = 100
	cfg.Server.LoginRateLimit = 10
	env := newAuthEnv(t, cfg)
	h := map[string]string{"X-Requested-With": "x"}

	for i := 0; i < 100; i++ {
		w := env.do(t, http.MethodPost, "/api/v1/auth/refresh", "", h)
		require.Equal(t, http.StatusUnauthorized, w.Code, "refresh %d (no cookie)", i+1)
	}
	w := env.do(t, http.MethodPost, "/api/v1/auth/refresh", "", h)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "the 101st request of the minute is limited by RATE_LIMIT")
}

// TestAuthOnLimitersHaveSeparateBudgets: one IP that used up the login budget
// still has the register budget, and the OIDC login and callback budgets are
// separate too.
func TestAuthOnLimitersHaveSeparateBudgets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		first, second struct{ method, path string }
	}{
		{name: "login then register",
			first:  struct{ method, path string }{http.MethodPost, "/api/v1/auth/login"},
			second: struct{ method, path string }{http.MethodPost, "/api/v1/auth/register"}},
		{name: "register then login",
			first:  struct{ method, path string }{http.MethodPost, "/api/v1/auth/register"},
			second: struct{ method, path string }{http.MethodPost, "/api/v1/auth/login"}},
		{name: "oidc login then callback",
			first:  struct{ method, path string }{http.MethodGet, "/api/v1/auth/oidc/login"},
			second: struct{ method, path string }{http.MethodGet, "/api/v1/auth/oidc/callback"}},
		{name: "oidc callback then login",
			first:  struct{ method, path string }{http.MethodGet, "/api/v1/auth/oidc/callback"},
			second: struct{ method, path string }{http.MethodGet, "/api/v1/auth/oidc/login"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := authOn()
			cfg.Server.LoginRateLimit = 10
			env := newAuthEnv(t, cfg)
			h := map[string]string{"X-Requested-With": "x", "Content-Type": "application/json"}
			send := func(r struct{ method, path string }, i int) int {
				body := fmt.Sprintf(`{"username":"nobody%d","password":"wrong-password"}`, i)
				return env.do(t, r.method, r.path, body, h).Code
			}
			for i := 0; i < 10; i++ {
				require.NotEqual(t, http.StatusTooManyRequests, send(tt.first, i), "first %d", i+1)
			}
			require.Equal(t, http.StatusTooManyRequests, send(tt.first, 10), "the first budget is used up")
			for i := 0; i < 10; i++ {
				require.NotEqual(t, http.StatusTooManyRequests, send(tt.second, i), "second %d: its own budget", i+1)
			}
			assert.Equal(t, http.StatusTooManyRequests, send(tt.second, 10), "the second budget is used up too")
		})
	}
}

// TestAuthOnLoginRateLimitUsesTrustedProxyClientIP: the login limit keys on
// c.ClientIP: X-Forwarded-For counts only from a trusted proxy.
func TestAuthOnLoginRateLimitUsesTrustedProxyClientIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		trusted []string
		wantNew int // status of the 3rd login from a new forwarded address
	}{
		{name: "untrusted peer: forwarded address ignored", trusted: nil, wantNew: http.StatusTooManyRequests},
		{name: "trusted proxy: forwarded address is the client", trusted: []string{"192.0.2.10"}, wantNew: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := authOn()
			cfg.Server.LoginRateLimit = 2
			cfg.Server.TrustedProxies = tt.trusted
			env := newAuthEnv(t, cfg)
			login := func(xff string) int {
				h := map[string]string{"X-Requested-With": "x", "X-Forwarded-For": xff}
				return env.do(t, http.MethodPost, "/api/v1/auth/login", `{"username":"nobody","password":"wrong-password"}`, h).Code
			}
			require.Equal(t, http.StatusUnauthorized, login("203.0.113.1"))
			require.Equal(t, http.StatusUnauthorized, login("203.0.113.1"))
			assert.Equal(t, tt.wantNew, login("203.0.113.2"))
		})
	}
}

// TestAuthOnBadAPIKeyFloodIsRateLimited is R27: a flood of bad X-API-Key
// values from one IP gets 429 from the per-IP RATE_LIMIT middleware.
func TestAuthOnBadAPIKeyFloodIsRateLimited(t *testing.T) {
	t.Parallel()
	cfg := authOn()
	cfg.Server.RateLimit = 100 // the RATE_LIMIT default
	env := newAuthEnv(t, cfg)

	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("sk_%064x", i)
		w := env.do(t, http.MethodGet, "/api/v1/items", "", map[string]string{"X-API-Key": key})
		require.Equal(t, http.StatusUnauthorized, w.Code, "request %d", i+1)
	}
	w := env.do(t, http.MethodGet, "/api/v1/items", "", map[string]string{"X-API-Key": "sk_" + strings.Repeat("f", 64)})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// TestAuthOnAuditWiring: a mutating request of an authenticated user is
// recorded after the auth middleware; a refused one is not.
func TestAuthOnAuditWiring(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	user := env.createUser(t, "alice", models.RoleUser)

	w := env.do(t, http.MethodPost, "/api/v1/items", `{"name":"w","price":1}`, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w = env.do(t, http.MethodPost, "/api/v1/items", `{"name":"w","price":1}`, bearer(env.token(t, user)))
	require.Equal(t, http.StatusCreated, w.Code)
	w = env.do(t, http.MethodGet, "/api/v1/items", "", bearer(env.token(t, user)))
	require.Equal(t, http.StatusOK, w.Code)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, env.res.AuditRecorder.Shutdown(ctx))
	entries := env.auditEntries(t)
	require.Len(t, entries, 1)
	assert.Equal(t, user.ID, entries[0].UserID)
	assert.Equal(t, middleware.AuditActionCreate, entries[0].Action)
	assert.Equal(t, middleware.AuditEntityItem, entries[0].EntityType)
}

// TestAuthOnAuditRoutesAreKnown: every mutating route of the auth-on router
// gives an audit action and entity type that the audit log knows.
func TestAuthOnAuditRoutesAreKnown(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)

	checked := 0
	for _, r := range env.router.Routes() {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		ar := middleware.AuditRouteFor(r.Method, r.Path)
		if ar.Skip {
			assert.True(t, strings.HasPrefix(r.Path, "/api/v1/auth/"), "only auth routes skip the audit: %s %s", r.Method, r.Path)
			continue
		}
		checked++
		assert.True(t, middleware.IsKnownAuditAction(ar.Action), "%s %s: action %q", r.Method, r.Path, ar.Action)
		assert.True(t, middleware.IsKnownAuditEntityType(ar.EntityType), "%s %s: entity type %q", r.Method, r.Path, ar.EntityType)
	}
	assert.GreaterOrEqual(t, checked, 9, "items, api keys and users are audited")
}

// TestAuthOnEveryV1RouteIsAuthenticated: apart from the listed public routes
// every /api/v1 route answers 401 without credentials.
func TestAuthOnEveryV1RouteIsAuthenticated(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	public := map[string]bool{
		"GET /api/v1/ui-config":          true,
		"POST /api/v1/auth/login":        true,
		"POST /api/v1/auth/register":     true,
		"POST /api/v1/auth/refresh":      true,
		"POST /api/v1/auth/logout":       true,
		"GET /api/v1/auth/oidc/login":    true,
		"GET /api/v1/auth/oidc/callback": true,
	}

	for _, r := range env.router.Routes() {
		if !strings.HasPrefix(r.Path, "/api/v1/") || public[r.Method+" "+r.Path] {
			continue
		}
		path := strings.ReplaceAll(r.Path, ":id", "00000000-0000-4000-8000-000000000001")
		w := env.do(t, r.Method, path, `{}`, map[string]string{"X-Requested-With": "x"})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s %s", r.Method, r.Path)
	}
}

// TestAuthOnEveryRouteIsAuthenticatedOrListed walks the whole route table, not
// only /api/v1. Every route is either in the explicit allowlist of public
// routes or answers 401 without credentials. A new route outside the
// allowlist (a debug endpoint, a second /ws, a route registered on the
// engine instead of the authenticated group) fails this test until someone
// decides that it is public.
func TestAuthOnEveryRouteIsAuthenticatedOrListed(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	public := map[string]bool{
		"GET /health":                    true,
		"GET /health/live":               true,
		"GET /health/ready":              true,
		"GET /api/v1/ui-config":          true,
		"POST /api/v1/auth/login":        true,
		"POST /api/v1/auth/register":     true,
		"POST /api/v1/auth/refresh":      true,
		"POST /api/v1/auth/logout":       true,
		"GET /api/v1/auth/oidc/login":    true,
		"GET /api/v1/auth/oidc/callback": true,
		// Authenticates itself (token in the query or the header), checked
		// separately below.
		"GET /ws": true,
	}
	// Swagger is registered by the bootstrap when ENABLE_SWAGGER is true, not
	// by Setup; a route with this prefix would be that one.
	const swaggerPrefix = "/swagger/"

	walked := 0
	for _, r := range env.router.Routes() {
		if public[r.Method+" "+r.Path] || strings.HasPrefix(r.Path, swaggerPrefix) {
			continue
		}
		walked++
		path := strings.ReplaceAll(r.Path, ":id", "00000000-0000-4000-8000-000000000001")
		w := env.do(t, r.Method, path, `{}`, map[string]string{"X-Requested-With": "x"})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s %s is not in the allowlist and must answer 401 without credentials", r.Method, r.Path)
	}
	assert.GreaterOrEqual(t, walked, 15, "the table has the items, API key, user and audit log routes")

	// Every allowlisted route exists (a stale entry would hide a removal).
	registered := map[string]bool{}
	for _, r := range env.router.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	for key := range public {
		assert.True(t, registered[key], "allowlisted route %s is not registered", key)
	}

	// The socket needs a token before the upgrade: no token, no header, 401.
	w := env.do(t, http.MethodGet, "/ws", "", nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GET /ws without a token")
	w = env.do(t, http.MethodGet, "/ws?token=not-a-token", "", nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GET /ws with a bad token")
}

// TestSetupBuildsTheOIDCProviderFromTheConfig: without an injected client,
// Setup builds the identity provider from OIDC_* (no network at
// construction); a configuration it cannot use is an error and nothing is
// registered.
func TestSetupBuildsTheOIDCProviderFromTheConfig(t *testing.T) {
	t.Parallel()
	oidcCfg := func(issuer string) config.OIDCConfig {
		return config.OIDCConfig{
			Enabled: true, Issuer: issuer, ClientID: "app", ClientSecret: "s3cret-value",
			RedirectURL: "https://app.example.com/api/v1/auth/oidc/callback", StateTTL: 5 * time.Minute,
			FrontendPath: "/auth/callback",
		}
	}

	t.Run("valid config", func(t *testing.T) {
		t.Parallel()
		cfg := authOn()
		cfg.OIDC = oidcCfg("https://idp.example.com")
		env := newAuthEnv(t, cfg) // no injected provider
		assert.NotNil(t, env.res.AuthHandler)
	})

	t.Run("unusable config", func(t *testing.T) {
		t.Parallel()
		cfg := authOn()
		cfg.OIDC = oidcCfg("http://idp.example.com")
		db := openAuthDB(t)
		st := sessionstore.NewMemory()
		t.Cleanup(st.Stop)
		router := gin.New()
		res, err := Setup(router, Deps{
			Config: cfg, Repository: handlers.NewMockRepository(), HealthChecker: health.New(), Hub: websocket.NewHub(),
			Auth: &AuthDeps{
				Users: database.NewGORMUserRepository(db), RefreshTokens: database.NewGORMRefreshTokenRepository(db),
				APIKeys: database.NewGORMAPIKeyRepository(db), AuditLogs: database.NewGORMAuditLogRepository(db),
				SessionStore: st,
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "oidc")
		assert.NotContains(t, err.Error(), "s3cret-value")
		assert.Nil(t, res)
		assert.Empty(t, router.Routes())
	})
}

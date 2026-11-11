package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPIKeyEndpointsAllowOnlyAJWTSession: key management is an allowlist of
// the JWT method. A caller with a user ID but no method (a route without the
// auth middleware) or an unknown method is refused like an API key.
func TestAPIKeyEndpointsAllowOnlyAJWTSession(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)

	// The context keys of the middleware package ("userID", "authMethod").
	// A future auth middleware that sets a new method must not pass.
	methods := []struct{ name, method string }{
		{name: "no method", method: ""},
		{name: "unknown method", method: "cookie"},
		{name: "api key", method: middleware.AuthMethodAPIKey},
	}

	for _, m := range methods {
		m := m
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("userID", "u-alice")
				c.Set("role", "admin")
				if m.method != "" {
					c.Set("authMethod", m.method)
				}
				c.Next()
			})
			r.POST("/api/v1/api-keys", env.handler.CreateAPIKey)
			r.GET("/api/v1/api-keys", env.handler.ListAPIKeys)
			r.DELETE("/api/v1/api-keys/:id", env.handler.DeleteAPIKey)

			for _, req := range []*http.Request{
				httptest.NewRequest(http.MethodPost, "/api/v1/api-keys", nil),
				httptest.NewRequest(http.MethodGet, "/api/v1/api-keys", nil),
				httptest.NewRequest(http.MethodDelete, "/api/v1/api-keys/k1", nil),
			} {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				assert.Equal(t, http.StatusForbidden, w.Code, "%s %s", req.Method, req.URL.Path)
				assert.JSONEq(t, `{"error":"API keys can only be managed with a login session"}`, w.Body.String())
			}
		})
	}
	assert.Zero(t, env.countKeys(t))
}

// TestLogoutAllRefusesAnAPIKey: a stolen key must not log its owner out
// everywhere, even on a route without RequireSessionAuth.
func TestLogoutAllRefusesAnAPIKey(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	key := env.create(t, "u-alice", `{"name":"ci","expires_in_days":1}`)

	store := sessionstore.NewMemory()
	t.Cleanup(store.Stop)
	cfg := &config.AuthConfig{
		Enabled: true, JWTSecret: testJWTSecret, JWTExpiration: time.Hour, AccessTokenExpiration: 15 * time.Minute,
	}
	h := NewAuthHandler(env.users, cfg, nil)
	h.SetSessionStore(store)
	rev := &recordingRevoker{}
	h.SetWebSocketRevoker(rev)

	r := gin.New()
	r.POST("/logout-all", middleware.CombinedAuth(middleware.APIKeyAuthDeps{
		JWTSecret: testJWTSecret, APIKeyRepo: env.keys, UserRepo: env.users, SessionStore: store,
	}), h.LogoutAll)

	before := time.Now().Add(-time.Second)
	req := httptest.NewRequest(http.MethodPost, "/logout-all", nil)
	req.Header.Set("X-API-Key", key.RawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"This action requires a login session"}`, w.Body.String())
	blocked, err := store.IsUserBlocked(context.Background(), "u-alice", before)
	require.NoError(t, err)
	assert.False(t, blocked, "no user block was written")
	users, _ := rev.snapshot()
	assert.Empty(t, users, "no socket was closed")

	// The owner's login session still can.
	req = httptest.NewRequest(http.MethodPost, "/logout-all", nil)
	for k, v := range jwtFor(t, "u-alice") {
		req.Header.Set(k, v)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestUIConfigFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.Config
		want UIConfigResponse
	}{
		{name: "auth off", cfg: config.Config{App: config.AppConfig{Title: "T"}},
			want: UIConfigResponse{Title: "T"}},
		{name: "auth off ignores oidc", cfg: config.Config{App: config.AppConfig{Title: "T"}, OIDC: config.OIDCConfig{Enabled: true, LocalAuth: true}},
			want: UIConfigResponse{Title: "T"}},
		{name: "auth on", cfg: config.Config{App: config.AppConfig{Title: "T"}, Auth: config.AuthConfig{Enabled: true}},
			want: UIConfigResponse{Title: "T", AuthEnabled: true, LocalAuthEnabled: true}},
		{name: "oidc only", cfg: config.Config{App: config.AppConfig{Title: "T"}, Auth: config.AuthConfig{Enabled: true}, OIDC: config.OIDCConfig{Enabled: true}},
			want: UIConfigResponse{Title: "T", AuthEnabled: true, OIDCEnabled: true}},
		{name: "oidc and local", cfg: config.Config{App: config.AppConfig{Title: "T"}, Auth: config.AuthConfig{Enabled: true}, OIDC: config.OIDCConfig{Enabled: true, LocalAuth: true}},
			want: UIConfigResponse{Title: "T", AuthEnabled: true, OIDCEnabled: true, LocalAuthEnabled: true}},
		{name: "empty title", cfg: config.Config{}, want: UIConfigResponse{Title: config.DefaultAppTitle}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, UIConfigFromConfig(&tt.cfg))
		})
	}
	assert.Equal(t, UIConfigResponse{Title: config.DefaultAppTitle}, UIConfigFromConfig(nil))
}

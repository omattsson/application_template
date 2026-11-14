package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"backend/internal/api/handlers"
	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPeer = "192.0.2.10:4321"

// TestMain sets the Gin mode once: gin.SetMode writes a global, so it cannot
// run inside parallel tests.
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// newTestRouter builds an auth-off router with Setup and stops the limiters
// and the hub when the test ends.
func newTestRouter(t *testing.T, cfg *config.Config) (*gin.Engine, *RateLimiters) {
	t.Helper()

	router := gin.New()
	healthChecker := health.New()
	healthChecker.SetReady(true)

	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Shutdown)

	res, err := Setup(router, Deps{
		Config: cfg, Repository: handlers.NewMockRepository(), HealthChecker: healthChecker, Hub: hub,
	})
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)

	return router, res.RateLimiters
}

func testConfig(rateLimit, loginRateLimit int32, trusted []string) *config.Config {
	return &config.Config{
		CORS: config.CORSConfig{AllowedOrigins: "*"},
		Server: config.ServerConfig{
			RateLimit:      rateLimit,
			LoginRateLimit: loginRateLimit,
			TrustedProxies: trusted,
		},
	}
}

func TestSetupRoutes(t *testing.T) {
	router, _ := newTestRouter(t, testConfig(100, 10, nil))

	// Test cases
	tests := []struct {
		name         string
		route        string
		method       string
		expectedCode int
		expectedBody map[string]string
	}{
		{
			name:         "Health Check",
			route:        "/health",
			method:       "GET",
			expectedCode: 200,
			expectedBody: map[string]string{"status": "ok"},
		},
		{
			name:         "Ping endpoint",
			route:        "/api/v1/ping",
			method:       "GET",
			expectedCode: 200,
			expectedBody: map[string]string{"message": "pong"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(tt.method, tt.route, nil)
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)

			if tt.expectedBody != nil {
				var response map[string]string
				err := json.Unmarshal(w.Body.Bytes(), &response)
				assert.Nil(t, err)
				assert.Equal(t, tt.expectedBody, response)
			}
		})
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	t.Parallel()

	router, _ := newTestRouter(t, testConfig(100, 10, nil))

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "ok", method: http.MethodGet, path: "/api/v1/ping", status: http.StatusOK},
		{name: "health", method: http.MethodGet, path: "/health", status: http.StatusOK},
		{name: "not found", method: http.MethodGet, path: "/nope", status: http.StatusNotFound},
		{name: "bad request", method: http.MethodPost, path: "/api/v1/items", status: http.StatusBadRequest},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader("{"))
			req.RemoteAddr = testPeer
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.status, w.Code)
			assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
			assert.Equal(t, "strict-origin-when-cross-origin", w.Header().Get("Referrer-Policy"))
		})
	}
}

func TestSecurityHeadersOnRateLimitedResponse(t *testing.T) {
	t.Parallel()

	router, _ := newTestRouter(t, testConfig(1, 1, nil))

	var last *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		last = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
		req.RemoteAddr = testPeer
		router.ServeHTTP(last, req)
	}

	require.Equal(t, http.StatusTooManyRequests, last.Code)
	assert.Equal(t, "nosniff", last.Header().Get("X-Content-Type-Options"))
}

func TestRedactWSTokenNeverLogged(t *testing.T) {
	// Not parallel: replaces the global slog default logger.
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	router, _ := newTestRouter(t, testConfig(100, 10, nil))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws?token=abc-secret-jwt&keep=1", nil)
	req.RequestURI = req.URL.RequestURI()
	req.RemoteAddr = testPeer
	router.ServeHTTP(w, req)

	logged := buf.String()
	assert.Contains(t, logged, "/ws", "the request must have been logged")
	assert.NotContains(t, logged, "abc-secret-jwt")
	assert.NotContains(t, logged, "token=")
}

func TestRequestBodyTooLarge(t *testing.T) {
	t.Parallel()

	router, _ := newTestRouter(t, testConfig(100, 10, nil))
	oversized := `{"name":"` + strings.Repeat("a", 1<<20) + `"}`

	tests := []struct {
		name          string
		contentLength int64 // -1 = unknown (chunked)
	}{
		{name: "known content length", contentLength: int64(len(oversized))},
		{name: "unknown content length", contentLength: -1},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(oversized))
			req.ContentLength = tt.contentLength
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = testPeer
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			assert.JSONEq(t, `{"error":"request body too large"}`, w.Body.String())
		})
	}
}

func TestMalformedJSONIsBadRequest(t *testing.T) {
	t.Parallel()

	router, _ := newTestRouter(t, testConfig(100, 10, nil))

	for _, body := range []string{`{`, `{"name":`, `not json`, `[1,2`, `{"name": 12}`} {
		body := body
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = testPeer
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

// pingFrom sends a GET /api/v1/ping from the test peer with the given
// X-Forwarded-For value and returns the status code.
func pingFrom(router *gin.Engine, forwardedFor string) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.RemoteAddr = testPeer
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	router.ServeHTTP(w, req)

	return w.Code
}

// TestTrustedProxyNoneTrusted: without trusted proxies the client IP is the
// TCP peer, so forging X-Forwarded-For does not give a new rate-limit bucket.
func TestTrustedProxyNoneTrusted(t *testing.T) {
	t.Parallel()

	const limit = 5

	tests := []struct {
		name    string
		trusted []string
	}{
		{name: "nil list", trusted: nil},
		{name: "empty list", trusted: []string{}},
		{name: "peer not in list", trusted: []string{"10.0.0.0/8"}},
		{name: "invalid list fails closed", trusted: []string{"not-an-ip"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router, _ := newTestRouter(t, testConfig(limit, 10, tt.trusted))

			for i := 0; i < limit; i++ {
				require.Equal(t, http.StatusOK, pingFrom(router, fmt.Sprintf("1.2.3.%d", i+1)), "request %d", i+1)
			}
			assert.Equal(t, http.StatusTooManyRequests, pingFrom(router, "1.2.3.99"))
		})
	}
}

func TestTrustedProxyPeerListed(t *testing.T) {
	t.Parallel()

	const limit = 3

	router, _ := newTestRouter(t, testConfig(limit, 10, []string{"192.0.2.10"}))

	// Different forwarded addresses are limited separately.
	for i := 0; i < limit+2; i++ {
		require.Equal(t, http.StatusOK, pingFrom(router, fmt.Sprintf("1.2.3.%d", i+1)), "request %d", i+1)
	}

	// The same forwarded address is limited.
	for i := 0; i < limit; i++ {
		require.Equal(t, http.StatusOK, pingFrom(router, "8.8.8.8"), "request %d", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, pingFrom(router, "8.8.8.8"))
	// Another forwarded address is unaffected.
	assert.Equal(t, http.StatusOK, pingFrom(router, "8.8.4.4"))
}

func TestLoginRateLimiterEnforcesLimit(t *testing.T) {
	t.Parallel()

	const loginLimit = 2

	_, rls := newTestRouter(t, testConfig(100, loginLimit, nil))
	require.NotNil(t, rls.Login)
	require.NotNil(t, rls.API)

	// The login route does not exist yet; this route stands in for it.
	login := gin.New()
	login.POST("/login", rls.Login.RateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })

	post := func() int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = testPeer
		login.ServeHTTP(w, req)

		return w.Code
	}

	for i := 0; i < loginLimit; i++ {
		require.Equal(t, http.StatusOK, post())
	}
	assert.Equal(t, http.StatusTooManyRequests, post())
}

func TestRateLimitersStop(t *testing.T) {
	t.Parallel()

	var nilLimiters *RateLimiters
	assert.NotPanics(t, nilLimiters.Stop)
	assert.NotPanics(t, (&RateLimiters{}).Stop)

	_, rls := newTestRouter(t, testConfig(100, 10, nil))
	assert.NotPanics(t, rls.Stop)
	assert.NotPanics(t, rls.Stop, "Stop is idempotent")
}

func TestUIConfigRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		title    string
		auth     bool
		wantBody string
	}{
		{name: "defaults", title: "Application Template", wantBody: `{"title":"Application Template","auth_enabled":false,"oidc_enabled":false,"local_auth_enabled":false}`},
		{name: "custom", title: "My App", auth: true, wantBody: `{"title":"My App","auth_enabled":true,"oidc_enabled":false,"local_auth_enabled":true}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var router *gin.Engine
			if tt.auth {
				cfg := authOn()
				cfg.App.Title = tt.title
				router = newAuthEnv(t, cfg).router
			} else {
				cfg := testConfig(100, 10, nil)
				cfg.App.Title = tt.title
				router, _ = newTestRouter(t, cfg)
			}

			// No credentials of any kind.
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
			req.RemoteAddr = testPeer
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.Equal(t, "public, max-age=300", w.Header().Get("Cache-Control"))
		})
	}
}

func TestUIConfigRouteFromEnv(t *testing.T) {
	// Not parallel: t.Setenv modifies the process environment.
	t.Setenv("ENV_FILE", "/nonexistent/.env")
	t.Setenv("APP_TITLE", "My App")
	t.Setenv("AUTH_ENABLED", "true")
	// AUTH_ENABLED=true needs a JWT secret (16+ characters) to start.
	t.Setenv("JWT_SECRET", "routes-test-secret-value")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	router := newAuthEnv(t, cfg).router

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
	req.RemoteAddr = testPeer
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"title":"My App","auth_enabled":true,"oidc_enabled":false,"local_auth_enabled":true}`, w.Body.String())
}

func TestUIConfigRouteIsRateLimited(t *testing.T) {
	t.Parallel()

	router, _ := newTestRouter(t, testConfig(1, 1, nil))

	var last *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		last = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
		req.RemoteAddr = testPeer
		router.ServeHTTP(last, req)
	}

	assert.Equal(t, http.StatusTooManyRequests, last.Code)
}

// TestTrailingSlashIsNotRedirected: with RedirectTrailingSlash on, Gin answers
// GET /ws/?token=... with a 301 before any middleware runs and, in debug mode,
// prints the redirect target (with the query, so the WebSocket token) to its
// own writer. The router must not redirect at all: a path with a trailing
// slash is a plain 404.
func TestTrailingSlashIsNotRedirected(t *testing.T) {
	// Not parallel: gin.SetMode and gin.DefaultWriter are process-wide.
	var buf bytes.Buffer
	origWriter := gin.DefaultWriter
	gin.SetMode(gin.DebugMode)
	gin.DefaultWriter = &buf
	t.Cleanup(func() {
		gin.DefaultWriter = origWriter
		gin.SetMode(gin.TestMode)
	})

	router, _ := newTestRouter(t, testConfig(100, 10, nil))

	for _, target := range []string{
		"/ws/?token=abc-secret-jwt",
		"/api/v1/items/?token=abc-secret-jwt",
		"/health/live/",
		"/API/V1/ITEMS?token=abc-secret-jwt",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RequestURI = req.URL.RequestURI()
		req.RemoteAddr = testPeer
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, target)
		assert.Empty(t, w.Header().Get("Location"), target)
	}

	out := buf.String()
	assert.NotContains(t, out, "abc-secret-jwt", "gin's debug output must never hold the WebSocket token")
	assert.NotContains(t, out, "redirecting request")
}

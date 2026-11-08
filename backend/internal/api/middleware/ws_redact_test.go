package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactWSToken_StripsTokenAndStashesInContext(t *testing.T) {
	t.Parallel()

	var (
		seenRawQuery   string
		seenRequestURI string
		stashedToken   string
	)
	router := gin.New()
	router.Use(RedactWSToken())
	// Downstream middleware sees the URL AFTER redaction: this simulates a
	// logger or tracer reading the URL for its payload.
	router.Use(func(c *gin.Context) {
		seenRawQuery = c.Request.URL.RawQuery
		seenRequestURI = c.Request.RequestURI
		c.Next()
	})
	router.GET("/ws", func(c *gin.Context) {
		stashedToken = WSTokenFromContext(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws?token=secret-jwt&keep=this", nil)
	req.RequestURI = req.URL.RequestURI()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "secret-jwt", stashedToken, "handler must see the token via WSTokenFromContext")
	assert.NotContains(t, seenRawQuery, "token=")
	assert.NotContains(t, seenRawQuery, "secret-jwt")
	assert.Contains(t, seenRawQuery, "keep=this", "non-sensitive query params must survive")
	assert.NotContains(t, seenRequestURI, "secret-jwt")
}

func TestRedactWSToken_PassthroughForOtherPaths(t *testing.T) {
	t.Parallel()

	var seenRawQuery string
	router := gin.New()
	router.Use(RedactWSToken())
	router.Use(func(c *gin.Context) {
		seenRawQuery = c.Request.URL.RawQuery
		c.Next()
	})
	router.GET("/api/v1/items", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/items?token=should-pass-through&cursor=xyz", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, seenRawQuery, "token=should-pass-through",
		"query semantics are path-specific: only the WebSocket path is scrubbed")
	assert.Contains(t, seenRawQuery, "cursor=xyz")
}

func TestRedactWSToken_NoTokenIsNoOp(t *testing.T) {
	t.Parallel()

	var (
		seenRawQuery string
		stashed      string
	)
	router := gin.New()
	router.Use(RedactWSToken())
	router.Use(func(c *gin.Context) {
		seenRawQuery = c.Request.URL.RawQuery
		c.Next()
	})
	router.GET("/ws", func(c *gin.Context) {
		stashed = WSTokenFromContext(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws?keep=this", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, "keep=this", seenRawQuery)
	assert.Empty(t, stashed, "no token in URL: empty stash")
}

// The key `token` is stripped whatever its value: leaving `token=` in the
// URL would still leak the parameter name to log scanners.
func TestRedactWSToken_EmptyTokenValueStillScrubbed(t *testing.T) {
	t.Parallel()

	var (
		seenRawQuery string
		stashed      string
	)
	router := gin.New()
	router.Use(RedactWSToken())
	router.Use(func(c *gin.Context) {
		seenRawQuery = c.Request.URL.RawQuery
		c.Next()
	})
	router.GET("/ws", func(c *gin.Context) {
		stashed = WSTokenFromContext(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws?token=&keep=this", nil)
	req.RequestURI = req.URL.RequestURI()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, seenRawQuery, "token")
	assert.Contains(t, seenRawQuery, "keep=this")
	assert.Empty(t, stashed, "an empty value is no credential")
}

func TestRedactWSToken_MatchesDescendantPathsButNotLookalikes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path      string
		wantScrub bool
	}{
		{path: "/ws", wantScrub: true},
		{path: "/ws/v2", wantScrub: true},
		{path: "/ws/foo/bar", wantScrub: true},
		{path: "/wsfoo", wantScrub: false},
		{path: "/wso", wantScrub: false},
		{path: "/ws-debug", wantScrub: false},
		{path: "/api/v1/items", wantScrub: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			var seenRawQuery string
			router := gin.New()
			router.Use(RedactWSToken())
			router.Use(func(c *gin.Context) {
				seenRawQuery = c.Request.URL.RawQuery
				c.Next()
			})
			router.GET(tc.path, func(c *gin.Context) { c.Status(http.StatusOK) })

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path+"?token=jwt", nil)
			router.ServeHTTP(w, req)

			if tc.wantScrub {
				assert.NotContains(t, seenRawQuery, "token=", "expected %s to be scrubbed", tc.path)
			} else {
				assert.Contains(t, seenRawQuery, "token=jwt", "expected %s to pass through", tc.path)
			}
		})
	}
}

func TestRedactWSToken_NeverReachesLogger(t *testing.T) {
	// Not parallel: replaces the global slog default logger.
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	router := gin.New()
	router.Use(RedactWSToken(), Logger())
	router.GET("/ws", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws?token=abc", nil)
	req.RequestURI = req.URL.RequestURI()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, buf.String(), "/ws")
	assert.NotContains(t, buf.String(), "abc")
	assert.NotContains(t, buf.String(), "token")
}

func TestWSTokenFromContext_EmptyWithoutMiddleware(t *testing.T) {
	t.Parallel()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/ws?token=foo", nil)

	assert.Empty(t, WSTokenFromContext(c))
}

func TestWSTokenFromContext_HandlesWrongType(t *testing.T) {
	t.Parallel()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(wsContextKey, 12345)
	require.NotPanics(t, func() {
		assert.Empty(t, WSTokenFromContext(c))
	})
}

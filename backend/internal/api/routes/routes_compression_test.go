package routes

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compressionOn turns the compression on in cfg.
func compressionOn(cfg *config.Config) *config.Config {
	cfg.Compression = config.CompressionConfig{Enabled: true, MinSize: 1024}
	return cfg
}

// bigListConfig is an auth-off config with a rate limit that the seeding
// stays under.
func bigListConfig() *config.Config {
	return testConfig(1000, 1000, nil)
}

// seedItems creates n items through the API so that the list is well over the
// minimum size.
func seedItems(t *testing.T, router http.Handler, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`{"name":"item-%03d %s","price":%d}`, i, strings.Repeat("x", 20), i+1)
		w := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", body, nil)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	}
}

func gunzipBody(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return out
}

func TestCompressionOffChangesNothing(t *testing.T) {
	t.Parallel()
	router, _ := newTestRouter(t, bigListConfig())
	seedItems(t, router, 30)

	w := doFrom(router, testPeer, http.MethodGet, "/api/v1/items?limit=100", "", map[string]string{"Accept-Encoding": "gzip"})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Greater(t, w.Body.Len(), 1024)
	assert.Empty(t, w.Header().Get("Content-Encoding"))
	assert.NotContains(t, w.Header().Get("Vary"), "Accept-Encoding")
	assert.Contains(t, w.Body.String(), `"item-000`, "the body is plain JSON")
}

func TestCompressionOnCompressesTheLargeList(t *testing.T) {
	t.Parallel()
	router, _ := newTestRouter(t, compressionOn(bigListConfig()))
	seedItems(t, router, 30)

	plain := doFrom(router, testPeer, http.MethodGet, "/api/v1/items?limit=100", "", nil)
	require.Equal(t, http.StatusOK, plain.Code)
	assert.Empty(t, plain.Header().Get("Content-Encoding"))
	assert.Contains(t, plain.Header().Get("Vary"), "Accept-Encoding", "a response that could be compressed varies")
	assert.Greater(t, plain.Body.Len(), 1024)

	w := doFrom(router, testPeer, http.MethodGet, "/api/v1/items?limit=100", "", map[string]string{"Accept-Encoding": "gzip"})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Contains(t, w.Header().Get("Vary"), "Accept-Encoding")
	assert.Empty(t, w.Header().Get("Content-Length"))
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.Less(t, w.Body.Len(), plain.Body.Len()/2, "the list got much smaller")
	assert.JSONEq(t, plain.Body.String(), string(gunzipBody(t, w.Body.Bytes())), "the decoded body is the plain one")

	// The modules after the compression still ran.
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), "SecurityHeaders")
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"), "CORS")
	assert.NotEmpty(t, w.Header().Get("X-Request-ID"), "RequestID")
}

func TestCompressionOnLeavesSmallAnswersAlone(t *testing.T) {
	t.Parallel()
	router, _ := newTestRouter(t, compressionOn(bigListConfig()))

	tests := []struct {
		name, method, path string
		wantStatus         int
	}{
		{name: "liveness", method: http.MethodGet, path: "/health/live", wantStatus: http.StatusOK},
		{name: "unknown route", method: http.MethodGet, path: "/nope", wantStatus: http.StatusNotFound},
		{name: "empty items list", method: http.MethodGet, path: "/api/v1/items", wantStatus: http.StatusOK},
		{name: "preflight", method: http.MethodOptions, path: "/api/v1/items", wantStatus: http.StatusNoContent},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := doFrom(router, testPeer, tt.method, tt.path, "", map[string]string{"Accept-Encoding": "gzip"})
			assert.Equal(t, tt.wantStatus, w.Code)
			assert.Empty(t, w.Header().Get("Content-Encoding"))
		})
	}
}

func TestCompressionOnOversizedRequestIs413(t *testing.T) {
	t.Parallel()
	router, _ := newTestRouter(t, compressionOn(bigListConfig()))

	t.Run("declared length", func(t *testing.T) {
		t.Parallel()
		w := doFrom(router, testPeer, http.MethodPost, "/api/v1/items", `{"name":"`+strings.Repeat("a", 2<<20)+`"}`, map[string]string{"Accept-Encoding": "gzip"})
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Empty(t, w.Header().Get("Content-Encoding"))
	})
	t.Run("chunked", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(`{"name":"`+strings.Repeat("a", 2<<20)+`"}`))
		req.ContentLength = -1
		req.RemoteAddr = testPeer
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	})
	t.Run("a gzip request body is not decoded", func(t *testing.T) {
		t.Parallel()
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte(`{"name":"widget","price":3}`))
		require.NoError(t, zw.Close())
		req := httptest.NewRequest(http.MethodPost, "/api/v1/items", &gz)
		req.RemoteAddr = testPeer
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, "the gzip bytes are not JSON: the module does not inflate requests")
	})
}

func TestCompressionOnWebSocketUpgradeWorks(t *testing.T) {
	t.Parallel()
	router, _ := newTestRouter(t, compressionOn(bigListConfig()))
	target := wsURL(t, router)

	conn, status := dialWS(t, target, http.Header{"Accept-Encoding": {"gzip"}})
	require.Equal(t, http.StatusSwitchingProtocols, status)
	require.NotNil(t, conn)
	subscribe(t, conn, "items")

	// An event of the Items handler reaches the socket: the hub writes frames
	// of its own, never through the response writer.
	seedItems(t, router, 1)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "item.created")
}

func TestCompressionOnAuthOn(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, compressionOn(authOn()))
	user := env.createUser(t, "alice", models.RoleUser)
	tok := bearer(env.token(t, user))
	gz := map[string]string{"Accept-Encoding": "gzip", "Authorization": tok["Authorization"]}

	for i := 0; i < 20; i++ {
		body := fmt.Sprintf(`{"name":"item-%03d %s","price":%d}`, i, strings.Repeat("x", 20), i+1)
		w := env.do(t, http.MethodPost, "/api/v1/items", body, gz)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.Empty(t, w.Header().Get("Content-Encoding"), "a small answer is sent as it is")
	}

	// A 401 is small and plain.
	w := env.do(t, http.MethodGet, "/api/v1/items", "", map[string]string{"Accept-Encoding": "gzip"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, w.Header().Get("Content-Encoding"))

	list := env.do(t, http.MethodGet, "/api/v1/items?limit=100", "", gz)
	require.Equal(t, http.StatusOK, list.Code)
	assert.Equal(t, "gzip", list.Header().Get("Content-Encoding"))
	plain := env.do(t, http.MethodGet, "/api/v1/items?limit=100", "", tok)
	assert.JSONEq(t, plain.Body.String(), string(gunzipBody(t, list.Body.Bytes())))

	// The audit recorder is unchanged: one entry for each create, none for reads.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, env.res.AuditRecorder.Shutdown(ctx))
	entries := env.auditEntries(t)
	require.Len(t, entries, 20)
	assert.Equal(t, middleware.AuditActionCreate, entries[0].Action)
}

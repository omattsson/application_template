package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSwaggerDocCompressesWithTheModuleOn: the Swagger JSON is large and is
// registered after routes.Setup, so it runs behind the global middleware.
func TestSwaggerDocCompressesWithTheModuleOn(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode, and
	// t.Setenv modifies the process environment.
	t.Setenv("GIN_MODE", gin.TestMode)

	tests := []struct {
		name     string
		enabled  bool
		wantGzip bool
	}{
		{name: "on", enabled: true, wantGzip: true},
		{name: "off", enabled: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cfg := bootstrapTestConfig()
			cfg.Server.EnableSwagger = true
			cfg.Compression = config.CompressionConfig{Enabled: tt.enabled, MinSize: 1024}

			router, res, err := buildRouter(cfg, routerDeps{HealthChecker: health.New(), Hub: websocket.NewHub()})
			require.NoError(t, err)
			t.Cleanup(res.RateLimiters.Stop)

			get := func(encoding string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/swagger/doc.json", nil)
				req.RemoteAddr = "192.0.2.1:1234"
				if encoding != "" {
					req.Header.Set("Accept-Encoding", encoding)
				}
				router.ServeHTTP(w, req)
				return w
			}

			plain := get("")
			require.Equal(t, http.StatusOK, plain.Code)
			require.Greater(t, plain.Body.Len(), 1024)
			assert.Empty(t, plain.Header().Get("Content-Encoding"))

			w := get("gzip")
			require.Equal(t, http.StatusOK, w.Code)
			if !tt.wantGzip {
				assert.Empty(t, w.Header().Get("Content-Encoding"))
				assert.Equal(t, plain.Body.String(), w.Body.String())
				return
			}
			assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
			assert.Contains(t, w.Header().Get("Vary"), "Accept-Encoding")
			zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
			require.NoError(t, err)
			out, err := io.ReadAll(zr)
			require.NoError(t, err)
			assert.Equal(t, plain.Body.String(), string(out))
			assert.Less(t, w.Body.Len(), plain.Body.Len()/2)
		})
	}
}

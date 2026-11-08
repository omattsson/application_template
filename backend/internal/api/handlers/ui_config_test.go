package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serveUIConfig(title string, authEnabled bool) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/api/v1/ui-config", NewUIConfigHandler(title, authEnabled))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
	router.ServeHTTP(w, req)

	return w
}

func TestUIConfigHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		title       string
		authEnabled bool
		wantBody    string
	}{
		{
			name:     "defaults",
			title:    "Application Template",
			wantBody: `{"title":"Application Template","auth_enabled":false}`,
		},
		{
			name:        "custom title and auth enabled",
			title:       "My App",
			authEnabled: true,
			wantBody:    `{"title":"My App","auth_enabled":true}`,
		},
		{
			name:     "empty title falls back to the default",
			title:    "",
			wantBody: `{"title":"Application Template","auth_enabled":false}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := serveUIConfig(tt.title, tt.authEnabled)

			require.Equal(t, http.StatusOK, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.Equal(t, "public, max-age=300", w.Header().Get("Cache-Control"))
			assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
			assert.True(t, validateJSONSchema(t, uiConfigSchema, w.Body.Bytes()))
		})
	}
}

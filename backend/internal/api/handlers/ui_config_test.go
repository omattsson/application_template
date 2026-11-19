package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serveUIConfig(resp UIConfigResponse) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/api/v1/ui-config", NewUIConfigHandler(resp))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
	router.ServeHTTP(w, req)

	return w
}

func TestUIConfigHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resp     UIConfigResponse
		wantBody string
	}{
		{
			name:     "defaults",
			resp:     UIConfigResponse{Title: "Application Template"},
			wantBody: `{"title":"Application Template","auth_enabled":false,"oidc_enabled":false,"local_auth_enabled":false,"password_reset_enabled":false}`,
		},
		{
			name:     "custom title and auth enabled",
			resp:     UIConfigResponse{Title: "My App", AuthEnabled: true, LocalAuthEnabled: true},
			wantBody: `{"title":"My App","auth_enabled":true,"oidc_enabled":false,"local_auth_enabled":true,"password_reset_enabled":false}`,
		},
		{
			name:     "oidc only",
			resp:     UIConfigResponse{Title: "My App", AuthEnabled: true, OIDCEnabled: true},
			wantBody: `{"title":"My App","auth_enabled":true,"oidc_enabled":true,"local_auth_enabled":false,"password_reset_enabled":false}`,
		},
		{
			name:     "password reset",
			resp:     UIConfigResponse{Title: "My App", AuthEnabled: true, LocalAuthEnabled: true, PasswordResetEnabled: true},
			wantBody: `{"title":"My App","auth_enabled":true,"oidc_enabled":false,"local_auth_enabled":true,"password_reset_enabled":true}`,
		},
		{
			name:     "empty title falls back to the default",
			resp:     UIConfigResponse{},
			wantBody: `{"title":"Application Template","auth_enabled":false,"oidc_enabled":false,"local_auth_enabled":false,"password_reset_enabled":false}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := serveUIConfig(tt.resp)

			require.Equal(t, http.StatusOK, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.Equal(t, "public, max-age=300", w.Header().Get("Cache-Control"))
			assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
			assert.True(t, validateJSONSchema(t, uiConfigSchema, w.Body.Bytes()))
		})
	}
}

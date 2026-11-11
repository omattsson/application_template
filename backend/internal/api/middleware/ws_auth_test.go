package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSocketQueryToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		target   string
		headers  map[string]string
		wantAuth string
	}{
		{name: "query token becomes a bearer header", target: "/ws?token=abc", wantAuth: "Bearer abc"},
		{name: "no query token", target: "/ws", wantAuth: ""},
		{name: "empty query token", target: "/ws?token=", wantAuth: ""},
		{name: "authorization header wins", target: "/ws?token=abc", headers: map[string]string{"Authorization": "Bearer hdr"}, wantAuth: "Bearer hdr"},
		{name: "api key header: query token ignored", target: "/ws?token=abc", headers: map[string]string{"X-API-Key": "sk_x"}, wantAuth: ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gin.New()
			var got, rawQuery string
			r.GET("/ws", RedactWSToken(), WebSocketQueryToken(), func(c *gin.Context) {
				got = c.GetHeader("Authorization")
				rawQuery = c.Request.URL.RawQuery
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusNoContent, w.Code)
			assert.Equal(t, tt.wantAuth, got)
			assert.NotContains(t, rawQuery, "token", "the token stays out of the URL")
		})
	}
}

func TestRequireActiveUser(t *testing.T) {
	t.Parallel()
	users := newUserRepoFake(
		&models.User{ID: "u1", Username: "alice", Role: models.RoleUser},
		&models.User{ID: "u2", Username: "bob", Role: models.RoleUser, Disabled: true},
	)

	tests := []struct {
		name     string
		userID   string
		findErr  error
		cancel   bool
		wantCode int
		wantBody string
	}{
		{name: "active user", userID: "u1", wantCode: http.StatusNoContent},
		{name: "disabled user", userID: "u2", wantCode: http.StatusUnauthorized, wantBody: `{"error":"Session revoked"}`},
		{name: "deleted user", userID: "gone", wantCode: http.StatusUnauthorized, wantBody: `{"error":"Session revoked"}`},
		{name: "not authenticated", userID: "", wantCode: http.StatusUnauthorized, wantBody: `{"error":"Authentication required"}`},
		{name: "lookup fails", userID: "u1", findErr: errors.New("dial tcp"), wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`},
		{name: "client left", userID: "u1", findErr: context.Canceled, cancel: true, wantCode: statusClientClosedRequest},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := users
			if tt.findErr != nil {
				repo = newUserRepoFake()
				repo.findErr = tt.findErr
			}
			r := gin.New()
			r.GET("/x", func(c *gin.Context) {
				if tt.userID != "" {
					c.Set(contextKeyUserID, tt.userID)
				}
				c.Next()
			}, RequireActiveUser(repo), func(c *gin.Context) { c.Status(http.StatusNoContent) })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tt.wantCode, w.Code)
			if tt.wantBody != "" {
				assert.JSONEq(t, tt.wantBody, w.Body.String())
			}
		})
	}
}

// TestRequireSessionAuthIsAnAllowlist: only a JWT session passes; any other
// method that an auth middleware set is refused. No method (no auth
// middleware ran) still passes.
func TestRequireSessionAuthIsAnAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		want   int
	}{
		{name: "jwt", method: AuthMethodJWT, want: http.StatusNoContent},
		{name: "api key", method: AuthMethodAPIKey, want: http.StatusForbidden},
		{name: "unknown method", method: "cookie", want: http.StatusForbidden},
		{name: "no auth middleware", method: "", want: http.StatusNoContent},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gin.New()
			r.POST("/x", func(c *gin.Context) {
				if tt.method != "" {
					c.Set(contextKeyAuthMethod, tt.method)
				}
				c.Next()
			}, RequireSessionAuth(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
			assert.Equal(t, tt.want, w.Code)
		})
	}
}

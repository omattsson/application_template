package middleware

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// roleRouter returns a router where a test middleware sets the role in the
// context (when role is not nil) and mw gates GET /gated.
func roleRouter(role *string, mw gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.GET("/gated", func(c *gin.Context) {
		if role != nil {
			c.Set(contextKeyRole, *role)
		}
	}, mw, func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	return r
}

func TestRequireRoleGates(t *testing.T) {
	t.Parallel()

	str := func(s string) *string { return &s }
	tests := []struct {
		name string
		mw   func() gin.HandlerFunc
		role *string
		want int
	}{
		// RequireAdmin: admin only.
		{name: "admin passes RequireAdmin", mw: RequireAdmin, role: str("admin"), want: http.StatusNoContent},
		{name: "devops fails RequireAdmin", mw: RequireAdmin, role: str("devops"), want: http.StatusForbidden},
		{name: "user fails RequireAdmin", mw: RequireAdmin, role: str("user"), want: http.StatusForbidden},

		// RequireDevOps: devops and admin.
		{name: "admin passes RequireDevOps", mw: RequireDevOps, role: str("admin"), want: http.StatusNoContent},
		{name: "devops passes RequireDevOps", mw: RequireDevOps, role: str("devops"), want: http.StatusNoContent},
		{name: "user fails RequireDevOps", mw: RequireDevOps, role: str("user"), want: http.StatusForbidden},

		// RequireUser: any known role (user is the lowest role).
		{name: "admin passes RequireUser", mw: RequireUser, role: str("admin"), want: http.StatusNoContent},
		{name: "devops passes RequireUser", mw: RequireUser, role: str("devops"), want: http.StatusNoContent},
		{name: "user passes RequireUser", mw: RequireUser, role: str("user"), want: http.StatusNoContent},

		// Unauthenticated: no role in the context.
		{name: "no role on RequireAdmin", mw: RequireAdmin, role: nil, want: http.StatusUnauthorized},
		{name: "no role on RequireDevOps", mw: RequireDevOps, role: nil, want: http.StatusUnauthorized},
		{name: "no role on RequireUser", mw: RequireUser, role: nil, want: http.StatusUnauthorized},
		{name: "empty role is unauthenticated", mw: RequireUser, role: str(""), want: http.StatusUnauthorized},

		// Unknown role strings never pass, whatever the gate.
		{name: "unknown role fails RequireUser", mw: RequireUser, role: str("superuser"), want: http.StatusForbidden},
		{name: "wrong case fails RequireUser", mw: RequireUser, role: str("Admin"), want: http.StatusForbidden},
		{name: "padded role fails RequireAdmin", mw: RequireAdmin, role: str("admin "), want: http.StatusForbidden},
		{name: "unknown role fails RequireAdmin", mw: RequireAdmin, role: str("root"), want: http.StatusForbidden},

		// A gate with an unknown or no required role denies everyone.
		{name: "gate with an unknown role denies admin", mw: func() gin.HandlerFunc { return RequireRole("superuser") }, role: str("admin"), want: http.StatusForbidden},
		{name: "gate with no roles denies admin", mw: func() gin.HandlerFunc { return RequireRole() }, role: str("admin"), want: http.StatusForbidden},
		{name: "gate with an empty role name denies user", mw: func() gin.HandlerFunc { return RequireRole("") }, role: str("user"), want: http.StatusForbidden},
		{name: "unknown required role does not lower a known one", mw: func() gin.HandlerFunc { return RequireRole("bogus", "admin") }, role: str("user"), want: http.StatusForbidden},

		// Several roles: the lowest known one is the minimum.
		{name: "RequireRole(devops, admin) lets devops in", mw: func() gin.HandlerFunc { return RequireRole("admin", "devops") }, role: str("devops"), want: http.StatusNoContent},
		{name: "RequireRole(devops, admin) keeps user out", mw: func() gin.HandlerFunc { return RequireRole("admin", "devops") }, role: str("user"), want: http.StatusForbidden},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := doGet(roleRouter(tt.role, tt.mw()), "/gated", nil)
			assert.Equal(t, tt.want, w.Code)
			switch tt.want {
			case http.StatusUnauthorized:
				assert.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
			case http.StatusForbidden:
				assert.JSONEq(t, `{"error":"Insufficient permissions"}`, w.Body.String())
			}
		})
	}
}

func TestRequireRoleWrongContextType(t *testing.T) {
	t.Parallel()
	r := gin.New()
	r.GET("/gated", func(c *gin.Context) { c.Set(contextKeyRole, 3) }, RequireAdmin(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	// A non-string role reads as "no role": the request is unauthenticated.
	assert.Equal(t, http.StatusUnauthorized, doGet(r, "/gated", nil).Code)
}

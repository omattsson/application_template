package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// The cookie CSRF check needs the X-Requested-With header, which a cross-origin
// page can only send after a successful preflight. So the preflight answer
// must never allow that header, and no answer may allow credentials (cookies)
// cross-origin. See the invariant on CORS.
func TestCORSNeverEnablesCookieCSRFHeader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		allowed string
		origin  string
	}{
		{name: "wildcard", allowed: "*", origin: "https://evil.example.com"},
		{name: "empty list (all origins)", allowed: "", origin: "https://evil.example.com"},
		{name: "listed origin", allowed: "https://app.example.com,https://other.example.com", origin: "https://app.example.com"},
		{name: "listed origin, second entry", allowed: "https://app.example.com,https://other.example.com", origin: "https://other.example.com"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gin.New()
			r.Use(CORS(tt.allowed))
			r.Any("/api/v1/auth/refresh", func(c *gin.Context) { c.Status(http.StatusOK) })

			for _, method := range []string{http.MethodOptions, http.MethodPost} {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(method, "/api/v1/auth/refresh", nil)
				req.Header.Set("Origin", tt.origin)
				req.Header.Set("Access-Control-Request-Method", http.MethodPost)
				req.Header.Set("Access-Control-Request-Headers", "x-requested-with")
				r.ServeHTTP(w, req)

				allowHeaders := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers"))
				assert.NotEmpty(t, allowHeaders, "%s: the other headers are still listed", method)
				for _, h := range strings.Split(allowHeaders, ",") {
					assert.NotEqual(t, "x-requested-with", strings.TrimSpace(h), "%s: X-Requested-With must not be allowed", method)
				}
				assert.NotContains(t, allowHeaders, "requested-with")
				assert.Empty(t, w.Header().Values("Access-Control-Allow-Credentials"), "%s: cookies are never allowed cross-origin", method)
			}
		})
	}
}

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func corsRouter(allowed string, opts ...CORSOption) *gin.Engine {
	r := gin.New()
	r.Use(CORS(allowed, opts...))
	r.Any("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func TestCORSWithIdempotency(t *testing.T) {
	t.Parallel()

	t.Run("off: the header lists are as before", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		corsRouter("*").ServeHTTP(w, httptest.NewRequest(http.MethodOptions, "/test", nil))
		assert.NotContains(t, w.Header().Get("Access-Control-Allow-Headers"), "Idempotency")
		assert.Empty(t, w.Header().Values("Access-Control-Expose-Headers"))
	})

	for _, allowed := range []string{"*", "https://app.example.com"} {
		allowed := allowed
		t.Run("on with "+allowed, func(t *testing.T) {
			t.Parallel()
			for _, method := range []string{http.MethodOptions, http.MethodPost} {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(method, "/test", nil)
				req.Header.Set("Origin", "https://app.example.com")
				corsRouter(allowed, WithIdempotencyHeaders()).ServeHTTP(w, req)

				allow := w.Header().Get("Access-Control-Allow-Headers")
				assert.Contains(t, allow, "Idempotency-Key", method)
				assert.Contains(t, allow, "Authorization", method)
				assert.NotContains(t, strings.ToLower(allow), "requested-with", method)
				expose := w.Header().Get("Access-Control-Expose-Headers")
				assert.Contains(t, expose, "Idempotency-Replayed", method)
				assert.Contains(t, expose, "Retry-After", method)
			}
		})
	}
}

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// spanAttrs runs one request through SpanEnrichUser behind a middleware that
// starts a span and the given identity setup, and returns the span's
// attributes.
func spanAttrs(t *testing.T, recording bool, setIdentity func(*gin.Context)) map[attribute.Key]string {
	t.Helper()
	gin.SetMode(gin.TestMode)

	exp := tracetest.NewInMemoryExporter()
	sampler := sdktrace.AlwaysSample()
	if !recording {
		sampler = sdktrace.NeverSample()
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)), sdktrace.WithSampler(sampler))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx, span := tp.Tracer("test").Start(c.Request.Context(), "request")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		span.End()
	})
	r.Use(func(c *gin.Context) { setIdentity(c); c.Next() })
	r.Use(SpanEnrichUser())
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	require.Equal(t, http.StatusNoContent, w.Code, "the middleware always calls the next handler")

	out := map[attribute.Key]string{}
	spans := exp.GetSpans()
	if len(spans) == 0 {
		return out
	}
	for _, kv := range spans[0].Attributes {
		out[kv.Key] = kv.Value.Emit()
	}
	return out
}

func TestSpanEnrichUser(t *testing.T) {
	t.Run("adds the identity of the auth middleware to the span", func(t *testing.T) {
		got := spanAttrs(t, true, func(c *gin.Context) {
			c.Set(contextKeyUserID, "u1")
			c.Set(contextKeyUsername, "alice")
			c.Set(contextKeyRole, "admin")
		})
		assert.Equal(t, map[attribute.Key]string{"user.id": "u1", "user.name": "alice", "user.role": "admin"}, got)
	})

	t.Run("adds nothing for an anonymous request", func(t *testing.T) {
		assert.Empty(t, spanAttrs(t, true, func(*gin.Context) {}))
	})

	t.Run("ignores empty and non-string values", func(t *testing.T) {
		got := spanAttrs(t, true, func(c *gin.Context) {
			c.Set(contextKeyUserID, "")
			c.Set(contextKeyUsername, 42)
			c.Set(contextKeyRole, "user")
		})
		assert.Equal(t, map[attribute.Key]string{"user.role": "user"}, got)
	})

	t.Run("does nothing without a span", func(t *testing.T) {
		assert.Empty(t, spanAttrs(t, false, func(c *gin.Context) { c.Set(contextKeyUserID, "u1") }))
	})
}

// TestSpanEnrichUserReadsTheKeysOfTheAuthMiddleware: the real auth middleware
// puts the identity under keys with the "auth." prefix, and SpanEnrichUser
// must read the same ones (a plain "userID" would never match).
func TestSpanEnrichUserReadsTheKeysOfTheAuthMiddleware(t *testing.T) {
	got := spanAttrs(t, true, func(c *gin.Context) {
		setJWTContext(c, &Claims{UserID: "u9", Username: "bob", Role: "user"})
	})
	assert.Equal(t, "u9", got["user.id"])
	assert.Equal(t, "bob", got["user.name"])
	assert.Equal(t, "user", got["user.role"])
}

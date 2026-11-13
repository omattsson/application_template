package middleware

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SpanEnrichUser adds the authenticated user to the span of the request, so a
// trace can be tied to a user: user.id, user.name and user.role, from the
// context values that the auth middleware sets (the "auth." keys, see
// GetUserIDFromContext). A request without a user, or without a recording
// span, gets nothing. Register it AFTER the auth middleware.
//
// The username is personal data: it goes to whatever collector receives the
// spans.
func SpanEnrichUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		span := trace.SpanFromContext(c.Request.Context())
		if !span.IsRecording() {
			c.Next()
			return
		}

		if id := GetUserIDFromContext(c); id != "" {
			span.SetAttributes(attribute.String("user.id", id))
		}

		if name := GetUsernameFromContext(c); name != "" {
			span.SetAttributes(attribute.String("user.name", name))
		}

		if role := GetRoleFromContext(c); role != "" {
			span.SetAttributes(attribute.String("user.role", role))
		}

		c.Next()
	}
}

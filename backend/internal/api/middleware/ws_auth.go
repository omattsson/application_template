package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
)

// msgSessionRevoked is the answer for a user that is deleted or disabled.
const msgSessionRevoked = "Session revoked"

// WebSocketQueryToken lets a browser authenticate a /ws upgrade: the browser
// WebSocket API cannot set an Authorization header, so it sends the access
// token as ?token=. RedactWSToken has already moved that value out of the URL
// into the context; this middleware turns it into an "Authorization: Bearer"
// header for CombinedAuth. It does nothing when the request has an
// Authorization header (the header wins) or an X-API-Key header (a key is
// authenticated alone, and a query token must not turn the request into the
// ambiguous "both" case). Register it after RedactWSToken and before
// CombinedAuth, on /ws only: no other route accepts a token in the URL.
func WebSocketQueryToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" && len(c.Request.Header.Values(apiKeyHeader)) == 0 {
			if tok := WSTokenFromContext(c); tok != "" {
				c.Request.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		c.Next()
	}
}

// RequireActiveUser refuses a request whose authenticated user no longer
// exists or is disabled (401 "Session revoked"). The JWT path checks only the
// blocklists; a long-lived connection such as a WebSocket also checks the
// account itself before it opens. A request without a user is 401. A failed
// lookup is 500 (fail closed: the lookup is the only check that sees the
// account directly), a cancelled request 499. Put it after CombinedAuth.
func RequireActiveUser(users models.UserRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := GetUserIDFromContext(c)
		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		user, err := users.FindByID(c.Request.Context(), userID)
		switch {
		case err == nil && !user.Disabled:
			c.Next()
		case err == nil, errors.Is(err, dberrors.ErrNotFound):
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgSessionRevoked})
		case errors.Is(err, context.Canceled):
			slog.DebugContext(c.Request.Context(), "User check cancelled by the client", "user_id", userID, "error", err)
			c.AbortWithStatusJSON(statusClientClosedRequest, gin.H{"error": msgClientClosed})
		default:
			slog.ErrorContext(c.Request.Context(), "User check failed", "user_id", userID, "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		}
	}
}

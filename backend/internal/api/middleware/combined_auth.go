package middleware

import (
	"net/http"
	"strings"

	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
)

// APIKeyAuthDeps holds the dependencies for combined JWT + API-key auth.
//
// The API-key branch is not implemented yet (the "API keys" task fills it
// in). APIKeyRepo and UserRepo are already part of the struct so that task
// does not change the signature of CombinedAuth.
type APIKeyAuthDeps struct {
	JWTSecret    string
	APIKeyRepo   models.APIKeyRepository
	UserRepo     models.UserRepository
	SessionStore sessionstore.SessionStore
	// FailClosedOnStoreError is JWTAuthOptions.FailClosedOnStoreError.
	FailClosedOnStoreError bool
}

// CombinedAuth returns middleware that accepts:
//  1. Authorization: Bearer <jwt>: the JWT path (AuthRequiredWithOptions).
//  2. X-API-Key: <key>: the API-key path. NOT IMPLEMENTED YET: when
//     APIKeyRepo and UserRepo are both set, a request without a Bearer
//     header is answered 401 and never authenticated.
//
// When APIKeyRepo or UserRepo is nil the middleware is the JWT middleware.
//
// Fallthrough rules (kept for the API-key branch):
//   - A header that starts with "Bearer " is a JWT request and never falls
//     through to the API key, so a bad token with a good key is refused.
//   - Any other Authorization scheme ("ApiKey ...", "Basic ...") does not
//     authenticate; only the X-API-Key header would.
func CombinedAuth(deps APIKeyAuthDeps) gin.HandlerFunc {
	jwtMW := AuthRequiredWithOptions(JWTAuthOptions{
		JWTSecret:              deps.JWTSecret,
		SessionStore:           deps.SessionStore,
		FailClosedOnStoreError: deps.FailClosedOnStoreError,
	})
	if deps.APIKeyRepo == nil || deps.UserRepo == nil {
		return jwtMW
	}

	return func(c *gin.Context) {
		// Prefer JWT Bearer when present.
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			jwtMW(c)
			return
		}

		// STUB: API-key verification is added by the API keys task (prefix
		// lookup, constant-time hash compare, expiry, disabled user, rate
		// limit). Until then no key authenticates, whatever the header says.
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
	}
}

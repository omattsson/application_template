package middleware

import (
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
)

// APIKeyAuthDeps holds the dependencies for combined JWT + API-key auth.
type APIKeyAuthDeps struct {
	JWTSecret    string
	APIKeyRepo   models.APIKeyRepository
	UserRepo     models.UserRepository
	SessionStore sessionstore.SessionStore
	// FailClosedOnStoreError is JWTAuthOptions.FailClosedOnStoreError. For the
	// API-key branch it makes a failing database lookup 500 instead of 401
	// (a key is never accepted on a failed lookup, in either mode).
	FailClosedOnStoreError bool
}

// CombinedAuth returns middleware that accepts:
//  1. Authorization: Bearer <jwt>: the JWT path (AuthRequiredWithOptions).
//  2. X-API-Key: sk_<64 hex>: the API-key path (APIKeyAuthenticator). The
//     header is the only place a key is read: never the query string and
//     never the Authorization header.
//
// When APIKeyRepo or UserRepo is nil the middleware is the JWT middleware and
// X-API-Key does not authenticate.
//
// Rules of the combination (see APIKeyAuthenticator.middleware):
//   - A request with an X-API-Key header is authenticated only by that key.
//     An invalid, expired, deleted or unknown key, a disabled or deleted owner,
//     an empty value and a repeated header are all 401 with the same body. It
//     never falls through to the JWT path or to "no authentication".
//   - A request with a Bearer Authorization header is a JWT request.
//   - A request with both is refused with 400 before either is checked.
//   - Any other Authorization scheme ("ApiKey ...", "Basic ...") does not
//     authenticate and does not stop the X-API-Key header from doing so.
//   - A request with neither is 401.
//
// On success the key branch sets the same context values as the JWT path
// (user ID, username, role: the owner's current role), the auth method
// AuthMethodAPIKey, the key ID (GetAPIKeyIDFromContext) and the key expiry
// (GetTokenExpiryFromContext). It sets no jti and no session ID.
func CombinedAuth(deps APIKeyAuthDeps) gin.HandlerFunc {
	jwtMW := AuthRequiredWithOptions(JWTAuthOptions{
		JWTSecret:              deps.JWTSecret,
		SessionStore:           deps.SessionStore,
		FailClosedOnStoreError: deps.FailClosedOnStoreError,
	})
	if deps.APIKeyRepo == nil || deps.UserRepo == nil {
		return jwtMW
	}

	return NewAPIKeyAuthenticator(deps).middleware(jwtMW)
}

package middleware

import "github.com/gin-gonic/gin"

// SetTestIdentity puts an identity on c the way the auth middleware does, for
// the tests of handlers that run without it (method is AuthMethodJWT or
// AuthMethodAPIKey; "" sets none). Production code never calls it: it does
// not check anything, so a route that used it would trust the caller.
// TestSetTestIdentityIsNotUsedByProductionCode fails when a non-test file
// uses it, so the linker leaves it out of the binary.
func SetTestIdentity(c *gin.Context, userID, role, method string) {
	c.Set(contextKeyUserID, userID)
	c.Set(contextKeyRole, role)
	if method != "" {
		c.Set(contextKeyAuthMethod, method)
	}
}

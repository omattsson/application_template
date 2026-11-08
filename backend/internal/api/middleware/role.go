package middleware

import (
	"net/http"

	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// roleHierarchy maps each role to its level in the permission hierarchy.
// Higher level = more permissions: admin > devops > user. These are the only
// roles; any other string has no level and never passes a gate.
var roleHierarchy = map[string]int{
	models.RoleUser:   1,
	models.RoleDevOps: 2,
	models.RoleAdmin:  3,
}

// RequireRole returns middleware that checks whether the authenticated user's
// role is at least as powerful as the lowest of the given roles (using the
// role hierarchy), so RequireRole("devops") lets devops and admin in.
//
// It answers 401 when the request has no role in its context (not
// authenticated) and 403 when the role is below the minimum or is not one of
// the three known roles. Role names that are not known are ignored; when none
// of the given roles is known (including a call with no roles) the gate is
// closed: every authenticated user gets 403. A typo in a route definition
// therefore denies access instead of granting it.
func RequireRole(roles ...string) gin.HandlerFunc {
	// Precompute the minimum required level; 0 means no valid role was given.
	minLevel := 0

	for _, r := range roles {
		if l, ok := roleHierarchy[r]; ok {
			if minLevel == 0 || l < minLevel {
				minLevel = l
			}
		}
	}

	return func(c *gin.Context) {
		userRole := GetRoleFromContext(c)
		if userRole == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		userLevel, ok := roleHierarchy[userRole]
		if minLevel == 0 || !ok || userLevel < minLevel {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
			return
		}

		c.Next()
	}
}

// RequireAdmin allows only the admin role.
func RequireAdmin() gin.HandlerFunc {
	return RequireRole(models.RoleAdmin)
}

// RequireDevOps allows the devops role and, above it, admin.
func RequireDevOps() gin.HandlerFunc {
	return RequireRole(models.RoleDevOps)
}

// RequireUser allows every authenticated user with a known role: user,
// devops and admin. It rejects an unauthenticated request (401) and a role
// outside the three known roles (403).
func RequireUser() gin.HandlerFunc {
	return RequireRole(models.RoleUser)
}

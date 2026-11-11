package handlers

import (
	"strings"
	"sync"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/models"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
)

// WebSocketIdentityFromContext is the identity resolver of an authenticated
// /ws upgrade (WebSocketHandler.SetIdentityResolver). It reads what
// CombinedAuth put in the request context:
//   - a JWT: the user, the role claim, the jti, the session (sid), the expiry
//     and the issue time (millisecond precision with the iat_ms claim);
//   - an API key: the user, the owner's current role, the key ID, the key
//     expiry, and the connection time as the issue time, so a later user
//     block (disable, password reset, role change) covers the socket.
//
// Without authentication it returns the empty identity.
func WebSocketIdentityFromContext(c *gin.Context) websocket.ClientIdentity {
	id := websocket.ClientIdentity{
		UserID:    middleware.GetUserIDFromContext(c),
		Role:      middleware.GetRoleFromContext(c),
		TokenID:   middleware.GetJTIFromContext(c),
		SessionID: middleware.GetSessionIDFromContext(c),
		APIKeyID:  middleware.GetAPIKeyIDFromContext(c),
	}
	if exp, ok := middleware.GetTokenExpiryFromContext(c); ok {
		id.ExpiresAt = exp
	}
	switch {
	case id.APIKeyID != "":
		id.IssuedAt = time.Now()
	default:
		if iat, ok := middleware.GetTokenIssuedAtFromContext(c); ok {
			id.IssuedAt = iat
		}
	}
	return id
}

// Topic prefixes of DefaultTopicAuthorizer.
const (
	topicItems       = "items"
	topicItemPrefix  = "item:"
	topicUserPrefix  = "user:"
	topicAdminPrefix = "admin:"
)

// DefaultTopicAuthorizer is the topic policy of the template with
// AUTH_ENABLED=true (websocket.Hub.SetTopicAuthorizer). The identity comes
// from the token or API key of the upgrade. It allows:
//   - "items" and "item:<id>": every authenticated user with a known role
//     (every role can read items);
//   - "user:<id>": that user, and admins;
//   - "admin:<name>": admins only.
//
// Everything else is refused (deny by default): a new feature that publishes
// on a topic adds its rule here. Names are case-sensitive and a prefix needs a
// non-empty name.
func DefaultTopicAuthorizer(id websocket.ClientIdentity, topic string) bool {
	if id.UserID == "" || !models.IsValidRole(id.Role) {
		return false
	}
	isAdmin := id.Role == models.RoleAdmin
	switch {
	case topic == topicItems:
		return true
	case strings.HasPrefix(topic, topicItemPrefix):
		return len(topic) > len(topicItemPrefix)
	case strings.HasPrefix(topic, topicUserPrefix):
		owner := strings.TrimPrefix(topic, topicUserPrefix)
		return owner != "" && (owner == id.UserID || isAdmin)
	case strings.HasPrefix(topic, topicAdminPrefix):
		return isAdmin && len(topic) > len(topicAdminPrefix)
	default:
		return false
	}
}

// logLimiter lets one log line through per interval.
type logLimiter struct {
	last     time.Time
	interval time.Duration
	mu       sync.Mutex
}

func (l *logLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		return false
	}
	l.last = now
	return true
}

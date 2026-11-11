package handlers

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
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

// NewWebSocketRevocationChecker returns the check of the open sockets: the
// hub runs it every revalidation interval (Hub.StartRevalidation) and the
// WebSocket handler runs it once on each new socket (SetRecheck). For each
// identity it applies the checks of the HTTP auth:
//   - the token, session and user blocklists of store
//     (middleware.RevocationStatus, the shared revocation helper);
//   - with users: a deleted or disabled user (each user is read once a run);
//   - with keys, for a socket opened with an API key: a deleted or expired
//     key, or a key of another user (all keys of a run in one FindByIDs).
//
// On the first lookup error the run stops and logs once (at most one Error
// line a minute, the others at debug). failClosed selects the policy for the
// identities that were not checked: true reports them revoked, so their
// sockets close (the production wiring, R20: a revocation that cannot be
// checked must not keep a revoked socket open); false lets them pass until
// the next run. A cancelled context (hub shutdown) reports nothing revoked.
// Each dependency may be nil to skip its checks; with all nil it returns nil.
func NewWebSocketRevocationChecker(store sessionstore.SessionStore, users models.UserRepository, keys models.APIKeyRepository, failClosed bool) websocket.IdentityChecker {
	if isNilDep(store) && isNilDep(users) && isNilDep(keys) {
		return nil
	}
	if isNilDep(store) {
		store = nil
	}
	if isNilDep(users) {
		users = nil
	}
	if isNilDep(keys) {
		keys = nil
	}
	limit := &logLimiter{interval: time.Minute}
	c := &revocationChecker{store: store, users: users, keys: keys, failClosed: failClosed, limit: limit}
	return c.check
}

// isNilDep reports whether a dependency interface is nil or holds a nil
// pointer (a typed nil is not == nil and would be called).
func isNilDep(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

type revocationChecker struct {
	store      sessionstore.SessionStore
	users      models.UserRepository
	keys       models.APIKeyRepository
	limit      *logLimiter
	failClosed bool
}

func (r *revocationChecker) logFailure(msg string, args ...any) {
	args = append(args, "fail_closed", r.failClosed)
	if r.limit.allow() {
		slog.Error(msg, args...)
		return
	}
	slog.Debug(msg, args...)
}

// stop ends a run after a lookup error at index from: it applies the policy
// to the identities from there on and logs.
func (r *revocationChecker) stop(ctx context.Context, revoked []bool, from int, what string, err error, args ...any) []bool {
	if errors.Is(ctx.Err(), context.Canceled) {
		slog.Debug("WebSocket revalidation cancelled", "checked", from, "identities", len(revoked))
		return revoked
	}
	r.logFailure("WebSocket revalidation stopped: "+what+" failed; the next run retries",
		append(args, "checked", from, "identities", len(revoked), "error", err)...)
	if r.failClosed {
		for i := from; i < len(revoked); i++ {
			revoked[i] = true
		}
	}
	return revoked
}

func (r *revocationChecker) check(ctx context.Context, identities []websocket.ClientIdentity) []bool {
	revoked := make([]bool, len(identities))
	keys, err := loadSocketAPIKeys(ctx, r.keys, identities)
	if err != nil {
		return r.stop(ctx, revoked, 0, "API key lookup", err)
	}
	inactive := make(map[string]bool)
	now := time.Now()
	for i, id := range identities {
		if errors.Is(ctx.Err(), context.Canceled) {
			return revoked
		}
		if id.APIKeyID != "" && keys != nil {
			key := keys[id.APIKeyID]
			if key == nil || key.UserID != id.UserID || (key.ExpiresAt != nil && !now.Before(*key.ExpiresAt)) {
				revoked[i] = true
				continue
			}
		}
		if r.store != nil {
			revokedErr, lookupErr := tokenRevocationStatus(ctx, r.store, id)
			if revokedErr != nil {
				revoked[i] = true
				continue
			}
			if lookupErr != nil {
				return r.stop(ctx, revoked, i, "session store check", lookupErr)
			}
		}
		if r.users == nil || id.UserID == "" {
			continue
		}
		gone, seen := inactive[id.UserID]
		if !seen {
			var err error
			gone, err = userInactive(ctx, r.users, id.UserID)
			if err != nil {
				return r.stop(ctx, revoked, i, "user lookup", err, "user_id", id.UserID)
			}
			inactive[id.UserID] = gone
		}
		revoked[i] = gone
	}
	return revoked
}

// userInactive reports whether the user is deleted (not found) or disabled.
// Other lookup errors are returned.
func userInactive(ctx context.Context, repo models.UserRepository, userID string) (bool, error) {
	user, err := repo.FindByID(ctx, userID)
	if err != nil {
		if isNotFoundError(err) {
			return true, nil
		}
		return false, err
	}
	return user.Disabled, nil
}

// loadSocketAPIKeys reads the API keys of the socket identities in one
// FindByIDs call. It returns nil (no key checks) when repo is nil or no socket
// uses a key; a key that does not exist is missing from the map.
func loadSocketAPIKeys(ctx context.Context, repo models.APIKeyRepository, identities []websocket.ClientIdentity) (map[string]*models.APIKey, error) {
	if repo == nil {
		return nil, nil
	}
	seen := make(map[string]bool)
	var ids []string
	for _, id := range identities {
		if id.APIKeyID != "" && !seen[id.APIKeyID] {
			seen[id.APIKeyID] = true
			ids = append(ids, id.APIKeyID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := repo.FindByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]*models.APIKey, len(found))
	for _, k := range found {
		keys[k.ID] = k
	}
	return keys, nil
}

// tokenRevocationStatus runs the session store checks
// (middleware.RevocationStatus) for the identity of a socket. For an API key
// socket only the user block applies (no jti, no session), with the
// connection time as the issue time.
func tokenRevocationStatus(ctx context.Context, store sessionstore.SessionStore, id websocket.ClientIdentity) (revoked error, lookupErr error) {
	claims := &middleware.Claims{UserID: id.UserID, SessionID: id.SessionID}
	claims.ID = id.TokenID
	if !id.IssuedAt.IsZero() {
		claims.IssuedAt = jwt.NewNumericDate(id.IssuedAt)
		// Keep the millisecond precision of the identity.
		claims.IssuedAtMs = id.IssuedAt.UnixMilli()
	}
	return middleware.RevocationStatus(ctx, store, claims)
}

package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"backend/internal/ctxutil"
	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
)

// Name of the only header that carries an API key. A key in the query string
// (it would end up in access logs and browser history) or in the
// Authorization header is never read.
const apiKeyHeader = "X-API-Key" // #nosec G101 -- the name of a header, not a credential

// contextKeyAPIKeyID holds the ID of the API key that authenticated the
// request (not the key itself).
const contextKeyAPIKeyID = "auth.apiKeyID" // #nosec G101 -- the name of a context key, not a credential

// Messages of the API key answers. None of them says why a key was refused,
// and none contains an internal error or the key.
const (
	msgInvalidAPIKey   = "Invalid API key" // #nosec G101 -- an error message, not a credential
	msgAuthRequired    = "Authorization required"
	msgAuthAmbiguous   = "Send either an Authorization Bearer token or an X-API-Key header, not both"
	msgSessionRequired = MsgSessionRequired
)

// MsgSessionRequired is the 403 of an endpoint that only a login session may
// use (not an API key). Handlers that check the session themselves answer with
// the same text.
const MsgSessionRequired = "This action requires a login session"

// Limits of the last-used write of an API key.
const (
	// apiKeyUpdateOpTimeout bounds one write.
	apiKeyUpdateOpTimeout = 5 * time.Second
	// lastUsedInterval is the minimum time between two writes of one key.
	lastUsedInterval = time.Minute
	// lastUsedMaxEntries bounds the table of recently written keys.
	lastUsedMaxEntries = 10000
	// lastUsedMaxInFlight bounds the writes that run at once. A write that
	// finds no free slot is skipped: the next request of the key writes it.
	lastUsedMaxInFlight = 8
)

// GetAPIKeyIDFromContext returns the ID of the API key that authenticated the
// request, or "" for any other request (JWT or no authentication).
func GetAPIKeyIDFromContext(c *gin.Context) string { return stringFromContext(c, contextKeyAPIKeyID) }

// APIKeyPrincipal is the identity that a valid API key stands for.
type APIKeyPrincipal struct {
	Key  *models.APIKey
	User *models.User
	// Role is the effective role: the CURRENT role of the key's owner. A key
	// has no role of its own, so it can never exceed its owner's role and a
	// demoted owner's keys are demoted with the owner.
	Role string
}

// APIKeyAuthFailure describes a refused key. Message is safe to return to the
// client and does not depend on the reason of the refusal.
type APIKeyAuthFailure struct {
	Message string
	Status  int
}

// APIKeyAuthenticator verifies API keys. It is the key branch of CombinedAuth
// and can be used by other entry points (for example a WebSocket upgrade).
type APIKeyAuthenticator struct {
	keys       models.APIKeyRepository
	users      models.UserRepository
	lastUsed   *lastUsedUpdater
	failClosed bool
}

// NewAPIKeyAuthenticator returns an authenticator over deps.APIKeyRepo and
// deps.UserRepo. deps.FailClosedOnStoreError selects what a failing database
// lookup answers: 500 when set, else 401. A key is never accepted when a
// lookup fails, in either mode.
func NewAPIKeyAuthenticator(deps APIKeyAuthDeps) *APIKeyAuthenticator {
	return &APIKeyAuthenticator{
		keys:       deps.APIKeyRepo,
		users:      deps.UserRepo,
		lastUsed:   newLastUsedUpdater(),
		failClosed: deps.FailClosedOnStoreError,
	}
}

// Authenticate verifies the value of an X-API-Key header ("sk_" and 64 hex
// characters). It accepts the key when:
//   - a stored key has the same prefix and the SHA-256 of the key equals its
//     hash (constant-time compare),
//   - the key has not expired,
//   - the owner exists, is not disabled and has a known role.
//
// Every refusal of the key itself (malformed, unknown, expired, owner
// disabled, deleted or without a valid role) is the same 401 answer. A
// cancelled ctx is 499 and a deadline that passed is 500 (both modes); another
// lookup error is 500 with FailClosedOnStoreError and 401 without it. Nothing
// is cached: a deleted key or a disabled owner is refused by the next
// request.
func (a *APIKeyAuthenticator) Authenticate(ctx context.Context, clientKey string) (*APIKeyPrincipal, *APIKeyAuthFailure) {
	invalid := &APIKeyAuthFailure{Status: http.StatusUnauthorized, Message: msgInvalidAPIKey}

	rawKey, ok := models.ParseAPIKey(clientKey)
	if !ok {
		return nil, invalid
	}

	records, err := a.keys.FindByPrefix(ctx, rawKey[:models.APIKeyPrefixLen])
	if err != nil {
		if errors.Is(err, dberrors.ErrNotFound) {
			return nil, invalid
		}
		return nil, a.lookupFailure(ctx, "API key lookup failed", err, invalid)
	}

	record := matchAPIKey(records, rawKey)
	if record == nil || (record.ExpiresAt != nil && !time.Now().Before(*record.ExpiresAt)) {
		return nil, invalid
	}

	user, err := a.users.FindByID(ctx, record.UserID)
	if err != nil {
		if errors.Is(err, dberrors.ErrNotFound) {
			return nil, invalid
		}
		return nil, a.lookupFailure(ctx, "API key owner lookup failed", err, invalid, "key_id", record.ID, "user_id", record.UserID)
	}
	if user.Disabled || !models.IsValidRole(user.Role) {
		return nil, invalid
	}

	a.lastUsed.touch(ctx, a.keys, record.UserID, record.ID)
	return &APIKeyPrincipal{Key: record, User: user, Role: user.Role}, nil
}

// matchAPIKey returns the candidate whose hash matches rawKey, or nil. It
// compares against every candidate (in constant time each) without an early
// exit.
func matchAPIKey(candidates []*models.APIKey, rawKey string) *models.APIKey {
	var match *models.APIKey
	for _, r := range candidates {
		if models.VerifyAPIKeyHash(rawKey, r.KeyHash) && match == nil {
			match = r
		}
	}
	return match
}

// lookupFailure maps a failed lookup to an answer and logs it (without the
// key).
func (a *APIKeyAuthenticator) lookupFailure(ctx context.Context, msg string, err error, invalid *APIKeyAuthFailure, attrs ...any) *APIKeyAuthFailure {
	if errors.Is(err, context.Canceled) {
		slog.DebugContext(ctx, msg+" (cancelled by the client)", append(attrs, "error", err)...)
		return &APIKeyAuthFailure{Status: statusClientClosedRequest, Message: msgClientClosed}
	}
	slog.ErrorContext(ctx, msg, append(attrs, "fail_closed", a.failClosed, "error", err)...)
	if a.failClosed || errors.Is(err, context.DeadlineExceeded) {
		return &APIKeyAuthFailure{Status: http.StatusInternalServerError, Message: msgInternalError}
	}
	return invalid
}

// middleware returns the CombinedAuth handler: jwtMW for a Bearer request,
// this authenticator for an X-API-Key request.
//
// The rule for a request that carries both is fixed: it is refused with 400
// before either credential is checked, so a stolen or stale token cannot
// change which identity a request has and a client never learns which of the
// two worked. A request with an X-API-Key header (even an empty one, or one
// that is repeated) is authenticated only by that key: it never falls through
// to the JWT path or to "no authentication".
func (a *APIKeyAuthenticator) middleware(jwtMW gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyValues := c.Request.Header.Values(apiKeyHeader)
		hasKey := len(keyValues) > 0
		bearer := hasBearerScheme(c.GetHeader("Authorization"))

		switch {
		case bearer && hasKey:
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": msgAuthAmbiguous})
		case bearer:
			jwtMW(c)
		case !hasKey:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgAuthRequired})
		case len(keyValues) > 1:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgInvalidAPIKey})
		default:
			a.authenticate(c, keyValues[0])
		}
	}
}

// authenticate runs the key check and sets the request context.
func (a *APIKeyAuthenticator) authenticate(c *gin.Context, clientKey string) {
	p, failure := a.Authenticate(c.Request.Context(), clientKey)
	if failure != nil {
		c.AbortWithStatusJSON(failure.Status, gin.H{"error": failure.Message})
		return
	}

	// The same keys as the JWT path, plus the method and the key ID. There is
	// no jti and no session ID: a key is not a login session.
	c.Set(contextKeyUserID, p.User.ID)
	c.Set(contextKeyUsername, p.User.Username)
	c.Set(contextKeyRole, p.Role)
	c.Set(contextKeyAuthMethod, AuthMethodAPIKey)
	c.Set(contextKeyAPIKeyID, p.Key.ID)
	if p.Key.ExpiresAt != nil {
		// The credential expiry, as for a token: a socket opened with the key
		// closes at it.
		c.Set(contextKeyExpiry, *p.Key.ExpiresAt)
	}
	c.Next()
}

// hasBearerScheme reports whether an Authorization header uses the Bearer
// scheme (case-insensitive), with or without a token.
func hasBearerScheme(header string) bool {
	scheme, _, _ := strings.Cut(header, " ")
	return strings.EqualFold(scheme, "Bearer")
}

// RequireSessionAuth refuses (403) a request that an auth middleware
// authenticated with anything other than a login session (a JWT): an API key
// today, and any method added later (an allowlist, not a denylist). Put it on
// the endpoints that act on the login session or the credentials themselves
// (logout, logout of all sessions, key management, user administration): a
// key must not be able to mint another key, which would outlive its own
// revocation, or end the owner's sessions. A request without an
// authentication method (no auth middleware ran) passes: that route's own
// authentication decides (the public logout reads only the bearer token and
// the refresh cookie).
func RequireSessionAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m := GetAuthMethodFromContext(c); m != "" && m != AuthMethodJWT {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": msgSessionRequired})
			return
		}
		c.Next()
	}
}

// lastUsedUpdater writes the last-used time of API keys. The write is a
// courtesy for the key list: it runs after the authentication, never fails or
// delays the request, is skipped when the key was written in the last
// lastUsedInterval, and is bounded in memory and in concurrent writes.
type lastUsedUpdater struct {
	now  func() time.Time
	seen map[string]time.Time
	sem  chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
}

func newLastUsedUpdater() *lastUsedUpdater {
	return &lastUsedUpdater{
		now:  time.Now,
		seen: make(map[string]time.Time),
		sem:  make(chan struct{}, lastUsedMaxInFlight),
	}
}

// reserve records the write of keyID and reports whether it should run.
func (u *lastUsedUpdater) reserve(keyID string, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if prev, ok := u.seen[keyID]; ok && now.Sub(prev) < lastUsedInterval {
		return false
	}
	if len(u.seen) >= lastUsedMaxEntries {
		for id, at := range u.seen {
			if now.Sub(at) >= lastUsedInterval {
				delete(u.seen, id)
			}
		}
		if len(u.seen) >= lastUsedMaxEntries {
			return false
		}
	}
	u.seen[keyID] = now
	return true
}

// touch starts the write of the last-used time of a key, at most once per
// key and interval. It returns at once. The write uses a context without the
// request's cancel (a client that leaves does not skip it) with a short
// timeout. A failed write is logged (key ID only) and otherwise ignored.
func (u *lastUsedUpdater) touch(ctx context.Context, repo models.APIKeyRepository, userID, keyID string) {
	if u == nil || repo == nil {
		return
	}
	now := u.now()
	if !u.reserve(keyID, now) {
		return
	}
	select {
	case u.sem <- struct{}{}:
	default:
		// Too many writes in flight: forget the reservation, so a later
		// request of this key writes it.
		u.mu.Lock()
		delete(u.seen, keyID)
		u.mu.Unlock()
		return
	}
	wctx, cancel := ctxutil.Detach(ctx, apiKeyUpdateOpTimeout)
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer func() { <-u.sem }()
		defer cancel()
		if err := repo.UpdateLastUsed(wctx, userID, keyID, now.UTC()); err != nil {
			if errors.Is(err, dberrors.ErrNotFound) {
				slog.DebugContext(wctx, "API key last-used update found no key", "key_id", keyID)
				return
			}
			slog.WarnContext(wctx, "API key last-used update failed", "key_id", keyID, "error", err)
		}
	}()
}

// wait blocks until the running writes end (tests).
func (u *lastUsedUpdater) wait() {
	if u == nil {
		return
	}
	u.wg.Wait()
}

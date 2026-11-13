package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// The gin context keys of the identity that the auth middleware sets. Every
// value has the "auth." prefix, so a key of another middleware or handler
// (a plain "role" or "userID") can never collide with, or forge, an identity.
const (
	contextKeyUserID   = "auth.userID"
	contextKeyUsername = "auth.username"
	contextKeyRole     = "auth.role"
	contextKeyJTI      = "auth.jti"
	contextKeyExpiry   = "auth.tokenExpiry"
	contextKeySID      = "auth.sessionID"
	// contextKeyIssuedAt holds the issue time of the access token
	// (Claims.IssueTime, millisecond precision when the iat_ms claim is set).
	contextKeyIssuedAt = "auth.tokenIssuedAt"
	// contextKeyAuthMethod holds AuthMethodJWT or AuthMethodAPIKey.
	contextKeyAuthMethod = "auth.method"
)

// Values of GetAuthMethodFromContext.
const (
	AuthMethodJWT    = "jwt"
	AuthMethodAPIKey = "api_key"
)

// jwtSigningMethod is the only signing method that ValidateJWT accepts, and
// the one GenerateTokenWithOpts uses.
const jwtSigningMethod = "HS256"

// statusClientClosedRequest is the nginx status for a request that the
// client cancelled before the response (net/http has no constant for it).
// handlers.statusClientClosedRequest has the same value; the handlers package
// imports this one, so the constant cannot be shared.
const statusClientClosedRequest = 499

// Messages of the error answers. None of them contains an internal error.
const (
	msgAuthHeaderRequired = "Authorization header is required"
	msgAuthHeaderFormat   = "Authorization header must be Bearer {token}"
	msgAuthInvalid        = "Invalid or expired token"
	msgInternalError      = "Internal server error"
	msgClientClosed       = "Client closed request"
)

// Claims represents the JWT claims payload.
type Claims struct {
	UserID       string `json:"user_id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name,omitempty"`
	Role         string `json:"role"`
	AuthProvider string `json:"auth_provider,omitempty"`
	Email        string `json:"email,omitempty"`
	// SessionID is the refresh-token family of the login session ("sid").
	// Empty for tokens outside a refresh session.
	SessionID string `json:"sid,omitempty"`
	// IssuedAtMs is the issue time in Unix milliseconds ("iat_ms"). The JWT
	// iat has one-second precision; the user blocklist compares this claim
	// with the block time, so a login just after a password reset in the
	// same second is not revoked. 0 for tokens issued before the claim
	// existed.
	IssuedAtMs int64 `json:"iat_ms,omitempty"`
	jwt.RegisteredClaims
}

// IssueTime returns the issue time of the token for the user blocklist: the
// iat_ms claim (millisecond precision) when it is set, else the iat claim
// truncated to whole seconds. It returns the zero time when the token has
// neither claim.
func (c *Claims) IssueTime() time.Time {
	if c == nil {
		return time.Time{}
	}

	if c.IssuedAtMs > 0 {
		return time.UnixMilli(c.IssuedAtMs)
	}

	if c.IssuedAt != nil {
		return c.IssuedAt.Time.Truncate(time.Second)
	}

	return time.Time{}
}

// ValidateJWT parses and validates a JWT token string, returning the claims if
// valid.
//
//   - Only HS256 is accepted. The signing method is pinned, so "none", the
//     other HMAC sizes and any asymmetric method (including a token signed
//     with RSA whose public key is used as the HMAC secret) are refused.
//   - An empty secret never validates a token.
//   - The token must have an exp claim: every token the app issues has one
//     (GenerateTokenWithOpts), and a token without exp would never expire.
//   - The subject and the user_id claim must not be empty.
//
// The error is always the same text: it neither says why the token was
// refused nor contains the secret.
func ValidateJWT(tokenStr string, jwtSecret string) (*Claims, error) {
	errInvalid := errors.New("invalid or expired token")
	if jwtSecret == "" || tokenStr == "" {
		return nil, errInvalid
	}

	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if token.Method.Alg() != jwtSigningMethod {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		return []byte(jwtSecret), nil
	}, jwt.WithValidMethods([]string{jwtSigningMethod}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return nil, errInvalid
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || claims.Subject == "" || claims.UserID == "" {
		return nil, errInvalid
	}

	return claims, nil
}

// JWTAuthOptions configures AuthRequiredWithOptions.
type JWTAuthOptions struct {
	JWTSecret    string
	SessionStore sessionstore.SessionStore // nil: no revocation checks
	// FailClosedOnStoreError makes a failing session store lookup end the
	// request with 500 instead of letting it through. Production wiring
	// should set it. The default (false) fails open: access tokens are
	// short-lived and a database blip would otherwise lock out every user,
	// but a revoked token is then accepted while the lookup fails. A revocation that a
	// working lookup finds always wins, in both modes. A cancelled request
	// context gives 499 in both modes.
	FailClosedOnStoreError bool
}

// bearerToken returns the token of an "Authorization: Bearer <token>" header
// (the scheme is case-insensitive). The token is the rest of the header after
// the first space, unchanged: a header with extra spaces gives a token that
// does not validate. The token is never read from the query string.
func bearerToken(header string) (string, bool) {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}

	return parts[1], true
}

// AuthRequiredWithOptions returns JWT middleware with revocation checks. It
// answers:
//   - 401 for a missing or malformed header, an invalid or expired token, and
//     a revoked token (message from RevocationMessage). 401, not 403, so the
//     frontend sends the user back to login.
//   - 499 when the request context was cancelled during the revocation check.
//   - 500 when the revocation check failed and opts.FailClosedOnStoreError is
//     set.
func AuthRequiredWithOptions(opts JWTAuthOptions) gin.HandlerFunc {
	jwtSecret := opts.JWTSecret
	store := opts.SessionStore

	errLog := newStoreErrorLog()

	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			RecordJWTAuth(jwtOutcomeMissing)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgAuthHeaderRequired})
			return
		}

		tokenStr, ok := bearerToken(authHeader)
		if !ok {
			RecordJWTAuth(jwtOutcomeMalformed)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgAuthHeaderFormat})
			return
		}

		claims, err := ValidateJWT(tokenStr, jwtSecret)
		if err != nil {
			RecordJWTAuth(jwtOutcomeInvalid)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgAuthInvalid})
			return
		}

		outcome := jwtOutcomeSuccess
		revoked, lookupErr := RevocationStatus(c.Request.Context(), store, claims)
		if revoked != nil {
			RecordJWTAuth(jwtOutcomeRevoked)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": RevocationMessage(revoked)})
			return
		}

		if lookupErr != nil {
			if errors.Is(lookupErr, context.Canceled) {
				slog.DebugContext(c.Request.Context(), "Revocation check cancelled by the client", "error", lookupErr)
				RecordJWTAuth(jwtOutcomeCancelled)
				c.AbortWithStatusJSON(statusClientClosedRequest, gin.H{"error": msgClientClosed})

				return
			}

			errLog.log(c.Request.Context(), "Failed to check token, session or user blocklist",
				"jti", claims.ID, "user_id", claims.UserID, "fail_closed", opts.FailClosedOnStoreError, "error", lookupErr)

			if opts.FailClosedOnStoreError {
				RecordJWTAuth(jwtOutcomeError)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
				return
			}

			outcome = jwtOutcomeSuccessUnchecked
		}

		RecordJWTAuth(outcome)
		setJWTContext(c, claims)
		c.Next()
	}
}

// setJWTContext puts the identity of validated claims in the request context.
func setJWTContext(c *gin.Context, claims *Claims) {
	c.Set(contextKeyUserID, claims.UserID)
	c.Set(contextKeyUsername, claims.Username)
	c.Set(contextKeyRole, claims.Role)
	c.Set(contextKeyAuthMethod, AuthMethodJWT)

	if it := claims.IssueTime(); !it.IsZero() {
		c.Set(contextKeyIssuedAt, it)
	}

	if claims.ID != "" {
		c.Set(contextKeyJTI, claims.ID)
	}

	if claims.ExpiresAt != nil {
		c.Set(contextKeyExpiry, claims.ExpiresAt.Time)
	}

	if claims.SessionID != "" {
		c.Set(contextKeySID, claims.SessionID)
	}
}

// Revocation errors returned by RevocationStatus.
var (
	// ErrTokenRevoked means the jti of the token is on the token blocklist
	// (logout).
	ErrTokenRevoked = errors.New("token revoked")
	// ErrSessionRevoked means a block covers the token: its session was
	// blocked (logout of the session), or a user block covers it because it
	// was issued at or before the block (user deleted, disabled, password
	// reset, role change or logout-all).
	ErrSessionRevoked = errors.New("session revoked")
)

// RevocationStatus checks validated claims against the session store, in
// this order:
//  1. The token blocklist (jti). A match returns ErrTokenRevoked.
//  2. The session blocklist (the sid claim), for a token with a session. A
//     match (logout of the session) returns ErrSessionRevoked.
//  3. The user blocklist (user ID and issue time, see Claims.IssueTime). A
//     block revokes tokens issued at or before the block (millisecond
//     precision with the iat_ms claim, else the block second). A token
//     without iat gets a zero time and counts as blocked. A match returns
//     ErrSessionRevoked.
//
// A nil store or nil claims pass. It does not log and has no policy for a
// failed lookup. It returns the revocation (ErrTokenRevoked, ErrSessionRevoked or nil) and the
// store errors (nil when all checks ran). A failed check counts as not
// revoked; the other checks still run, and a revocation that one of them
// finds is returned together with the errors of the earlier ones.
//
// The caller must decide what a non-nil lookupErr means and must not ignore
// it: AuthRequiredWithOptions fails open or closed by
// JWTAuthOptions.FailClosedOnStoreError, and the WebSocket upgrade
// (WebSocketQueryToken with the same auth middleware) applies the same policy
// (production wiring fails closed: a non-nil lookupErr with no revocation
// refuses the connection).
func RevocationStatus(ctx context.Context, store sessionstore.SessionStore, claims *Claims) (revoked error, lookupErr error) {
	if store == nil || claims == nil {
		return nil, nil
	}

	var errs []error

	if claims.ID != "" {
		blocked, err := store.IsTokenBlocked(ctx, claims.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("token blocklist: %w", err))
		} else if blocked {
			return ErrTokenRevoked, errors.Join(errs...)
		}
	}

	if claims.SessionID != "" {
		blocked, err := store.IsSessionBlocked(ctx, claims.SessionID)
		if err != nil {
			errs = append(errs, fmt.Errorf("session blocklist: %w", err))
		} else if blocked {
			return ErrSessionRevoked, errors.Join(errs...)
		}
	}

	if claims.UserID != "" {
		blocked, err := store.IsUserBlocked(ctx, claims.UserID, claims.IssueTime())
		if err != nil {
			errs = append(errs, fmt.Errorf("user blocklist: %w", err))
		} else if blocked {
			return ErrSessionRevoked, errors.Join(errs...)
		}
	}

	return nil, errors.Join(errs...)
}

// RevocationMessage returns the client message for the revocation error of
// RevocationStatus.
func RevocationMessage(err error) string {
	if errors.Is(err, ErrTokenRevoked) {
		return "Token has been revoked"
	}

	return "Session revoked"
}

// GenerateTokenOptions holds all parameters for JWT generation.
type GenerateTokenOptions struct {
	UserID       string
	Username     string
	DisplayName  string
	Role         string
	Secret       string
	Expiration   time.Duration
	AuthProvider string // optional, included in claims when non-empty
	Email        string // optional, included in claims when non-empty
	SessionID    string // optional "sid" claim: refresh-token family of the session
}

// GenerateTokenWithOpts creates a signed HS256 JWT using the provided options.
func GenerateTokenWithOpts(opts GenerateTokenOptions) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:       opts.UserID,
		Username:     opts.Username,
		DisplayName:  opts.DisplayName,
		Role:         opts.Role,
		AuthProvider: opts.AuthProvider,
		Email:        opts.Email,
		SessionID:    opts.SessionID,
		IssuedAtMs:   now.UnixMilli(),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.New().String(),
			ExpiresAt: jwt.NewNumericDate(now.Add(opts.Expiration)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   opts.UserID,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	return token.SignedString([]byte(opts.Secret))
}

// GenerateToken creates a signed JWT token for the given user.
// Use GenerateTokenWithOpts when the token needs auth_provider, email or a
// session ID in its claims.
func GenerateToken(userID, username, role, secret string, expiration time.Duration) (string, error) {
	return GenerateTokenWithOpts(GenerateTokenOptions{
		UserID:     userID,
		Username:   username,
		Role:       role,
		Secret:     secret,
		Expiration: expiration,
	})
}

// stringFromContext returns the string stored under key, or "" when the key
// is missing or holds another type.
func stringFromContext(c *gin.Context, key string) string {
	if v, exists := c.Get(key); exists {
		if s, ok := v.(string); ok {
			return s
		}
	}

	return ""
}

// timeFromContext returns the time stored under key and whether it was set.
func timeFromContext(c *gin.Context, key string) (time.Time, bool) {
	if v, exists := c.Get(key); exists {
		if t, ok := v.(time.Time); ok {
			return t, true
		}
	}

	return time.Time{}, false
}

// GetUserIDFromContext extracts the user ID set by the auth middleware, or ""
// when the request is not authenticated.
func GetUserIDFromContext(c *gin.Context) string { return stringFromContext(c, contextKeyUserID) }

// GetUsernameFromContext extracts the username set by the auth middleware.
func GetUsernameFromContext(c *gin.Context) string {
	return stringFromContext(c, contextKeyUsername)
}

// GetRoleFromContext extracts the role set by the auth middleware.
func GetRoleFromContext(c *gin.Context) string { return stringFromContext(c, contextKeyRole) }

// GetJTIFromContext extracts the JWT ID (jti) set by the auth middleware.
func GetJTIFromContext(c *gin.Context) string { return stringFromContext(c, contextKeyJTI) }

// GetSessionIDFromContext returns the "sid" claim set by the JWT middleware,
// or "" for tokens without a session.
func GetSessionIDFromContext(c *gin.Context) string { return stringFromContext(c, contextKeySID) }

// GetTokenExpiryFromContext extracts the token expiry time set by the JWT
// middleware.
func GetTokenExpiryFromContext(c *gin.Context) (time.Time, bool) {
	return timeFromContext(c, contextKeyExpiry)
}

// GetTokenIssuedAtFromContext returns the issue time of the access token
// (iat_ms precision when the claim is set). The second value is false for a
// token without an issue time.
func GetTokenIssuedAtFromContext(c *gin.Context) (time.Time, bool) {
	return timeFromContext(c, contextKeyIssuedAt)
}

// GetAuthMethodFromContext returns AuthMethodJWT ("jwt") for a login session
// token or AuthMethodAPIKey ("api_key") for an API key, or "" when the request
// is not authenticated.
func GetAuthMethodFromContext(c *gin.Context) string {
	return stringFromContext(c, contextKeyAuthMethod)
}

package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Refresh session settings.
const (
	// refreshTokenCookieName is the httpOnly cookie that carries the raw
	// refresh token. With SECURE_COOKIES it gets the __Host- prefix (see
	// cookieName). Its Path is "/": a reverse proxy may mount the API under
	// a prefix it strips (the bundled nginx and the Vite proxy serve
	// /api/v1/... to the browser as /api/api/v1/...), so a narrower Path
	// would never be sent. The __Host- prefix needs Secure, Path=/ and no
	// Domain, and stops a sibling subdomain from setting the cookie.
	refreshTokenCookieName = "refresh_token"
	refreshCookiePath      = "/"
	// hostCookiePrefix makes a browser refuse a cookie that is not Secure,
	// has a Domain or has a Path other than "/".
	hostCookiePrefix = "__Host-"
	// refreshTokenBytes is the randomness of a raw refresh token (256 bits).
	// The raw token is base64url without padding, refreshTokenEncodedLen
	// characters. It is opaque (never a JWT) and stored only as its SHA-256.
	refreshTokenBytes      = 32
	refreshTokenEncodedLen = 43
	// maxRefreshTokensPerUser caps the active sessions of a user: a login
	// beyond it ends the oldest session (the source project's default).
	maxRefreshTokensPerUser = 10
	// fallbackRefreshTokenExpiration applies when the config has no refresh
	// token lifetime (AuthConfig.Validate requires one).
	fallbackRefreshTokenExpiration = 168 * time.Hour
	// The column sizes of refresh_tokens.user_agent and ip_address.
	maxUserAgentLen = 500
	maxIPAddressLen = 45
	// csrfHeader must be on every request to a cookie-authenticated endpoint.
	// A cross-site form cannot set it, and a cross-origin fetch that sets it
	// needs a CORS preflight, which the CORS middleware does not allow for
	// this header.
	csrfHeader = "X-Requested-With"
)

// Client messages of the session endpoints. Every refused refresh gets
// msgInvalidSession, so the answer never tells an unknown, expired, consumed
// or revoked token, an ended session or an account state apart.
const (
	msgInvalidSession    = "Invalid or expired session"
	msgRefreshNotEnabled = "Refresh tokens are not enabled"
	msgCrossSiteRefused  = "Cross-site request refused: send the X-Requested-With header"
)

// Reasons of a refused refresh, logged only (never in the answer).
const (
	refuseMalformed      = "missing_or_malformed"
	refuseUnknown        = "unknown_token"
	refuseExpired        = "token_expired"
	refuseMaxLifetime    = "session_max_lifetime"
	refuseIdle           = "session_idle"
	refuseSessionBlocked = "session_blocked"
	refuseUserGone       = "user_not_found"
	refuseUserDisabled   = "user_disabled"
	refuseSessionRevoked = "session_revoked"
	refuseReuse          = "token_reuse"
)

// errRefreshTokenConsumed aborts the rotation transaction when another
// request consumed or revoked the presented refresh token first.
var errRefreshTokenConsumed = errors.New("refresh token already consumed")

// RefreshResponse represents the response from the refresh endpoint.
type RefreshResponse struct {
	Token string `json:"token"`
}

// refreshSession identifies the login session (refresh-token family) that a
// new refresh token or access token belongs to.
type refreshSession struct {
	startedAt time.Time // login time; SESSION_MAX_LIFETIME counts from here
	familyID  string    // ID of the first token of the session; the "sid" claim
	isNew     bool      // at login: the first token uses familyID as its ID
}

// newRefreshSession starts a new session at login.
func newRefreshSession() refreshSession {
	return refreshSession{familyID: uuid.New().String(), startedAt: time.Now().UTC(), isNew: true}
}

// continuedSession is the session of a stored token.
func continuedSession(rt *models.RefreshToken) refreshSession {
	return refreshSession{familyID: rt.SessionFamily(), startedAt: rt.SessionStart()}
}

// Refresh godoc
// @Summary     Refresh access token
// @Description Issues a new access token for the session of the refresh token cookie and rotates the refresh token (the old one is consumed, a new cookie is set). The request needs the X-Requested-With header (any value) and must not be cross-site; otherwise 403. A session ends SESSION_MAX_LIFETIME after login and SESSION_IDLE_TIMEOUT after its last login or refresh; rotation never extends it, and the access token never outlives it. A token rotated less than REFRESH_REUSE_GRACE ago (two tabs refreshing at once) gets one more access token, once, without a new cookie. Any other reuse of a consumed token revokes the session and blocks its access tokens. A session that started before a user block (logout-all, a role change, disabling or deleting the user) is refused and revoked even when its refresh token was still active. The refresh cookie is named __Host-refresh_token unless SECURE_COOKIES=false (then refresh_token) and has Path /. Every refusal answers the same 401 and deletes the cookie.
// @Tags        auth
// @Produce     json
// @Param       X-Requested-With header   string true "Any value (CSRF protection), for example XMLHttpRequest"
// @Success     200              {object} RefreshResponse
// @Failure     401              {object} map[string]string "Invalid or expired session"
// @Failure     403              {object} map[string]string "Cross-site request refused"
// @Failure     500              {object} map[string]string
// @Failure     501              {object} map[string]string "Refresh tokens not enabled"
// @Router      /api/v1/auth/refresh [post]
func (h *AuthHandler) Refresh(c *gin.Context) {
	noStore(c)
	if h.refreshTokenRepo == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": msgRefreshNotEnabled})
		return
	}
	if !requireSameSiteRequest(c) {
		return
	}
	raw, err := c.Cookie(h.refreshCookieName())
	if err != nil || !wellFormedRefreshToken(raw) {
		h.refuseRefresh(c, refuseMalformed, "")
		return
	}

	// The reads that decide the request use the request context: a client
	// that leaves gets 499 and nothing changes. Every write after the token
	// is found uses detachedCtx, so a client that leaves cannot skip a
	// revoke or half-finish a rotation.
	tokenHash := hashRefreshToken(raw)
	stored, ok := h.findRefreshToken(c.Request.Context(), c, tokenHash)
	if !ok {
		return
	}

	now := time.Now().UTC()
	if stored.Revoked {
		h.refreshConsumedToken(c, stored, now)
		return
	}
	if reason := h.sessionEndReason(stored, now); reason != "" {
		if reason != refuseExpired {
			h.revokeFamilyBestEffort(c, stored.SessionFamily(), stored.UserID)
		}
		h.refuseRefresh(c, reason, stored.UserID)
		return
	}
	if !h.checkSessionNotBlocked(c, stored) {
		return
	}
	if !h.checkUserNotBlocked(c, stored) {
		return
	}
	user, ok := h.refreshUser(c, stored)
	if !ok {
		return
	}
	h.rotate(c, stored, tokenHash, user, now)
}

// rotate consumes the stored token and creates its successor in one
// transaction, then answers with a new access token for the re-read user.
// Consuming first takes the row lock, so two concurrent refreshes with the
// same token serialise and the second one sees 0 changed rows. The
// transaction repository is the only one used inside, so the lock retry of
// BaseRepository.Tx reruns the whole transaction.
func (h *AuthHandler) rotate(c *gin.Context, stored *models.RefreshToken, tokenHash string, user *models.User, now time.Time) {
	wctx, cancel := detachedCtx(c)
	defer cancel()

	sess := continuedSession(stored)
	var newRaw string
	var newExpiry time.Time
	err := h.refreshTokenRepo.WithTx(wctx, func(tx models.RefreshTokenRepository) error {
		n, err := tx.MarkRotatedIfActive(wctx, stored.ID, now)
		if err != nil {
			return err
		}
		if n == 0 {
			return errRefreshTokenConsumed
		}
		newRaw, newExpiry, err = h.createRefreshToken(wctx, c, tx, user.ID, sess, stored.ID)
		return err
	})
	if errors.Is(err, errRefreshTokenConsumed) {
		// Another request consumed the token after our read. Read it again,
		// so a concurrent rotation gets the grace rule and a revoke does not.
		if latest, ok := h.findRefreshToken(wctx, c, tokenHash); ok {
			h.refreshConsumedToken(c, latest, now)
		}
		return
	}
	if err != nil {
		h.log().Error("Failed to rotate refresh token", "user_id", user.ID, "session_id", sess.familyID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}

	access, err := h.sessionAccessToken(user, sess, now)
	if err != nil {
		h.log().Error("Failed to sign access token on refresh", "user_id", user.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}
	if !h.recheckIssued(c, wctx, user.ID, sess.familyID) {
		return
	}
	// The cookie is set only after the transaction committed and the
	// re-checks passed.
	h.setRefreshCookie(c, newRaw, newExpiry)
	middleware.RecordRefresh("success")
	c.JSON(http.StatusOK, RefreshResponse{Token: access})
}

// refreshConsumedToken handles a refresh with a token that is already
// revoked.
//
// Grace: a token that a rotation consumed less than RefreshReuseGrace ago
// gets one access token for its session, once (ClaimReuseGrace), when the
// user is active and the session still has an active token (two tabs refresh
// with the same cookie at the same time). The answer sets no cookie: the
// rotation already set the successor.
//
// Replay: any other reuse (a second reuse in the window, after the window, or
// a token revoked by logout or an admin) revokes the session family and
// blocks its session, so its access tokens stop working too. Only the family
// is revoked: the replayed token can only belong to it.
func (h *AuthHandler) refreshConsumedToken(c *gin.Context, stored *models.RefreshToken, now time.Time) {
	family := stored.SessionFamily()
	if h.withinReuseGrace(stored, now) && h.tryGraceRefresh(c, stored, family, now) {
		return
	}

	wctx, cancel := detachedCtx(c)
	defer cancel()
	revokeErr := h.refreshTokenRepo.RevokeFamily(wctx, family)
	blockErr := h.blockSessions(wctx, stored.UserID, []sessionToBlock{{id: family, revokeFailed: revokeErr != nil}})
	h.log().Warn("Refresh token reuse detected; session revoked",
		"user_id", stored.UserID, "session_id", family,
		"client_ip", c.ClientIP(), "request_id", c.GetString(middleware.RequestIDKey))
	if revokeErr != nil || blockErr != nil {
		// The session may stay usable on the server: an operator must see it.
		h.log().Error("Failed to end a replayed session", "user_id", stored.UserID, "session_id", family,
			"revoke_error", revokeErr, "block_error", blockErr)
		h.clearRefreshCookie(c)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}
	h.refuseRefresh(c, refuseReuse, stored.UserID)
}

// tryGraceRefresh answers a refresh with a consumed token inside the reuse
// grace window. It returns true when it wrote an answer (grace answer, 401,
// 499 or 500); false means the caller treats the token as a replay.
func (h *AuthHandler) tryGraceRefresh(c *gin.Context, stored *models.RefreshToken, family string, now time.Time) bool {
	reqCtx := c.Request.Context()
	user, err := h.userRepo.FindByID(reqCtx, stored.UserID)
	if err != nil {
		if isNotFoundError(err) {
			return false
		}
		h.respondServerError(c, "Failed to look up the user of a refresh", err, "user_id", stored.UserID)
		return true
	}
	if user.Disabled {
		return false
	}
	if err := recheckSessionActive(reqCtx, h.refreshTokenRepo, family); err != nil {
		if errors.Is(err, errSessionRevoked) {
			return false
		}
		h.respondServerError(c, "Failed to check a refresh session", err, "user_id", stored.UserID)
		return true
	}
	if blocked, answered := h.graceBlockedByStore(c, stored, family); answered {
		return true
	} else if blocked {
		return false
	}

	wctx, cancel := detachedCtx(c)
	defer cancel()
	won, err := h.refreshTokenRepo.ClaimReuseGrace(wctx, stored.ID, now.Add(-h.cfg.RefreshReuseGrace))
	if err != nil {
		h.respondServerError(c, "Failed to claim the refresh reuse grace", err, "user_id", stored.UserID)
		return true
	}
	if !won {
		return false // the grace was already spent: a replay
	}
	access, err := h.sessionAccessToken(user, continuedSession(stored), now)
	if err != nil {
		h.respondServerError(c, "Failed to sign access token on refresh", err, "user_id", user.ID)
		return true
	}
	if !h.recheckIssued(c, wctx, user.ID, family) {
		return true
	}
	c.JSON(http.StatusOK, RefreshResponse{Token: access})
	return true
}

// graceBlockedByStore checks the session block and the user block before a
// grace answer. blocked means the grace must not be given (the caller treats
// the token as a replay); answered means a store error was answered (fail
// closed: 499 or 500) and the caller returns.
func (h *AuthHandler) graceBlockedByStore(c *gin.Context, stored *models.RefreshToken, family string) (blocked, answered bool) {
	if h.sessionStore == nil {
		return false, false
	}
	ctx := c.Request.Context()
	sessionBlocked, err := h.sessionStore.IsSessionBlocked(ctx, family)
	if err != nil {
		h.respondServerError(c, "Failed to check the session block on refresh", err, "user_id", stored.UserID)
		return false, true
	}
	if sessionBlocked {
		return true, false
	}
	userBlocked, err := h.userBlocked(ctx, stored)
	if err != nil {
		h.respondServerError(c, "Failed to check the user block on refresh", err, "user_id", stored.UserID)
		return false, true
	}
	return userBlocked, false
}

// withinReuseGrace reports whether a consumed token may still get an access
// token: rotated (not revoked otherwise) at most RefreshReuseGrace ago, in a
// session that has not reached SESSION_MAX_LIFETIME.
func (h *AuthHandler) withinReuseGrace(stored *models.RefreshToken, now time.Time) bool {
	if h.cfg.RefreshReuseGrace <= 0 || stored.RotatedAt == nil {
		return false
	}
	if now.Sub(*stored.RotatedAt) > h.cfg.RefreshReuseGrace {
		return false
	}
	return !h.sessionExpired(stored, now)
}

// sessionEndReason returns why an active token can no longer refresh, or "".
// The idle time counts from LastActivity: the login or the last refresh,
// which sets it on the new token (there is no per-request activity tracking,
// so the granularity is the access token lifetime).
func (h *AuthHandler) sessionEndReason(stored *models.RefreshToken, now time.Time) string {
	switch {
	case !now.Before(stored.ExpiresAt):
		return refuseExpired
	case h.sessionExpired(stored, now):
		return refuseMaxLifetime
	case h.cfg.SessionIdleTimeout > 0 && now.Sub(stored.LastActivity) > h.cfg.SessionIdleTimeout:
		return refuseIdle
	}
	return ""
}

// checkSessionNotBlocked refuses a refresh of a session that the session
// store blocked (a logout or replay whose family revoke failed) and revokes
// the family. A store error fails closed (500; 499 for a cancel). It returns
// true when the refresh may go on.
func (h *AuthHandler) checkSessionNotBlocked(c *gin.Context, stored *models.RefreshToken) bool {
	if h.sessionStore == nil {
		return true
	}
	family := stored.SessionFamily()
	blocked, err := h.sessionStore.IsSessionBlocked(c.Request.Context(), family)
	if err != nil {
		h.respondServerError(c, "Failed to check the session block on refresh", err, "user_id", stored.UserID)
		return false
	}
	if blocked {
		h.revokeFamilyBestEffort(c, family, stored.UserID)
		h.refuseRefresh(c, refuseSessionBlocked, stored.UserID)
		return false
	}
	return true
}

// userBlocked reports whether a user block (logout-all, a role change, a
// password reset, disabling the user) was written at or after the start of the
// session of stored. The block revokes the refresh tokens too, but that revoke
// can fail or lose a race; this check makes the block end the session even
// then. A session that started after the block is not caught.
func (h *AuthHandler) userBlocked(ctx context.Context, stored *models.RefreshToken) (bool, error) {
	return h.sessionStore.IsUserBlocked(ctx, stored.UserID, stored.SessionStart())
}

// checkUserNotBlocked refuses a refresh for a session that started before a
// user block: it revokes the session and answers the generic 401. A store
// error fails closed (500, 499 for a cancelled request). It returns false
// when it wrote an answer.
func (h *AuthHandler) checkUserNotBlocked(c *gin.Context, stored *models.RefreshToken) bool {
	if h.sessionStore == nil {
		return true
	}
	blocked, err := h.userBlocked(c.Request.Context(), stored)
	if err != nil {
		h.respondServerError(c, "Failed to check the user block on refresh", err, "user_id", stored.UserID)
		return false
	}
	if blocked {
		h.revokeFamilyBestEffort(c, stored.SessionFamily(), stored.UserID)
		h.refuseRefresh(c, refuseSessionRevoked, stored.UserID)
		return false
	}
	return true
}

// refreshUser reads the user of the token. A deleted user ends the session, a
// disabled one ends every session of the user. It returns false when it
// wrote an answer.
func (h *AuthHandler) refreshUser(c *gin.Context, stored *models.RefreshToken) (*models.User, bool) {
	user, err := h.userRepo.FindByID(c.Request.Context(), stored.UserID)
	switch {
	case err == nil && !user.Disabled:
		return user, true
	case err == nil:
		wctx, cancel := detachedCtx(c)
		defer cancel()
		if err := h.refreshTokenRepo.RevokeAllForUser(wctx, stored.UserID); err != nil {
			h.log().Error("Failed to revoke the sessions of a disabled user", "user_id", stored.UserID, "error", err)
		}
		h.refuseRefresh(c, refuseUserDisabled, stored.UserID)
	case isNotFoundError(err):
		h.revokeFamilyBestEffort(c, stored.SessionFamily(), stored.UserID)
		h.refuseRefresh(c, refuseUserGone, stored.UserID)
	default:
		h.respondServerError(c, "Failed to look up the user of a refresh", err, "user_id", stored.UserID)
	}
	return nil, false
}

// recheckIssued runs the re-checks after an access token was signed: the user
// is still active (with the request context: a cancel gives 499 and revokes
// nothing) and the session still has an active token. It returns false when
// it wrote an answer.
func (h *AuthHandler) recheckIssued(c *gin.Context, wctx context.Context, userID, family string) bool {
	if err := recheckUserActive(c.Request.Context(), h.userRepo, userID); err != nil {
		if errors.Is(err, errAccountDisabled) || errors.Is(err, errUserGone) {
			if rerr := h.refreshTokenRepo.RevokeAllForUser(wctx, userID); rerr != nil {
				h.log().Error("Failed to revoke the sessions after the user re-check", "user_id", userID, "error", rerr)
			}
			h.refuseRefresh(c, refuseUserDisabled, userID)
			return false
		}
		h.respondServerError(c, "Refresh user re-check failed", err, "user_id", userID)
		return false
	}
	if err := recheckSessionActive(wctx, h.refreshTokenRepo, family); err != nil {
		if errors.Is(err, errSessionRevoked) {
			h.refuseRefresh(c, refuseSessionRevoked, userID)
			return false
		}
		h.respondServerError(c, "Refresh session re-check failed", err, "user_id", userID, "session_id", family)
		return false
	}
	return true
}

// findRefreshToken looks up a refresh token by hash with ctx. On failure it
// writes the answer (the generic 401 for an unknown token, 499 for a
// cancelled ctx, 500 for a repository error) and returns false.
func (h *AuthHandler) findRefreshToken(ctx context.Context, c *gin.Context, tokenHash string) (*models.RefreshToken, bool) {
	stored, err := h.refreshTokenRepo.FindByTokenHash(ctx, tokenHash)
	if err == nil {
		return stored, true
	}
	if isNotFoundError(err) {
		h.refuseRefresh(c, refuseUnknown, "")
		return nil, false
	}
	h.respondServerError(c, "Failed to look up refresh token", err)
	return nil, false
}

// refuseRefresh logs the reason, deletes the cookie and answers the generic
// 401.
func (h *AuthHandler) refuseRefresh(c *gin.Context, reason, userID string) {
	middleware.RecordRefresh("refused")
	h.log().Info("Refresh refused", "reason", reason, "user_id", userID,
		"client_ip", c.ClientIP(), "request_id", c.GetString(middleware.RequestIDKey))
	h.clearRefreshCookie(c)
	c.JSON(http.StatusUnauthorized, gin.H{"error": msgInvalidSession})
}

// respondServerError answers 499 for a cancelled request and otherwise logs
// msg with the error and answers 500 without detail. A passed deadline is a
// 500.
func (h *AuthHandler) respondServerError(c *gin.Context, msg string, err error, attrs ...any) {
	if respondCancelled(c, err) {
		return
	}
	middleware.RecordRefresh("error")
	h.log().Error(msg, append(attrs, "error", err, "request_id", c.GetString(middleware.RequestIDKey))...)
	c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
}

// revokeFamilyBestEffort revokes a session family whose refusal does not
// depend on the revoke (the stored token already proves the session ended);
// a failure is only logged.
func (h *AuthHandler) revokeFamilyBestEffort(c *gin.Context, family, userID string) {
	wctx, cancel := detachedCtx(c)
	defer cancel()
	if err := h.refreshTokenRepo.RevokeFamily(wctx, family); err != nil {
		h.log().Error("Failed to revoke an ended session", "user_id", userID, "session_id", family, "error", err)
	}
}

// sessionDeadline returns the end of a session that started at start, or the
// zero time without SESSION_MAX_LIFETIME.
func (h *AuthHandler) sessionDeadline(start time.Time) time.Time {
	if h.cfg.SessionMaxLifetime <= 0 {
		return time.Time{}
	}
	return start.Add(h.cfg.SessionMaxLifetime)
}

// sessionExpired reports whether the session of the token is older than
// SESSION_MAX_LIFETIME (counted from the login, kept by every rotation).
func (h *AuthHandler) sessionExpired(rt *models.RefreshToken, now time.Time) bool {
	deadline := h.sessionDeadline(rt.SessionStart())
	return !deadline.IsZero() && now.After(deadline)
}

// sessionAccessToken signs an access token of the session for the current
// user record (role, name and email as stored now). It lives
// ACCESS_TOKEN_EXPIRATION, cut to the session deadline.
func (h *AuthHandler) sessionAccessToken(user *models.User, sess refreshSession, now time.Time) (string, error) {
	ttl := h.cfg.AccessTokenExpiration
	if deadline := h.sessionDeadline(sess.startedAt); !deadline.IsZero() {
		ttl = min(ttl, deadline.Sub(now))
	}
	ttl = max(ttl, time.Second)
	return h.signToken(middleware.GenerateTokenOptions{
		UserID:       user.ID,
		Username:     user.Username,
		DisplayName:  user.DisplayName,
		Role:         user.Role,
		Secret:       h.cfg.JWTSecret,
		Expiration:   ttl,
		AuthProvider: authProviderOf(user),
		Email:        user.Email,
		SessionID:    sess.familyID,
	})
}

// createRefreshToken stores a new refresh token of the session in repo (the
// transaction repository during rotation) and returns the raw token and its
// expiry. The expiry is the earlier of now + REFRESH_TOKEN_EXPIRATION and the
// session deadline, so rotation never extends a session. It keeps the user
// within maxRefreshTokensPerUser by revoking the oldest active tokens, never
// excludeID (the token being rotated) and never one newer than those kept.
func (h *AuthHandler) createRefreshToken(ctx context.Context, c *gin.Context, repo models.RefreshTokenRepository, userID string, sess refreshSession, excludeID string) (string, time.Time, error) {
	active, err := repo.CountActiveForUser(ctx, userID)
	if err != nil {
		return "", time.Time{}, err
	}
	if active >= maxRefreshTokensPerUser {
		if _, err := repo.RevokeOldestForUser(ctx, userID, maxRefreshTokensPerUser-1, excludeID); err != nil {
			return "", time.Time{}, err
		}
	}

	raw, err := generateRefreshToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC()
	start := sess.startedAt
	if start.IsZero() {
		start = now
	}
	lifetime := h.cfg.RefreshTokenExpiration
	if lifetime <= 0 {
		lifetime = fallbackRefreshTokenExpiration
	}
	expires := now.Add(lifetime)
	if deadline := h.sessionDeadline(start); !deadline.IsZero() && deadline.Before(expires) {
		expires = deadline
	}
	rt := &models.RefreshToken{
		UserID:           userID,
		FamilyID:         sess.familyID,
		TokenHash:        hashRefreshToken(raw),
		ExpiresAt:        expires,
		LastActivity:     now,
		CreatedAt:        now,
		SessionStartedAt: start,
		UserAgent:        truncateAtRune(c.GetHeader("User-Agent"), maxUserAgentLen),
		IPAddress:        truncateAtRune(c.ClientIP(), maxIPAddressLen),
	}
	if sess.isNew {
		rt.ID = sess.familyID
	}
	if err := repo.Create(ctx, rt); err != nil {
		return "", time.Time{}, err
	}
	return raw, expires, nil
}

// generateRefreshToken returns a new opaque refresh token: refreshTokenBytes
// from crypto/rand, base64url without padding.
func generateRefreshToken() (string, error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashRefreshToken returns the SHA-256 hex digest of the raw token, the only
// form that is stored.
func hashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// wellFormedRefreshToken reports whether raw has the shape of a raw refresh
// token. Anything else is refused without a database query.
func wellFormedRefreshToken(raw string) bool {
	if len(raw) != refreshTokenEncodedLen {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil
}

// cookieName returns the name of a cookie: base with the __Host- prefix when
// the cookie is Secure (SECURE_COOKIES), base alone for plain-HTTP development
// (a browser refuses a __Host- cookie without Secure).
func cookieName(base string, secure bool) string {
	if secure {
		return hostCookiePrefix + base
	}
	return base
}

// refreshCookieName is the name the refresh cookie has with the current
// SECURE_COOKIES setting.
func (h *AuthHandler) refreshCookieName() string {
	return cookieName(refreshTokenCookieName, h.cfg.SecureCookies)
}

// refreshCookie returns the refresh cookie with value. Every write of the
// cookie (set and delete) uses the same attributes: httpOnly, Secure from
// SECURE_COOKIES, SameSite=Strict, host-only, Path "/" (so it fits the
// __Host- prefix and survives a proxy that strips a path prefix).
func (h *AuthHandler) refreshCookie(value string, maxAge int) *http.Cookie {
	// #nosec G124 -- HttpOnly and SameSite=Strict are fixed; Secure follows SECURE_COOKIES (default true, false only for plain-HTTP development).
	return &http.Cookie{
		Name:     h.refreshCookieName(),
		Value:    value,
		Path:     refreshCookiePath,
		MaxAge:   maxAge,
		Secure:   h.cfg.SecureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// setRefreshCookie sets the refresh cookie. Max-Age follows the token expiry,
// so the cookie never outlives the session.
func (h *AuthHandler) setRefreshCookie(c *gin.Context, raw string, expiresAt time.Time) {
	maxAge := max(int(time.Until(expiresAt)/time.Second), 1) // 0 would make a browser-session cookie
	http.SetCookie(c.Writer, h.refreshCookie(raw, maxAge))
}

// clearRefreshCookie deletes the refresh cookie (Max-Age=0).
func (h *AuthHandler) clearRefreshCookie(c *gin.Context) {
	http.SetCookie(c.Writer, h.refreshCookie("", -1))
}

// requireSameSiteRequest refuses a request to a cookie-authenticated endpoint
// that another site could have sent: it needs the X-Requested-With header
// (which a cross-site form cannot set and a cross-origin fetch can set only
// after a CORS preflight that the CORS middleware refuses) and must not be
// marked cross-site by the browser (Sec-Fetch-Site). SameSite=Strict on the
// cookie is the other layer. It answers 403 and returns false.
func requireSameSiteRequest(c *gin.Context) bool {
	if strings.TrimSpace(c.GetHeader(csrfHeader)) != "" && !strings.EqualFold(c.GetHeader("Sec-Fetch-Site"), "cross-site") {
		return true
	}
	c.JSON(http.StatusForbidden, gin.H{"error": msgCrossSiteRefused})
	return false
}

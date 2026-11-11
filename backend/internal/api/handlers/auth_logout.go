package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// sessionBlockSkew is added to every block lifetime. It covers a token
// signer whose clock is ahead of the replica that writes the block: its
// tokens expire later by that difference, and the block must outlast them.
const sessionBlockSkew = time.Minute

// sessionToBlock is a session to block and whether its family revoke failed
// (it then gets the long block).
type sessionToBlock struct {
	id           string
	userID       string // the owner when a credential proves it; "" if unknown
	revokeFailed bool
}

// sessionBlockUntil is the end of a session block: the latest expiry of an
// access token of the session plus sessionBlockSkew. Call it after the family
// revoke: every token of the session was signed before it.
func (h *AuthHandler) sessionBlockUntil() time.Time {
	return time.Now().Add(h.cfg.AccessTokenExpiration + sessionBlockSkew)
}

// sessionLongBlockUntil is the end of a block for a session whose family
// revoke failed: its refresh token works until SESSION_MAX_LIFETIME ends the
// session, so the block lasts that long.
func (h *AuthHandler) sessionLongBlockUntil() time.Time {
	return time.Now().Add(max(h.cfg.SessionMaxLifetime, h.cfg.AccessTokenExpiration) + sessionBlockSkew)
}

// userBlockUntil is the end of a user block: the longest access-token
// lifetime (a session token or one issued outside a session) plus
// sessionBlockSkew.
func (h *AuthHandler) userBlockUntil() time.Time {
	return time.Now().Add(max(h.cfg.AccessTokenExpiration, h.cfg.JWTExpiration) + sessionBlockSkew)
}

// sessionListWindow is how far back the expiry of a refresh-token row can lie
// while its session may still hold a live access token: the access token
// lifetime, the block margin and the reuse grace. Logout-all lists the
// families of rows that expire within it, and the cleanup keeps rows for it.
func (h *AuthHandler) sessionListWindow() time.Duration {
	return h.cfg.AccessTokenExpiration + sessionBlockSkew + h.cfg.RefreshReuseGrace
}

// blockSessions blocks the sessions in the session store, so no access token
// of them passes the JWT middleware. A session whose revoke failed gets the
// long block. It writes at most two batches and returns their errors.
func (h *AuthHandler) blockSessions(ctx context.Context, userID string, sessions []sessionToBlock) error {
	if h.sessionStore == nil || len(sessions) == 0 {
		return nil
	}
	var normal, long []string
	for _, s := range sessions {
		if s.revokeFailed {
			long = append(long, s.id)
		} else {
			normal = append(normal, s.id)
		}
	}
	var errs []error
	for _, batch := range []struct {
		until time.Time
		ids   []string
	}{
		{ids: normal, until: h.sessionBlockUntil()},
		{ids: long, until: h.sessionLongBlockUntil()},
	} {
		if len(batch.ids) == 0 {
			continue
		}
		if err := h.sessionStore.BlockSessions(ctx, batch.ids, batch.until); err != nil {
			h.log().Error("Failed to block sessions", "user_id", userID, "sessions", len(batch.ids), "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// waitPastMillisecond returns once the wall clock is in a later millisecond
// than at (at most one millisecond). The user block is inclusive to the
// millisecond: a token issued in the block's own millisecond is blocked.
// Logout-all waits after its block, so a login that starts after its answer
// gets a later iat_ms and works.
func waitPastMillisecond(at time.Time) {
	next := time.UnixMilli(at.UnixMilli() + 1)
	if d := time.Until(next); d > 0 {
		time.Sleep(d)
	}
}

// bearerClaims returns the claims of a valid bearer token of the request, or
// nil. Logout is a public route, so the handler parses the header itself; an
// expired or invalid token proves nothing and is ignored.
func (h *AuthHandler) bearerClaims(c *gin.Context) *middleware.Claims {
	token, ok := bearerTokenOf(c.GetHeader("Authorization"))
	if !ok {
		return nil
	}
	claims, err := middleware.ValidateJWT(token, h.cfg.JWTSecret)
	if err != nil {
		return nil
	}
	return claims
}

// bearerTokenOf returns the token of an "Authorization: Bearer <token>"
// header, with the rule of the JWT middleware (scheme case-insensitive).
func bearerTokenOf(header string) (string, bool) {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

// revokeLogoutSessions revokes the sessions that the logout request proves
// possession of: the family of the refresh token cookie and the session (sid)
// of a valid bearer token. When they differ it ends both: each credential
// alone could end its session. Each family is revoked only for the user that
// its credential names. It returns the sessions (also the ones whose revoke
// failed, so they get the long block), a user ID for the logs, and whether a
// lookup or revoke failed.
func (h *AuthHandler) revokeLogoutSessions(ctx context.Context, c *gin.Context, bearer *middleware.Claims) ([]sessionToBlock, string, bool) {
	var sessions []sessionToBlock
	userID := ""
	failed := false
	if h.refreshTokenRepo == nil {
		return nil, "", false
	}
	if raw, err := c.Cookie(refreshTokenCookieName); err == nil && wellFormedRefreshToken(raw) {
		stored, err := h.refreshTokenRepo.FindByTokenHash(ctx, hashRefreshToken(raw))
		switch {
		case err == nil:
			sessions = append(sessions, sessionToBlock{id: stored.SessionFamily(), userID: stored.UserID})
			userID = stored.UserID
		case !isNotFoundError(err):
			h.log().Error("Failed to look up refresh token on logout", "error", err)
			failed = true
		}
	}
	if bearer != nil && bearer.SessionID != "" {
		if len(sessions) == 0 || sessions[0].id != bearer.SessionID {
			sessions = append(sessions, sessionToBlock{id: bearer.SessionID, userID: bearer.UserID})
		}
		if userID == "" {
			userID = bearer.UserID
		}
	}
	for i := range sessions {
		if err := h.refreshTokenRepo.RevokeFamilyForUser(ctx, sessions[i].id, sessions[i].userID); err != nil {
			h.log().Error("Failed to revoke the session on logout", "user_id", sessions[i].userID, "session_id", sessions[i].id, "error", err)
			sessions[i].revokeFailed = true
			failed = true
		}
	}
	return sessions, userID, failed
}

// Logout godoc
// @Summary     Logout
// @Description Ends the sessions that the request proves: the session of the refresh token cookie and the session (sid) of a valid bearer token. It revokes their refresh tokens, blocks the sessions so that all their access tokens get 401, blocks the presented access token until its expiry, closes the WebSockets opened with it and deletes the cookie. Other sessions of the user keep working. An expired or invalid bearer token is ignored. A request without a cookie and without a valid bearer token ends nothing and answers 200; a repeated logout answers 200. The request needs the X-Requested-With header (any value) and must not be cross-site; otherwise 403. When a lookup, revoke or block fails, the other steps still run and the cookie is deleted, but the answer is 500.
// @Tags        auth
// @Produce     json
// @Param       X-Requested-With header   string true "Any value (CSRF protection), for example XMLHttpRequest"
// @Success     200              {object} map[string]string
// @Failure     403              {object} map[string]string "Cross-site request refused"
// @Failure     500              {object} map[string]string
// @Router      /api/v1/auth/logout [post]
func (h *AuthHandler) Logout(c *gin.Context) {
	noStore(c)
	if !requireSameSiteRequest(c) {
		return
	}
	// Detached: a client that leaves is still logged out.
	ctx, cancel := detachedCtx(c)
	defer cancel()

	bearer := h.bearerClaims(c)
	// Revoke first, then block: a refresh whose session re-check passed before
	// the revoke signed its token before the block, so the block covers it.
	sessions, userID, failed := h.revokeLogoutSessions(ctx, c, bearer)
	if err := h.blockSessions(ctx, userID, sessions); err != nil {
		failed = true
	}
	if bearer != nil && bearer.ID != "" {
		if h.sessionStore != nil && bearer.ExpiresAt != nil {
			if err := h.sessionStore.BlockToken(ctx, bearer.ID, bearer.ExpiresAt.Time); err != nil {
				h.log().Error("Failed to block the access token on logout", "user_id", bearer.UserID, "jti", bearer.ID, "error", err)
				failed = true
			}
		}
		if h.wsRevoker != nil {
			h.wsRevoker.DisconnectToken(bearer.ID)
		}
	}

	h.clearRefreshCookie(c)
	if failed {
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}
	h.log().Info("Logout", "user_id", userID, "sessions", len(sessions), "request_id", c.GetString(requestIDKey))
	c.JSON(http.StatusOK, gin.H{"message": "Logged out"})
}

// recentSessionsToBlock lists the sessions that logout-all blocks: the
// families of the user's refresh-token rows that expire within
// sessionListWindow (revoked or not: a family revoked by the idle timeout or
// the session cap can still hold a live access token), plus the sid of the
// presented token. Run it after the revoke, so a login that committed before
// the revoke is listed too.
func (h *AuthHandler) recentSessionsToBlock(ctx context.Context, c *gin.Context, userID string) ([]sessionToBlock, error) {
	var sessions []sessionToBlock
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			sessions = append(sessions, sessionToBlock{id: id, userID: userID})
		}
	}
	var listErr error
	if h.refreshTokenRepo != nil {
		families, err := h.refreshTokenRepo.ListRecentFamiliesForUser(ctx, userID, time.Now().Add(-h.sessionListWindow()))
		if err != nil {
			h.log().Error("Failed to list the sessions on logout-all", "user_id", userID, "error", err)
			listErr = err
		}
		if len(families) >= models.MaxRecentFamilies {
			h.log().Warn("Logout-all session list hit its limit; the user block covers the rest", "user_id", userID, "limit", models.MaxRecentFamilies)
		}
		for _, id := range families {
			add(id)
		}
	}
	add(middleware.GetSessionIDFromContext(c))
	return sessions, listErr
}

// LogoutAll godoc
// @Summary     Logout from all sessions
// @Description Ends every session of the current user: revokes all their refresh tokens, blocks every session that can still hold a live access token, writes a user block that rejects every access token issued up to now, blocks the presented access token, closes all WebSockets of the user and deletes the cookie. A login after the answer works. It is idempotent. When a step fails, the other steps still run and the answer is 500.
// @Tags        auth
// @Produce     json
// @Success     200 {object} map[string]string
// @Failure     401 {object} map[string]string
// @Failure     500 {object} map[string]string
// @Router      /api/v1/auth/logout-all [post]
func (h *AuthHandler) LogoutAll(c *gin.Context) {
	noStore(c)
	userID := middleware.GetUserIDFromContext(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgAuthRequired})
		return
	}
	// Detached: a client that leaves is still logged out everywhere.
	ctx, cancel := detachedCtx(c)
	defer cancel()

	// Order: revoke, list, block the sessions, block the user. Revoke before
	// block, so a refresh that passed its session re-check before the revoke
	// signed its token before the blocks.
	failed, revokeFailed := false, false
	if h.refreshTokenRepo != nil {
		if err := h.refreshTokenRepo.RevokeAllForUser(ctx, userID); err != nil {
			h.log().Error("Failed to revoke all sessions", "user_id", userID, "error", err)
			failed, revokeFailed = true, true
		}
	}
	sessions, listErr := h.recentSessionsToBlock(ctx, c, userID)
	if listErr != nil {
		failed = true
	}
	for i := range sessions {
		sessions[i].revokeFailed = revokeFailed
	}
	if err := h.blockSessions(ctx, userID, sessions); err != nil {
		failed = true
	}
	if !h.blockUserAndToken(ctx, c, userID) {
		failed = true
	}
	if h.wsRevoker != nil {
		h.wsRevoker.DisconnectUser(userID)
	}

	h.clearRefreshCookie(c)
	if failed {
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}
	h.log().Info("Logout from all sessions", "user_id", userID, "sessions", len(sessions), "request_id", c.GetString(requestIDKey))
	c.JSON(http.StatusOK, gin.H{"message": "Logged out from all sessions"})
}

// blockUserAndToken writes the user block of logout-all (every access token
// of the user issued up to now) and blocks the presented access token. It
// returns false when a write failed. Without a session store it does nothing.
func (h *AuthHandler) blockUserAndToken(ctx context.Context, c *gin.Context, userID string) bool {
	if h.sessionStore == nil {
		return true
	}
	ok := true
	if err := h.sessionStore.BlockUser(ctx, userID, h.userBlockUntil()); err != nil {
		h.log().Error("Failed to block the user on logout-all", "user_id", userID, "error", err)
		ok = false
	} else {
		// R18: the block is inclusive to the millisecond. Answer only after
		// the block millisecond, so a login that follows the answer gets a
		// later iat_ms and is not caught by it.
		waitPastMillisecond(time.Now())
	}
	if jti := middleware.GetJTIFromContext(c); jti != "" {
		expiry, found := middleware.GetTokenExpiryFromContext(c)
		if !found {
			expiry = h.userBlockUntil()
		}
		if err := h.sessionStore.BlockToken(ctx, jti, expiry); err != nil {
			h.log().Error("Failed to block the access token on logout-all", "user_id", userID, "jti", jti, "error", err)
			ok = false
		}
	}
	return ok
}

// CleanupExpiredTokens deletes the refresh tokens that expired more than
// sessionListWindow ago, consumed or not. A row that expired more recently
// can belong to a session that still holds a live access token (logout-all
// must still find it), and a consumed row that has not expired is kept so its
// reuse is still detected. Active rows are never deleted. The leader worker
// calls it with its own context; a context error is returned wrapped
// (errors.Is finds it).
func (h *AuthHandler) CleanupExpiredTokens(ctx context.Context) error {
	if h.refreshTokenRepo == nil {
		return nil
	}
	deleted, err := h.refreshTokenRepo.DeleteExpired(ctx, time.Now().Add(-h.sessionListWindow()))
	if err != nil {
		return fmt.Errorf("delete expired refresh tokens: %w", err)
	}
	if deleted > 0 {
		h.log().Info("Cleaned up expired refresh tokens", "count", deleted)
	}
	return nil
}

package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// Client messages of the password change.
const (
	msgCurrentPasswordIncorrect = "Current password is incorrect"
	msgPasswordUnchanged        = "The new password must differ from the current password"
	msgPasswordFieldsRequired   = "current_password and new_password are required"
	msgLocalPasswordDisabled    = "Password sign-in is disabled"
	msgPasswordChanged          = "Password changed"
)

// ChangePasswordRequest is the request body of PUT /auth/me/password. Any
// other field is refused.
type ChangePasswordRequest struct {
	// CurrentPassword is the password that the account has now.
	CurrentPassword string `json:"current_password"`
	// NewPassword is the new password: at least 8 characters, at most 72 bytes,
	// and not the current one.
	NewPassword string `json:"new_password"`
}

// passwordChangeRefusal checks the account state that a password change needs
// and writes the refusal. It returns false when it answered. The state is
// checked twice (before the password check and again before the write), so a
// disable or a mode change that commits during the bcrypt work applies.
func (h *AuthHandler) passwordChangeRefusal(c *gin.Context, user *models.User) bool {
	switch {
	case user.Disabled:
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgAccountDisabled})
	case authProviderOf(user) != models.AuthProviderLocal || user.PasswordHash == "":
		c.JSON(http.StatusBadRequest, gin.H{"error": msgPasswordNotLocal})
	case h.oidcCfg != nil && h.oidcCfg.Enabled && !h.oidcCfg.LocalAuth && !user.ServiceAccount:
		// OIDC is the only interactive login: only the break-glass service
		// account uses a password (the rule of the password login).
		c.JSON(http.StatusForbidden, gin.H{"error": msgLocalPasswordDisabled})
	default:
		return true
	}
	return false
}

// ChangePassword godoc
// @Summary     Change the own password
// @Description Changes the password of the signed-in local user. Login session only (an API key is refused with 403). The body needs the current password and the new one (at least 8 characters, at most 72 bytes, not equal to the current one); unknown fields are refused. A wrong current password answers 401 "Current password is incorrect" and counts toward the same per-account failure lock as the password login (5 failures within 15 minutes lock the account for 15 minutes; the locked account answers 429 with Retry-After, also for the right password). A user with no local password (an SSO account) gets 400; with OIDC as the only interactive login (OIDC_LOCAL_AUTH=false) only the service account may change a password (403 otherwise); a disabled user gets 401. On success only the password hash is written and every other session of the user ends: their refresh tokens are revoked and their sessions are blocked, so their access tokens get 401; the open WebSockets of those sessions are closed by the next revalidation (within about a minute). The current session keeps working: its cookie, its access token and its socket are not touched. API keys are kept: delete them separately when the old password may have been exposed. The entry of the audit log has the action change_password and never holds a password. When the password was stored but a revocation step failed, the answer is 500, the new password stands and the entry has revocation_failed; end the remaining sessions with logout-all or change the password again. The login rate limit (LOGIN_RATE_LIMIT per client IP) applies.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       request body     ChangePasswordRequest true "Current and new password"
// @Success     200     {object} map[string]string
// @Failure     400     {object} map[string]string
// @Failure     401     {object} map[string]string
// @Failure     403     {object} map[string]string "An API key cannot change a password, or password sign-in is disabled for the account"
// @Failure     404     {object} map[string]string
// @Failure     413     {object} map[string]string
// @Failure     429     {object} map[string]string
// @Failure     499     {object} map[string]string "Client closed request"
// @Failure     500     {object} map[string]string
// @Security    BearerAuth
// @Router      /api/v1/auth/me/password [put]
func (h *AuthHandler) ChangePassword(c *gin.Context) {
	noStore(c)
	userID := middleware.GetUserIDFromContext(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgAuthRequired})
		return
	}
	// Only a login session may change the password: a stolen API key must not
	// take over the account. The route also has RequireSessionAuth; this check
	// does not rely on the wiring.
	if middleware.GetAuthMethodFromContext(c) != middleware.AuthMethodJWT {
		c.JSON(http.StatusForbidden, gin.H{"error": middleware.MsgSessionRequired})
		return
	}

	var req ChangePasswordRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}
	// The checks that need no lookup come first: they cost no bcrypt work, use
	// no failure budget and tell nothing about the current password.
	if req.CurrentPassword == "" || req.NewPassword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgPasswordFieldsRequired})
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.NewPassword == req.CurrentPassword {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgPasswordUnchanged})
		return
	}

	ctx := c.Request.Context()
	user, ok := h.findPasswordChangeUser(ctx, c, userID)
	if !ok || !h.passwordChangeRefusal(c, user) {
		return
	}

	// Reserve a slot under the account key of the login throttle before the
	// password check: the failures count with the failed logins of the
	// account, and a parallel burst cannot get more guesses in than the
	// threshold. The settle releases the slot on every exit.
	attempt, retryAfter := h.throttle.begin(accountThrottleKey(user.ID))
	if attempt == nil {
		h.log().Warn("Password change throttled", "user_id", user.ID,
			"client_ip", c.ClientIP(), "request_id", c.GetString(middleware.RequestIDKey))
		c.Header("Retry-After", retryAfterSeconds(retryAfter))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": msgTooManyLoginAttempts})
		return
	}
	defer attempt.settle()

	// A wait for a bcrypt slot that ends with the request is not a credential
	// failure: 499 for a client that left, 500 for a deadline.
	waitEnded := func(err error) bool {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		if !respondCancelled(c, err) {
			h.log().Error("Password change ended while it waited for bcrypt", "user_id", user.ID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		}
		return true
	}
	refuse := func(reason string) {
		attempt.outcome = outcomeFailure
		h.log().Warn("Password change refused", "reason", reason, "user_id", user.ID,
			"client_ip", c.ClientIP(), "request_id", c.GetString(middleware.RequestIDKey))
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgCurrentPasswordIncorrect})
	}

	if reason := unusableForPassword(user, req.CurrentPassword); reason != "" {
		// A hash that cannot be compared (malformed) or a password bcrypt would
		// only check in part: spend the compare of a wrong password.
		if err := h.comparePassword(ctx, dummyPasswordHash(), truncateAtRune(req.CurrentPassword, maxPasswordBytes)); waitEnded(err) {
			return
		}
		refuse(reason)
		return
	}
	if err := h.comparePassword(ctx, []byte(user.PasswordHash), req.CurrentPassword); err != nil {
		if waitEnded(err) {
			return
		}
		reason := loginReasonPasswordMismatch
		if !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			reason = loginReasonHashInvalid
		}
		refuse(reason)
		return
	}

	hash, err := hashPassword(ctx, req.NewPassword)
	if err != nil {
		if waitEnded(err) {
			return
		}
		h.log().Error("Failed to hash the new password", "user_id", user.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		return
	}

	// Re-read the user after the bcrypt work: a disable, a mode change or an
	// admin password reset that committed meanwhile applies. A changed hash
	// means the password that was just verified is no longer the current one.
	fresh, ok := h.findPasswordChangeUser(ctx, c, user.ID)
	if !ok || !h.passwordChangeRefusal(c, fresh) {
		return
	}
	if fresh.PasswordHash != user.PasswordHash {
		h.log().Warn("Password change refused", "reason", loginReasonHashChanged, "user_id", user.ID,
			"client_ip", c.ClientIP(), "request_id", c.GetString(middleware.RequestIDKey))
		c.JSON(http.StatusUnauthorized, gin.H{"error": msgCurrentPasswordIncorrect})
		return
	}
	// The current password was proven: clear the failures of the account.
	attempt.outcome = outcomeSuccess
	if err := ctx.Err(); err != nil {
		if !respondCancelled(c, err) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		}
		return
	}

	// Detached from here on: a client that leaves must not stop the change
	// halfway or skip its revocation.
	wctx, cancel := detachedCtx(c)
	defer cancel()
	// Write only the password hash: a full-row save would overwrite a
	// concurrent role or disabled change.
	if err := h.userRepo.UpdatePassword(wctx, user.ID, hash); err != nil {
		status, message := mapError(err, "User")
		if status >= http.StatusInternalServerError {
			h.log().Error("Failed to store a changed password", "user_id", user.ID, "error", err)
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	// The password committed: the entry is recorded although a later step
	// fails (the route is under /auth/, and the table entry names the action).
	// The details never hold a password.
	middleware.SetAuditEntityID(c, user.ID)
	middleware.AuditCommitted(c)
	middleware.AddAuditDetail(c, "password_changed", true)

	if err := h.endOtherSessions(wctx, c, user.ID); err != nil {
		respondRevocationFailed(c)
		return
	}
	h.log().Info("Password changed", "user_id", user.ID, "request_id", c.GetString(middleware.RequestIDKey))
	c.JSON(http.StatusOK, gin.H{"message": msgPasswordChanged})
}

// findPasswordChangeUser reads the user on the request path. On a failure it
// writes the answer (499, 404 or 500) and returns false.
func (h *AuthHandler) findPasswordChangeUser(ctx context.Context, c *gin.Context, userID string) (*models.User, bool) {
	user, err := h.userRepo.FindByID(ctx, userID)
	if err != nil {
		status, message := mapError(err, "User")
		if status >= http.StatusInternalServerError {
			h.log().Error("Failed to look up the user of a password change", "user_id", userID, "error", err)
		}
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	return user, true
}

// endOtherSessions ends every session of the user except the one of the
// request: it revokes their refresh-token families, then lists the user's
// sessions and blocks all but the current one in the session store, so their
// access tokens get 401. It writes no user block (that would reject the
// current token too) and closes no socket: the hub cannot close the sockets
// of one session, and the revalidation closes the sockets of a blocked session
// within one interval. Order: revoke, list, block (a refresh that passed its
// session re-check before the revoke signed its token before the block).
// A failed step does not stop the others; the errors are joined.
func (h *AuthHandler) endOtherSessions(ctx context.Context, c *gin.Context, userID string) error {
	current := middleware.GetSessionIDFromContext(c)
	var errs []error
	revokeFailed := false
	var families []string
	if h.refreshTokenRepo != nil {
		if err := h.refreshTokenRepo.RevokeAllForUserExceptFamily(ctx, userID, current); err != nil {
			h.log().Error("Failed to revoke the other sessions after a password change", "user_id", userID, "error", err)
			errs = append(errs, err)
			revokeFailed = true
		}
		listed, err := h.refreshTokenRepo.ListRecentFamiliesForUser(ctx, userID, time.Now().Add(-h.sessionListWindow()))
		if err != nil {
			h.log().Error("Failed to list the sessions after a password change", "user_id", userID, "error", err)
			errs = append(errs, err)
		}
		if len(listed) >= models.MaxRecentFamilies {
			// A session beyond the limit has its refresh tokens revoked; its
			// live access token lasts at most ACCESS_TOKEN_EXPIRATION.
			h.log().Warn("Password change session list hit its limit", "user_id", userID, "limit", models.MaxRecentFamilies)
		}
		families = listed
	}
	sessions := make([]sessionToBlock, 0, len(families))
	for _, id := range families {
		if id != "" && id != current {
			sessions = append(sessions, sessionToBlock{id: id, userID: userID, revokeFailed: revokeFailed})
		}
	}
	if err := h.blockSessions(ctx, userID, sessions); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/ctxutil"
	"backend/internal/inflight"
	"backend/internal/logutil"
	"backend/internal/mail"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// Messages and limits of the password reset.
const (
	// msgPasswordResetAccepted is the one answer of POST /auth/password-reset
	// (202), whoever the username belongs to.
	msgPasswordResetAccepted = "If the account exists and can receive mail, a reset link has been sent."
	// msgResetLinkInvalid is the one answer of a confirm with a token that is
	// not usable: malformed, unknown, used, expired, or of an account that cannot
	// reset any more.
	msgResetLinkInvalid = "Invalid or expired token"
	msgUsernameRequired = "username is required"
	msgInvalidResetName = "Invalid username"

	// passwordResetMaxInFlight bounds the lookups and mails that run after
	// their answers. A request over the bound is dropped without a trace in the
	// answer: a flood of requests (from many addresses, past the per-address
	// limit) cannot grow the goroutines or the mail volume without limit.
	passwordResetMaxInFlight = 16
	// resetTokenBytes is the size of a raw token: 256 random bits.
	resetTokenBytes = 32
	// resetTokenLen is the length of a raw token: resetTokenBytes in unpadded
	// URL-safe base64.
	resetTokenLen = 43
	// resetMailSubject is the subject of the mail. It holds no input.
	resetMailSubject = "Reset your password"
)

// PasswordResetRequest is the request body of POST /auth/password-reset. Any
// other field is refused.
type PasswordResetRequest struct {
	// Username is the name of the local account that forgot its password.
	Username string `json:"username" example:"alice"`
}

// PasswordResetAcceptedResponse is the answer of POST /auth/password-reset. It
// is the same for every username.
type PasswordResetAcceptedResponse struct {
	Message string `json:"message" example:"If the account exists and can receive mail, a reset link has been sent."`
}

// PasswordResetConfirmRequest is the request body of POST
// /auth/password-reset/confirm. Any other field is refused.
type PasswordResetConfirmRequest struct {
	// Token is the token from the link of the mail (the fragment of the URL).
	Token string `json:"token"`
	// NewPassword is the new password: at least 8 characters, at most 72 bytes.
	NewPassword string `json:"new_password"`
}

// PasswordResetDeps are the dependencies of the PasswordResetHandler. Every
// field but Auth, Logger and SendTimeout is required.
type PasswordResetDeps struct {
	Users    models.UserRepository
	Tokens   models.PasswordResetTokenRepository
	Mailer   mail.Mailer
	InFlight *inflight.Tracker
	// Auth shares the login throttle: a reset clears the lock of the account.
	Auth *AuthHandler
	// Access ends the sessions of the user after a reset (the revocation of
	// the admin password reset).
	Access *UserHandler
	// Logger nil uses slog.Default() (tests set it).
	Logger *slog.Logger
	// AppTitle is the product name in the mail text (APP_TITLE).
	AppTitle string
	Config   config.PasswordResetConfig
	// SendTimeout bounds the mail send; 0 uses ctxutil.WriteTimeout. The
	// bootstrap passes the SMTP timeout plus a margin.
	SendTimeout time.Duration
}

// PasswordResetHandler handles the optional password reset: a mailed,
// single-use token that lets a local user set a new password.
//
// No answer depends on whether an account exists. The request route answers
// 202 at once with a fixed body, and the lookup, the throttle, the token
// insert and the mail run afterwards in tracked background work, so neither
// the status, the body, the headers nor the time of the answer tells. The
// confirm route answers one generic 400 for every token that cannot be used.
type PasswordResetHandler struct {
	users    models.UserRepository
	tokens   models.PasswordResetTokenRepository
	mailer   mail.Mailer
	tracker  *inflight.Tracker
	auth     *AuthHandler
	access   *UserHandler
	logger   *slog.Logger
	sem      chan struct{}
	baseURL  string
	appTitle string
	// storeLog and mailLog let one Error line through per interval: a database
	// or mail outage fails every request, and the other lines go to Debug.
	storeLog    logutil.Limiter
	mailLog     logutil.Limiter
	ttl         time.Duration
	minInterval time.Duration
	sendTimeout time.Duration
}

// NewPasswordResetHandler creates the handler.
func NewPasswordResetHandler(d PasswordResetDeps) *PasswordResetHandler {
	sendTimeout := d.SendTimeout
	if sendTimeout <= 0 {
		sendTimeout = ctxutil.WriteTimeout
	}

	return &PasswordResetHandler{
		users:       d.Users,
		tokens:      d.Tokens,
		mailer:      d.Mailer,
		tracker:     d.InFlight,
		auth:        d.Auth,
		access:      d.Access,
		logger:      d.Logger,
		sem:         make(chan struct{}, passwordResetMaxInFlight),
		baseURL:     strings.TrimRight(d.Config.URL, "/"),
		appTitle:    cleanLine(d.AppTitle),
		ttl:         d.Config.TokenTTL,
		minInterval: d.Config.MinInterval,
		sendTimeout: sendTimeout,
	}
}

func (h *PasswordResetHandler) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}

	return slog.Default()
}

// resetEligible reports whether the account may reset its password by mail: a
// local account (not an SSO one: the identity provider owns its credentials),
// enabled, with an email address, and not a service account. The bootstrap
// admin is the break-glass account for the case that the mail or the identity
// provider is down: a reset mail to a shared mailbox would make that mailbox a
// way to take it over.
func resetEligible(u *models.User) bool {
	return authProviderOf(u) == models.AuthProviderLocal &&
		!u.Disabled &&
		!u.ServiceAccount &&
		strings.TrimSpace(u.Email) != ""
}

// resetTokenHash is the stored form of a raw token.
func resetTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))

	return hex.EncodeToString(sum[:])
}

// newResetToken returns a raw token: 256 random bits, unpadded URL-safe base64.
func newResetToken() (string, error) {
	b := make([]byte, resetTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

// resetTokenShaped reports whether s can be a token that this module issued.
// It saves the lookup of a value that cannot match, and it is the only check
// that differs between a bad and an unknown token, which answer the same.
func resetTokenShaped(s string) bool {
	if len(s) != resetTokenLen {
		return false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return false
		}
	}

	return true
}

// cleanLine replaces every control character with a space and trims the
// result: a value that goes into the body of the mail on one line.
func cleanLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) || r == ' ' || r == ' ' {
			return ' '
		}

		return r
	}, s))
}

// validityText says how long a token lives, for the mail ("1 hour 30 minutes").
func validityText(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}

		return strconv.Itoa(n) + " " + unit + "s"
	}

	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)

	switch {
	case hours == 0:
		return plural(max(minutes, 1), "minute")
	case minutes == 0:
		return plural(hours, "hour")
	default:
		return plural(hours, "hour") + " " + plural(minutes, "minute")
	}
}

// message builds the mail: plain text, the link with the token in the URL
// fragment (a fragment is never sent to a server, so it reaches no server log
// and no Referer), the account name, the validity and the line that it can be
// ignored. It holds no password and no secret but the token.
func (h *PasswordResetHandler) message(user *models.User, raw string) mail.Message {
	title := h.appTitle
	if title == "" {
		title = "the application"
	}

	link := h.baseURL + "/reset-password#token=" + raw

	var b strings.Builder

	fmt.Fprintf(&b, "Hello,\n\n")
	fmt.Fprintf(&b, "A password reset was requested for the account %q on %s.\n\n", cleanLine(user.Username), title)
	fmt.Fprintf(&b, "To choose a new password, open this link. It is valid for %s and works once:\n\n", validityText(h.ttl))
	fmt.Fprintf(&b, "%s\n\n", link)
	fmt.Fprintf(&b, "If you did not ask for this, you can ignore this message: your password stays as it is.\n")

	return mail.Message{To: strings.TrimSpace(user.Email), Subject: resetMailSubject, Body: b.String()}
}

// Request godoc
// @Summary     Request a password reset mail
// @Description Sends a mail with a single-use reset link to the local account with this username, when the account exists, is enabled, has an email address, is not a service account (the bootstrap admin) and is not an SSO account. Public. Only available when PASSWORD_RESET_ENABLED=true (otherwise 404). The answer is always 202 with the same body, whether or not the account exists or got a mail (an unknown, ineligible, throttled or mailed account, and a mail failure, cannot be told apart by the status, the body, the headers or the time): the lookup and the send run after the answer. At most one mail per account in PASSWORD_RESET_MIN_INTERVAL; a request in that time sends nothing. The link is PASSWORD_RESET_URL + /reset-password#token=... and the token is valid for PASSWORD_RESET_TOKEN_TTL. A request that asks again replaces the unused earlier token. Unknown fields are refused. It has its own LOGIN_RATE_LIMIT budget per client IP. No cookie is set or read.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       request body     PasswordResetRequest true "The username of the account"
// @Success     202     {object} PasswordResetAcceptedResponse
// @Failure     400     {object} map[string]string "No username, a username that cannot exist, or an invalid body"
// @Failure     404     {object} map[string]string "The password reset is not enabled"
// @Failure     413     {object} map[string]string
// @Failure     429     {object} map[string]string
// @Router      /api/v1/auth/password-reset [post]
func (h *PasswordResetHandler) Request(c *gin.Context) {
	var req PasswordResetRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}

	// These answers depend on the input only, never on an account.
	username := strings.TrimSpace(req.Username)
	switch {
	case username == "":
		c.JSON(http.StatusBadRequest, gin.H{"error": msgUsernameRequired})
		return
	case len(username) > models.MaxUsernameLen:
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidResetName})
		return
	}

	h.startWork(username, c.GetString(middleware.RequestIDKey))

	noStore(c)
	c.JSON(http.StatusAccepted, PasswordResetAcceptedResponse{Message: msgPasswordResetAccepted})
}

// startWork hands the lookup, the throttle, the token insert and the mail to
// tracked background work. A request over the bound, or one that arrives after
// shutdown began, is dropped: the user asks again.
func (h *PasswordResetHandler) startWork(username, requestID string) {
	select {
	case h.sem <- struct{}{}:
	default:
		if ok, suppressed := h.storeLog.Allow(); ok {
			h.log().Warn("Password reset work dropped: too many in flight", "request_id", requestID, "suppressed", suppressed)
		}

		return
	}

	started := h.tracker.Go(func() {
		defer func() { <-h.sem }()

		h.process(username, requestID)
	})
	if !started {
		<-h.sem
		h.log().Debug("Password reset work dropped: shutdown has begun", "request_id", requestID)
	}
}

// process is the work of one request. It never answers anybody: every failure
// is logged and ends here.
func (h *PasswordResetHandler) process(username, requestID string) {
	ctx, cancel := ctxutil.Detach(context.Background(), ctxutil.WriteTimeout)
	defer cancel()

	user, err := h.users.FindByUsername(ctx, username)
	if err != nil {
		if !isNotFoundError(err) {
			h.logStoreFailure("look up the user of a password reset", err, requestID)
		}

		return
	}

	if !resetEligible(user) {
		h.log().Debug("Password reset not sent: the account is not eligible", "user_id", user.ID, "request_id", requestID)
		return
	}

	raw, err := newResetToken()
	if err != nil {
		h.logStoreFailure("generate a password reset token", err, requestID)
		return
	}

	// Build and check the mail before a token is stored: an address that the
	// mailer would refuse (a stored value that is not an address) must not leave
	// a token behind.
	msg := h.message(user, raw)
	if err := msg.Validate(); err != nil {
		h.log().Debug("Password reset not sent: the mail cannot be built", "user_id", user.ID, "error", err, "request_id", requestID)
		return
	}

	issued, err := h.tokens.Issue(ctx, user.ID, resetTokenHash(raw), h.ttl, h.minInterval)
	if err != nil {
		h.logStoreFailure("store a password reset token", err, requestID)
		return
	}

	if !issued {
		h.log().Debug("Password reset not sent: a mail went out recently", "user_id", user.ID, "request_id", requestID)
		return
	}

	sendCtx, sendCancel := ctxutil.Detach(context.Background(), h.sendTimeout)
	defer sendCancel()

	// One attempt: the user asks again after the minimum interval.
	if err := h.mailer.Send(sendCtx, msg); err != nil {
		// Never the error text: go-mail's SendError and the server's answer
		// hold the recipient address. mail.Describe gives the kind, the stage,
		// the reason and the SMTP code only.
		attrs := append([]any{"user_id", user.ID, "request_id", requestID}, mail.Describe(err)...)
		if ok, suppressed := h.mailLog.Allow(); ok {
			h.log().Error("Failed to send a password reset mail", append(attrs, "suppressed", suppressed)...)
		} else {
			h.log().Debug("Failed to send a password reset mail", attrs...)
		}

		return
	}

	h.log().Info("Password reset mail sent", "user_id", user.ID, "request_id", requestID)
}

func (h *PasswordResetHandler) logStoreFailure(what string, err error, requestID string) {
	if ok, suppressed := h.storeLog.Allow(); ok {
		h.log().Error("Failed to "+what, "error", err, "request_id", requestID, "suppressed", suppressed)
		return
	}

	h.log().Debug("Failed to "+what, "error", err, "request_id", requestID)
}

// Confirm godoc
// @Summary     Set a new password with a reset token
// @Description Sets a new password with the token from the mail. Public. Only available when PASSWORD_RESET_ENABLED=true (otherwise 404). The password follows the password rules (at least 8 characters, at most 72 bytes) and is checked first: a bad password answers 400 and does not use up the token. Then the token is consumed atomically (single use: of two requests with the same token exactly one succeeds) and the new password is stored. A token that is malformed, unknown, used or expired, or of an account that was disabled, deleted, turned into a service account or lost its email address, answers one generic 400 "Invalid or expired token". On success (204, no body) every session of the user ends the way an admin password reset ends it: the refresh tokens are revoked, every access token issued before is rejected (user block), the open WebSockets are closed and the failed-login lock of the account is cleared; the account can sign in with the new password at once, also when it was locked. API keys are kept: delete them separately when the account may have been exposed. The audit log gets an entry with the action password_reset (actor and entity: the account). When the password was stored but a revocation step failed, the answer is 500, the new password stands and the entry has revocation_failed. Unknown fields are refused. It has its own LOGIN_RATE_LIMIT budget per client IP. No cookie is set or read.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       request body     PasswordResetConfirmRequest true "The token and the new password"
// @Success     204     "The password was changed"
// @Failure     400     {object} map[string]string "Invalid or expired token, or a password that breaks the rules (the token is not used up)"
// @Failure     404     {object} map[string]string "The password reset is not enabled"
// @Failure     413     {object} map[string]string
// @Failure     429     {object} map[string]string
// @Failure     500     {object} map[string]string
// @Router      /api/v1/auth/password-reset/confirm [post]
func (h *PasswordResetHandler) Confirm(c *gin.Context) {
	noStore(c)

	var req PasswordResetConfirmRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		respondBindError(c, err)
		return
	}

	// The password first: a refused password must not use up the token.
	if err := validatePassword(req.NewPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !resetTokenShaped(req.Token) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgResetLinkInvalid})
		return
	}

	// Detached from here on: the token is used up below, and a client that
	// leaves must not stop the change halfway or skip its revocation.
	ctx, cancel := detachedCtx(c)
	defer cancel()

	requestID := c.GetString(middleware.RequestIDKey)

	userID, ok, err := h.tokens.Consume(ctx, resetTokenHash(req.Token))
	if err != nil {
		h.failConfirm(c, "consume a password reset token", err, requestID)
		return
	}

	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgResetLinkInvalid})
		return
	}

	user, err := h.users.FindByID(ctx, userID)
	if err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": msgResetLinkInvalid})
			return
		}

		h.failConfirm(c, "look up the user of a password reset", err, requestID)

		return
	}

	// The account may have changed since the mail: the same rule as the request.
	if !resetEligible(user) {
		h.log().Debug("Password reset refused: the account is not eligible any more", "user_id", user.ID, "request_id", requestID)
		c.JSON(http.StatusBadRequest, gin.H{"error": msgResetLinkInvalid})

		return
	}

	hash, err := hashPassword(ctx, req.NewPassword)
	if err != nil {
		h.failConfirm(c, "hash the password of a password reset", err, requestID)
		return
	}

	// Write only the password hash: a full-row save would overwrite a
	// concurrent role or disabled change.
	if err := h.users.UpdatePassword(ctx, user.ID, hash); err != nil {
		h.failConfirm(c, "store the password of a password reset", err, requestID)
		return
	}

	// The password committed. The caller has no session, so the handler names
	// the account that it reset (it proved control of the mailbox with the
	// token); the entry is recorded although a later step fails.
	middleware.SetAuditActor(c, user.ID, user.Username)
	middleware.SetAuditEntityID(c, user.ID)
	middleware.AuditCommitted(c)
	middleware.AddAuditDetail(c, "password_reset", true)

	if h.access != nil {
		if err := h.access.revokeUserAccess(ctx, user.ID, revokeOptions{}); err != nil {
			respondRevocationFailed(c)
			return
		}
	}

	// The owner proved control of the mailbox: a lock that guesses caused must
	// not keep them out. The account key covers every spelling of the name; the
	// name key is the stored one.
	if h.auth != nil && h.auth.throttle != nil {
		h.auth.throttle.clear(accountThrottleKey(user.ID))
		h.auth.throttle.clear(nameThrottleKey(user.Username))
	}

	// Other unused tokens of the user are useless now.
	dropPasswordResetTokens(ctx, h.log(), h.tokens, user.ID)

	h.log().Info("Password reset completed", "user_id", user.ID, "request_id", requestID, "client_ip", c.ClientIP())
	c.Status(http.StatusNoContent)
}

// failConfirm answers a failure of the confirm with 499 (the wait for bcrypt
// ended with the client, which cannot happen on the detached context, but the
// mapping is the one of every handler) or 500, and logs it. The answer never
// holds the error.
func (h *PasswordResetHandler) failConfirm(c *gin.Context, what string, err error, requestID string) {
	// An account that vanished meanwhile is an unusable token, not a 404.
	if isNotFoundError(err) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgResetLinkInvalid})
		return
	}

	status, message := mapError(err, "User")
	if status >= http.StatusInternalServerError {
		h.log().Error("Failed to "+what, "error", err, "request_id", requestID)
	}

	c.JSON(status, gin.H{"error": message})
}

// dropPasswordResetTokens deletes the unused reset tokens of a user, best
// effort: a password change (own or by an admin) and a completed reset make an
// older mail useless. A failure is logged and never fails the change that
// already committed. A nil repository (the module is off) does nothing.
func dropPasswordResetTokens(ctx context.Context, log *slog.Logger, repo models.PasswordResetTokenRepository, userID string) {
	if repo == nil {
		return
	}

	if _, err := repo.DeleteUnusedForUser(ctx, userID); err != nil {
		log.Warn("Failed to delete the unused password reset tokens of a user", "user_id", userID, "error", err)
	}
}

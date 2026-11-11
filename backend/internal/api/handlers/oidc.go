package handlers

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"backend/internal/auth"
	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
)

// OIDC login settings.
const (
	// oidcBindingCookieName is the cookie that ties a login to the browser that
	// started it (login CSRF protection). Its Path limits it to the OIDC routes.
	oidcBindingCookieName = "oidc_binding"
	oidcBindingCookiePath = "/api/v1/auth/oidc"
	// oidcExchangeTimeout bounds the token exchange and the ID token
	// validation of one callback (a request to the provider has its own, shorter
	// timeout).
	oidcExchangeTimeout = 15 * time.Second
	// The limits of the callback parameters. A state and a binding value of
	// this server are 43 characters; a code is short at every provider.
	maxOIDCStateLen   = 128
	maxOIDCCodeLen    = 4096
	maxOIDCBindingLen = 128
	// maxOIDCUsernameAttempts is the number of usernames a first login tries
	// when the username is taken by another account.
	maxOIDCUsernameAttempts = 3
	// generatedUsernamePrefix starts the username of an account whose verified
	// email is not available or already a username.
	generatedUsernamePrefix = "oidc_"
)

// Client messages of the OIDC routes. Every refusal that the user's own browser
// or the provider can cause answers the same 400, so an answer never tells a
// missing, expired, reused or foreign state, a refused code or a bad ID token
// apart.
const (
	msgOIDCLoginFailed         = "Login failed"
	msgOIDCProviderUnavailable = "Identity provider unavailable"
	msgOIDCNotPermitted        = "Login not permitted"
)

// errNoUsername: every username that a first login tried belongs to another
// account.
var errNoUsername = errors.New("no username available for the new OIDC account")

// OIDCProvider is the identity provider client that the handler uses
// (*auth.Provider).
type OIDCProvider interface {
	AuthCodeURL(ctx context.Context, state, challenge, nonce string) (string, error)
	Exchange(ctx context.Context, code, verifier, nonce string) (*auth.Identity, error)
}

// OIDCHandler handles the optional OpenID Connect login: a redirect to the
// identity provider and the callback that provisions the user and starts the
// same session as the password login. It is inert (404) unless both
// AUTH_ENABLED and OIDC_ENABLED are true.
//
// Rate limiting and the middleware of the two routes belong to the route
// registration: the login writes one row per call, so it needs the login rate
// limit.
type OIDCHandler struct {
	provider OIDCProvider
	auth     *AuthHandler // the session code of the password login is reused
	users    models.UserRepository
	store    sessionstore.SessionStore
	cfg      *config.OIDCConfig
	// exchangeTimeout is oidcExchangeTimeout (tests shorten it).
	exchangeTimeout time.Duration
}

// NewOIDCHandler creates an OIDCHandler. authHandler supplies the session
// (refresh token, cookie, re-checks) and must have its refresh token
// repository set: without refresh sessions the routes answer 501, because an
// access token must never travel in a redirect URL.
func NewOIDCHandler(provider OIDCProvider, authHandler *AuthHandler, users models.UserRepository, store sessionstore.SessionStore, cfg *config.OIDCConfig) *OIDCHandler {
	return &OIDCHandler{
		provider:        provider,
		auth:            authHandler,
		users:           users,
		store:           store,
		cfg:             cfg,
		exchangeTimeout: oidcExchangeTimeout,
	}
}

// enabled reports whether the OIDC login is on: OIDC_ENABLED and AUTH_ENABLED.
func (h *OIDCHandler) enabled() bool {
	return h != nil && h.cfg != nil && h.cfg.Enabled && h.auth != nil && h.auth.cfg != nil && h.auth.cfg.Enabled
}

// bindingCookie returns the browser binding cookie with value. HttpOnly, host
// only, limited to the OIDC routes, Secure from SECURE_COOKIES. SameSite is Lax,
// not Strict: the callback is a cross-site top-level navigation (the provider
// redirects the browser back), and a Strict cookie is not sent on it. Lax
// cookies are sent on top-level GET navigations only, never on cross-site
// sub-requests or POSTs, which is all the callback needs.
func (h *OIDCHandler) bindingCookie(value string, maxAge int) *http.Cookie {
	// #nosec G124 -- HttpOnly and SameSite=Lax are fixed (Lax is required for the cross-site redirect back from the provider); Secure follows SECURE_COOKIES (default true).
	return &http.Cookie{
		Name:     oidcBindingCookieName,
		Value:    value,
		Path:     oidcBindingCookiePath,
		MaxAge:   maxAge,
		Secure:   h.auth.cfg.SecureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *OIDCHandler) clearBindingCookie(c *gin.Context) {
	http.SetCookie(c.Writer, h.bindingCookie("", -1))
}

// redirect answers 302 without a body.
func redirect(c *gin.Context, location string) {
	c.Header("Location", location)
	c.Status(http.StatusFound)
	c.Writer.WriteHeaderNow()
}

// refuse answers the generic 400 of every login the user or the provider
// caused.
func refuse(c *gin.Context) {
	c.JSON(http.StatusBadRequest, gin.H{"error": msgOIDCLoginFailed})
}

// guard answers 404 when the OIDC login is off and 501 without refresh
// sessions, and reports whether the handler may go on.
func (h *OIDCHandler) guard(c *gin.Context) bool {
	if !h.enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": msgNotFound})
		return false
	}
	noStore(c)
	if h.auth.refreshTokenRepo == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": msgRefreshNotEnabled})
		return false
	}
	return true
}

// Login godoc
// @Summary     Start the OIDC login
// @Description Redirects the browser to the identity provider (authorization code flow with PKCE S256, a random state and a nonce). The state, the PKCE verifier and the nonce are kept on the server for OIDC_STATE_TTL and work once; a short-lived httpOnly cookie (oidc_binding, SameSite=Lax, Path /api/v1/auth/oidc) ties the login to this browser. Answers 404 unless AUTH_ENABLED and OIDC_ENABLED are true. No parameter of the request is used.
// @Tags        auth
// @Produce     json
// @Success     302 {string} string "Redirect to the identity provider"
// @Failure     404 {object} map[string]string
// @Failure     499 {object} map[string]string "Client closed request"
// @Failure     500 {object} map[string]string
// @Failure     501 {object} map[string]string "Refresh tokens not enabled"
// @Failure     502 {object} map[string]string "Identity provider unavailable"
// @Router      /api/v1/auth/oidc/login [get]
func (h *OIDCHandler) Login(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx := c.Request.Context()

	state, err := auth.RandomToken()
	if err != nil {
		h.serverError(c, "Failed to generate the OIDC state", err)
		return
	}
	nonce, err := auth.RandomToken()
	if err != nil {
		h.serverError(c, "Failed to generate the OIDC nonce", err)
		return
	}
	binding, err := auth.RandomToken()
	if err != nil {
		h.serverError(c, "Failed to generate the OIDC browser binding", err)
		return
	}
	verifier, challenge, err := auth.NewPKCE()
	if err != nil {
		h.serverError(c, "Failed to generate the PKCE verifier", err)
		return
	}

	authURL, err := h.provider.AuthCodeURL(ctx, state, challenge, nonce)
	if err != nil {
		h.failProvider(c, "login", err)
		return
	}
	data := sessionstore.OIDCStateData{CodeVerifier: verifier, Nonce: nonce, BindingHash: auth.BindingHash(binding)}
	if err := h.store.SaveOIDCState(ctx, state, data, h.cfg.StateTTL); err != nil {
		if respondCancelled(c, err) {
			return
		}
		h.serverError(c, "Failed to save the OIDC state", err)
		return
	}

	http.SetCookie(c.Writer, h.bindingCookie(binding, int(h.cfg.StateTTL.Seconds())))
	c.Header("Referrer-Policy", "no-referrer")
	redirect(c, authURL)
}

// Callback godoc
// @Summary     OIDC callback
// @Description The identity provider sends the browser here with a code and the state. The state works once and only from the browser that started the login (the oidc_binding cookie); the code is exchanged with the PKCE verifier; the ID token is validated (signature, issuer, audience, expiry, nonce). The account is found by issuer and sub, or created with auth_provider oidc and the role user (admin when a role of the OIDC_ADMIN_ROLES claim matches exactly); an existing OIDC account gets its role from the claim at every login. An existing local account is never linked or taken over. On success the answer sets the httpOnly refresh_token cookie and redirects (302) to the fixed web UI path OIDC_FRONTEND_PATH, without any token in the URL; the web UI then calls POST /api/v1/auth/refresh for its access token. Every refusal caused by the request or the provider answers 400 {"error":"Login failed"}.
// @Tags        auth
// @Produce     json
// @Param       code              query string false "Authorization code from the identity provider"
// @Param       state             query string false "State of the login"
// @Param       error             query string false "Error code from the identity provider (the login is refused)"
// @Param       error_description query string false "Ignored"
// @Success     302 {string} string "Redirect to the web UI"
// @Failure     400 {object} map[string]string "Login failed"
// @Failure     403 {object} map[string]string "Login not permitted (disabled account)"
// @Failure     404 {object} map[string]string
// @Failure     499 {object} map[string]string "Client closed request"
// @Failure     500 {object} map[string]string
// @Failure     501 {object} map[string]string "Refresh tokens not enabled"
// @Failure     502 {object} map[string]string "Identity provider unavailable"
// @Router      /api/v1/auth/oidc/callback [get]
func (h *OIDCHandler) Callback(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	data, code, ok := h.acceptCallback(c)
	if !ok {
		return
	}

	exchangeCtx, cancel := context.WithTimeout(c.Request.Context(), h.exchangeTimeout)
	ident, err := h.provider.Exchange(exchangeCtx, code, data.CodeVerifier, data.Nonce)
	cancel()
	if err != nil {
		h.failProvider(c, "callback", err)
		return
	}
	h.completeLogin(c, ident)
}

// acceptCallback checks the request before anything is sent to the provider:
// a usable state that works once, no error from the provider, the browser
// binding, a usable code. It answers every refusal itself and returns the
// stored login data and the code on success.
func (h *OIDCHandler) acceptCallback(c *gin.Context) (*sessionstore.OIDCStateData, string, bool) {
	log := h.auth.log()
	meta := []any{"client_ip", c.ClientIP(), "request_id", c.GetString(requestIDKey)}

	state := c.Query("state")
	if !wellFormedOIDCState(state) {
		log.Warn("OIDC callback without a usable state", meta...)
		h.clearBindingCookie(c)
		refuse(c)
		return nil, "", false
	}

	// The state is consumed first and works once, whatever else is wrong with
	// the request: a refused callback leaves nothing to retry.
	data, err := h.store.ConsumeOIDCState(c.Request.Context(), state)
	if err != nil {
		if !respondCancelled(c, err) {
			h.serverError(c, "Failed to read the OIDC state", err)
		}
		return nil, "", false
	}
	h.clearBindingCookie(c)
	if data == nil {
		log.Warn("OIDC callback with an unknown, expired or used state", meta...)
		refuse(c)
		return nil, "", false
	}
	if idpError := c.Query("error"); idpError != "" {
		// Only a short token is logged; the description and anything else the
		// provider sent is neither logged nor shown.
		log.Warn("OIDC provider reported an error", append(meta, "error_code", safeLogToken(idpError))...)
		refuse(c)
		return nil, "", false
	}
	if !bindingMatches(c, data) {
		log.Warn("OIDC callback from a browser that did not start the login", meta...)
		refuse(c)
		return nil, "", false
	}
	code := c.Query("code")
	if code == "" || len(code) > maxOIDCCodeLen {
		log.Warn("OIDC callback without a usable code", meta...)
		refuse(c)
		return nil, "", false
	}
	return data, code, true
}

// completeLogin finds or creates the user of the identity and starts the
// session. The writes are detached: a client that leaves must not leave the
// user sync or the session half done.
func (h *OIDCHandler) completeLogin(c *gin.Context, ident *auth.Identity) {
	ctx, cancel := detachedCtx(c)
	defer cancel()
	user, err := h.provisionUser(ctx, ident)
	if err != nil {
		h.failProvisioning(c, err)
		return
	}

	// Close the disable race: re-read the user before a session is stored.
	if err := recheckUserActive(ctx, h.users, user.ID); err != nil {
		if errors.Is(err, errAccountDisabled) || errors.Is(err, errUserGone) {
			h.auth.log().Warn("OIDC login refused: account changed during the login", "user_id", user.ID, "request_id", c.GetString(requestIDKey))
			c.JSON(http.StatusForbidden, gin.H{"error": msgOIDCNotPermitted})
			return
		}
		h.serverError(c, "Failed to re-check the OIDC user", err, "user_id", user.ID)
		return
	}

	fail := func(f loginFailure) {
		if f.httpCode == http.StatusUnauthorized {
			h.auth.log().Warn("OIDC login refused: the session was revoked during the login", "user_id", user.ID, "reason", f.reason, "request_id", c.GetString(requestIDKey))
			c.JSON(http.StatusForbidden, gin.H{"error": msgOIDCNotPermitted})
			return
		}
		h.serverError(c, "Failed to start the OIDC session", f.err, "user_id", user.ID, "reason", f.reason)
	}
	if !h.auth.startSession(c, user.ID, newRefreshSession(), fail) {
		return
	}

	h.auth.log().Info("OIDC login succeeded",
		"user_id", user.ID,
		"client_ip", c.ClientIP(),
		"request_id", c.GetString(requestIDKey))
	redirect(c, h.cfg.FrontendPath)
}

// bindingMatches reports whether the browser presents the binding value whose
// hash the state holds.
func bindingMatches(c *gin.Context, data *sessionstore.OIDCStateData) bool {
	raw, err := c.Cookie(oidcBindingCookieName)
	if err != nil || raw == "" || len(raw) > maxOIDCBindingLen || data.BindingHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(auth.BindingHash(raw)), []byte(data.BindingHash)) == 1
}

// wellFormedOIDCState reports whether s can be a state of this server: 1 to
// maxOIDCStateLen base64url characters. Anything else is refused without a
// store lookup.
func wellFormedOIDCState(s string) bool {
	if s == "" || len(s) > maxOIDCStateLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !(ch == '-' || ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
			return false
		}
	}
	return true
}

// safeLogToken returns s when it is a short token of letters, digits and
// ._- (an OAuth error code), and "invalid" otherwise.
func safeLogToken(s string) string {
	if s == "" || len(s) > 64 {
		return "invalid"
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !(ch == '-' || ch == '_' || ch == '.' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
			return "invalid"
		}
	}
	return s
}

// failProvider answers a failure of the provider call: 499 when the client
// left, 400 when the provider or the ID token refused the login, 502
// otherwise. The log has the fixed description of the error (see package
// auth), never a response body, a code or a secret.
func (h *OIDCHandler) failProvider(c *gin.Context, stage string, err error) {
	if cerr := c.Request.Context().Err(); cerr != nil {
		if respondCancelled(c, cerr) {
			return
		}
		h.serverError(c, "OIDC request context failed", cerr)
		return
	}
	if errors.Is(err, auth.ErrLoginRejected) {
		h.auth.log().Warn("OIDC login rejected", "stage", stage, "reason", err.Error(), "client_ip", c.ClientIP(), "request_id", c.GetString(requestIDKey))
		refuse(c)
		return
	}
	h.auth.log().Warn("OIDC provider failure", "stage", stage, "reason", err.Error(), "request_id", c.GetString(requestIDKey))
	c.JSON(http.StatusBadGateway, gin.H{"error": msgOIDCProviderUnavailable})
}

// failProvisioning answers a failure of the user lookup or sync.
func (h *OIDCHandler) failProvisioning(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errAccountDisabled):
		h.auth.log().Warn("OIDC login refused: account disabled", "client_ip", c.ClientIP(), "request_id", c.GetString(requestIDKey))
		c.JSON(http.StatusForbidden, gin.H{"error": msgOIDCNotPermitted})
	case errors.Is(err, errNoUsername):
		h.auth.log().Warn("OIDC login refused: no free username for the new account", "client_ip", c.ClientIP(), "request_id", c.GetString(requestIDKey))
		c.JSON(http.StatusForbidden, gin.H{"error": msgOIDCNotPermitted})
	default:
		h.serverError(c, "OIDC user provisioning failed", err)
	}
}

// serverError logs err and answers the generic 500.
func (h *OIDCHandler) serverError(c *gin.Context, msg string, err error, attrs ...any) {
	attrs = append(attrs, "error", err, "request_id", c.GetString(requestIDKey))
	h.auth.log().Error(msg, attrs...)
	c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
}

// provisionUser returns the user of the identity (issuer and sub): the existing
// account, synced with the provider, or a new one. It never links the identity
// to an account found by username or email: an OIDC login that matched a local
// account that way would let anyone who can set the email or username at the
// provider take that account over.
func (h *OIDCHandler) provisionUser(ctx context.Context, ident *auth.Identity) (*models.User, error) {
	extID := ident.ExternalID()
	user, err := h.users.FindByExternalID(ctx, models.AuthProviderOIDC, extID)
	if err == nil {
		return h.syncUser(ctx, user, ident)
	}
	if !isNotFoundError(err) {
		return nil, err
	}
	return h.createUser(ctx, ident, extID)
}

// syncUser writes the changed email, display name and role of the identity to
// the account. The role always follows the claim, a demotion included. Only the
// changed columns are written (a concurrent change of another column is not
// overwritten). A disabled account is refused and left unchanged. An email that
// the provider did not verify never replaces the stored one.
func (h *OIDCHandler) syncUser(ctx context.Context, user *models.User, ident *auth.Identity) (*models.User, error) {
	if user.Disabled {
		return nil, errAccountDisabled
	}
	var upd models.UserProfileUpdate
	changed := false
	if ident.Email != "" && user.Email != ident.Email {
		user.Email = ident.Email
		upd.Email = &user.Email
		changed = true
	}
	if ident.Name != "" && user.DisplayName != ident.Name {
		user.DisplayName = ident.Name
		upd.DisplayName = &user.DisplayName
		changed = true
	}
	if ident.Role != user.Role {
		if user.Role == models.RoleAdmin {
			// An admin loses the role because of the identity provider: say so
			// (an admin who set the role here sees it revert at the next login).
			h.auth.log().Warn("OIDC login lowers the role of an admin to the role from the identity provider",
				"user_id", user.ID, "old_role", user.Role, "new_role", ident.Role)
		}
		user.Role = ident.Role
		upd.Role = &user.Role
		changed = true
	}
	if changed {
		// A failed write ends the login: a session must not start with a role
		// or profile that was not stored.
		if err := h.users.UpdateProfile(ctx, user.ID, upd); err != nil {
			return nil, err
		}
	}
	return user, nil
}

// createUser creates the account of a first login. The username is the
// verified email when it fits and is free, otherwise a random generated name;
// a name taken by another account is never reused (that would be a link by
// name). When the create fails with a duplicate key, another request may have
// created this identity first: the user is read again by identity, which makes
// two concurrent first logins one account.
func (h *OIDCHandler) createUser(ctx context.Context, ident *auth.Identity, extID string) (*models.User, error) {
	for attempt := 0; attempt < maxOIDCUsernameAttempts; attempt++ {
		username := ""
		if attempt == 0 && ident.Email != "" && len(ident.Email) <= models.MaxUsernameLen {
			username = ident.Email
		}
		if username == "" {
			generated, err := generateUsername()
			if err != nil {
				return nil, err
			}
			username = generated
		}
		displayName := ident.Name
		if displayName == "" {
			displayName = username
		}
		user := &models.User{
			Username:     username,
			DisplayName:  displayName,
			Email:        ident.Email,
			Role:         ident.Role,
			AuthProvider: models.AuthProviderOIDC,
			ExternalID:   &extID,
		}
		err := h.users.Create(ctx, user)
		if err == nil {
			h.auth.log().Info("OIDC user provisioned", "user_id", user.ID, "role", user.Role)
			return user, nil
		}
		if !errors.Is(err, dberrors.ErrDuplicateKey) {
			return nil, err
		}
		existing, ferr := h.users.FindByExternalID(ctx, models.AuthProviderOIDC, extID)
		if ferr == nil {
			return h.syncUser(ctx, existing, ident)
		}
		if !isNotFoundError(ferr) {
			return nil, ferr
		}
		h.auth.log().Warn("OIDC first login: the username belongs to another account; not linking, trying a generated one", "attempt", attempt+1)
	}
	return nil, errNoUsername
}

// generateUsername returns "oidc_" and 16 random hex characters (64 bits from
// crypto/rand). It is not derived from the identity: an attacker who knows a
// victim's sub cannot register the name in advance.
func generateUsername() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return generatedUsernamePrefix + hex.EncodeToString(b[:]), nil
}

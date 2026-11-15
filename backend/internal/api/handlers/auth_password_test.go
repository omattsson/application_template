package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

const (
	passwordURL = "/api/v1/auth/me/password"
	newPassword = "Brand-New-Pass-1"
)

// pwBody is the request body of a password change.
func pwBody(t *testing.T, current, next string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"current_password": current, "new_password": next})
	require.NoError(t, err)
	return string(b)
}

// changePassword sends PUT /auth/me/password with the access token.
func (e *adminEnv) changePassword(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(http.MethodPut, passwordURL, body, bearerOf(token))
}

// refreshStatus refreshes with the raw refresh token and returns the status.
func (e *adminEnv) refreshStatus(raw string) int {
	return e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"X-Requested-With": "XMLHttpRequest",
		"Cookie":           hostRefreshCookie + "=" + raw,
	}).Code
}

// passwordMatches reports whether the stored hash of the user matches password.
func (e *adminEnv) passwordMatches(t *testing.T, id, password string) bool {
	t.Helper()
	return bcrypt.CompareHashAndPassword([]byte(e.user(t, id).PasswordHash), []byte(password)) == nil
}

// A password change ends the other sessions and keeps the current one, its
// cookie, its access token and the API keys. The entry of the audit log names
// the caller and holds no password.
func TestChangePasswordEndsOtherSessionsAndKeepsTheCurrent(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	currentToken, currentRaw := e.login(t, "user-u")
	otherToken, otherRaw := e.login(t, "user-u")
	thirdToken, thirdRaw := e.login(t, "user-u")
	otherUserToken, otherUserRaw := e.login(t, "user-v")
	keyRaw, _ := e.newKey(t, idUserU)
	before := e.user(t, idUserU)

	w := e.changePassword(t, currentToken, pwBody(t, testPassword, newPassword))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, messageSchema, w.Body.Bytes()))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.NotContains(t, w.Body.String(), newPassword)

	// Only the hash changed.
	after := e.user(t, idUserU)
	assert.NotEqual(t, before.PasswordHash, after.PasswordHash)
	assert.True(t, e.passwordMatches(t, idUserU, newPassword))
	assert.False(t, e.passwordMatches(t, idUserU, testPassword))
	assert.Equal(t, before.Role, after.Role)
	assert.Equal(t, before.Disabled, after.Disabled)

	// The current session works: access token, refresh cookie, not blocked.
	currentSID := claimsOf(t, currentToken).SessionID
	require.NotEmpty(t, currentSID)
	assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(currentToken)))
	blocked, err := e.store.IsSessionBlocked(context.Background(), currentSID)
	require.NoError(t, err)
	assert.False(t, blocked, "the current session is not blocked")
	assert.Equal(t, http.StatusOK, e.refreshStatus(currentRaw))

	// The other sessions of the user end.
	for name, tok := range map[string]string{"other": otherToken, "third": thirdToken} {
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), name)
	}
	assert.Equal(t, http.StatusUnauthorized, e.refreshStatus(otherRaw))
	assert.Equal(t, http.StatusUnauthorized, e.refreshStatus(thirdRaw))

	// Another user is not affected; the API key and the sockets of the user
	// (no user-wide disconnect, which would close the current socket) stay.
	assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(otherUserToken)))
	assert.Equal(t, http.StatusOK, e.refreshStatus(otherUserRaw))
	assert.Equal(t, http.StatusOK, e.tokenStatus(keyHeader(keyRaw)), "API keys are kept")
	users, tokens := e.revoker.snapshot()
	assert.Empty(t, users, "DisconnectUser would close the current socket")
	assert.Empty(t, tokens)

	// The new password signs in, the old one does not.
	lw := e.do(http.MethodPost, "/api/v1/auth/login", loginBody(t, "user-u", newPassword),
		map[string]string{"X-Requested-With": "XMLHttpRequest"})
	assert.Equal(t, http.StatusOK, lw.Code, lw.Body.String())
	lw = e.do(http.MethodPost, "/api/v1/auth/login", loginBody(t, "user-u", testPassword),
		map[string]string{"X-Requested-With": "XMLHttpRequest"})
	assert.Equal(t, http.StatusUnauthorized, lw.Code)

	rows := e.auditRows(t)
	require.Len(t, rows, 1, "one entry, written although the route is under /auth/")
	assert.Equal(t, middleware.AuditActionChangePassword, rows[0].Action)
	assert.Equal(t, middleware.AuditEntityUser, rows[0].EntityType)
	assert.Equal(t, idUserU, rows[0].EntityID)
	assert.Equal(t, idUserU, rows[0].UserID)
	assert.Contains(t, rows[0].Details, `"password_changed":true`)
	assert.Contains(t, rows[0].Details, `"status":200`)
	for _, secret := range []string{testPassword, newPassword, "current_password", "new_password"} {
		assert.NotContains(t, rows[0].Details, secret)
	}
}

// A token without a session ID (issued outside a refresh session) cannot name
// a session to keep: every refresh session of the user ends.
func TestChangePasswordWithATokenWithoutASession(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	sessionToken, sessionRaw := e.login(t, "user-u")
	plain := tokenWith(t, idUserU, "user-u", models.RoleUser)

	w := e.changePassword(t, plain, pwBody(t, testPassword, newPassword))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(sessionToken)))
	assert.Equal(t, http.StatusUnauthorized, e.refreshStatus(sessionRaw))
	assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(plain)), "the presented token has no session to block")
	assert.True(t, e.passwordMatches(t, idUserU, newPassword))
}

// Every refusal leaves the password, the sessions and the audit log alone.
func TestChangePasswordRefusals(t *testing.T) {
	t.Parallel()
	long73 := strings.Repeat("a", 73)
	tests := []struct {
		name   string
		body   string
		want   int
		errMsg string
		// compares is the number of bcrypt compares that the request costs.
		compares int
	}{
		{name: "wrong current password", body: pwBody(t, "not-the-password", newPassword), want: http.StatusUnauthorized, errMsg: msgCurrentPasswordIncorrect, compares: 1},
		{name: "current password over 72 bytes costs a dummy compare", body: pwBody(t, long73, newPassword), want: http.StatusUnauthorized, errMsg: msgCurrentPasswordIncorrect, compares: 1},
		{name: "new password too short", body: pwBody(t, testPassword, "short"), want: http.StatusBadRequest, errMsg: msgPasswordTooShort},
		{name: "new password too long", body: pwBody(t, testPassword, long73), want: http.StatusBadRequest, errMsg: msgPasswordTooLong},
		{name: "new equals current", body: pwBody(t, testPassword, testPassword), want: http.StatusBadRequest, errMsg: msgPasswordUnchanged},
		{name: "current missing", body: `{"new_password":"` + newPassword + `"}`, want: http.StatusBadRequest, errMsg: msgPasswordFieldsRequired},
		{name: "new missing", body: `{"current_password":"` + testPassword + `"}`, want: http.StatusBadRequest, errMsg: msgPasswordFieldsRequired},
		{name: "empty object", body: `{}`, want: http.StatusBadRequest, errMsg: msgPasswordFieldsRequired},
		{name: "unknown field", body: `{"current_password":"` + testPassword + `","new_password":"` + newPassword + `","role":"admin"}`, want: http.StatusBadRequest, errMsg: msgInvalidRequestFormat},
		{name: "trailing data", body: pwBody(t, testPassword, newPassword) + `{}`, want: http.StatusBadRequest, errMsg: msgInvalidRequestFormat},
		{name: "malformed JSON", body: `{"current_password":`, want: http.StatusBadRequest, errMsg: msgInvalidRequestFormat},
		{name: "wrong types", body: `{"current_password":1,"new_password":2}`, want: http.StatusBadRequest, errMsg: msgInvalidRequestFormat},
		{name: "oversized body", body: `{"current_password":"` + strings.Repeat("a", 2<<20) + `"}`, want: http.StatusRequestEntityTooLarge, errMsg: msgRequestBodyTooLarge},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			rc := &recordingCompare{}
			e.auth.compareHash = rc.compare
			token, raw := e.login(t, "user-u")
			rc.mu.Lock()
			rc.hashes, rc.pwLens = nil, nil // the login compared once
			rc.mu.Unlock()
			before := e.user(t, idUserU).PasswordHash

			w := e.changePassword(t, token, tt.body)

			require.Equal(t, tt.want, w.Code, w.Body.String())
			assert.Equal(t, tt.errMsg, errorOf(t, w))
			assert.Len(t, rc.calls(), tt.compares)
			assert.Equal(t, before, e.user(t, idUserU).PasswordHash, "the password is unchanged")
			assert.Equal(t, http.StatusOK, e.refreshStatus(raw), "the session is unchanged")
			assert.Empty(t, e.auditRows(t), "a refused change is not audited")
		})
	}
}

// A wrong current password is the same answer whatever else is true of the
// request, and the body never shows the passwords.
func TestChangePasswordWrongPasswordAnswerIsGeneric(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	token, _ := e.login(t, "user-u")

	w := e.changePassword(t, token, pwBody(t, "guess-number-one", newPassword))

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error":"Current password is incorrect"}`, w.Body.String())
	assert.Empty(t, w.Header().Get("Retry-After"))
}

// The account with no local password (SSO, or no stored hash) cannot change
// one; an API key and an anonymous request are refused before the handler.
func TestChangePasswordAccountAndCredentialRules(t *testing.T) {
	t.Parallel()

	t.Run("SSO user is refused with 400", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.changePassword(t, tokenWith(t, idOIDC, "oidc-o", models.RoleUser), pwBody(t, "anything-at-all", newPassword))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Equal(t, msgPasswordNotLocal, errorOf(t, w))
		assert.Empty(t, e.user(t, idOIDC).PasswordHash, "no password is set for an SSO user")
	})
	t.Run("local user without a stored hash is refused with 400", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", idUserU).Update("password_hash", "").Error)
		w := e.changePassword(t, tokenWith(t, idUserU, "user-u", models.RoleUser), pwBody(t, testPassword, newPassword))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Equal(t, msgPasswordNotLocal, errorOf(t, w))
	})
	t.Run("API key is refused with 403", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		keyRaw, _ := e.newKey(t, idUserU)
		w := e.do(http.MethodPut, passwordURL, pwBody(t, testPassword, newPassword), keyHeader(keyRaw))
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.Equal(t, middleware.MsgSessionRequired, errorOf(t, w))
		assert.True(t, e.passwordMatches(t, idUserU, testPassword))
	})
	t.Run("anonymous request is refused with 401", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.do(http.MethodPut, passwordURL, pwBody(t, testPassword, newPassword), nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.True(t, e.passwordMatches(t, idUserU, testPassword))
	})
	t.Run("handler without a user in the context is 401", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.do(http.MethodPut, "/open/me/password", pwBody(t, testPassword, newPassword), nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, msgAuthRequired, errorOf(t, w))
	})
	t.Run("disabled user is refused", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		// Disabled in the database without the admin API: no user block, so the
		// token still passes the REST middleware and the handler must refuse.
		require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", idUserU).Update("disabled", true).Error)
		w := e.changePassword(t, tokenWith(t, idUserU, "user-u", models.RoleUser), pwBody(t, testPassword, newPassword))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assert.Equal(t, msgAccountDisabled, errorOf(t, w))
		assert.True(t, e.passwordMatches(t, idUserU, testPassword))
		assert.Empty(t, e.auditRows(t))
	})
	t.Run("deleted user is refused", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		token := tokenWith(t, idUserU, "user-u", models.RoleUser)
		require.NoError(t, e.db.Where("id = ?", idUserU).Delete(&models.User{}).Error)
		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		assert.Equal(t, "User not found", errorOf(t, w))
		assert.Empty(t, e.auditRows(t))
	})
}

// With OIDC as the only interactive login only the service account may use a
// password (the rule of the password login).
func TestChangePasswordOIDCOnlyMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		serviceAccount bool
		localAuth      bool
		want           int
	}{
		{name: "regular user is refused", want: http.StatusForbidden},
		{name: "service account may change it", serviceAccount: true, want: http.StatusOK},
		{name: "regular user with local auth on", localAuth: true, want: http.StatusOK},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			e.auth.oidcCfg = &config.OIDCConfig{Enabled: true, LocalAuth: tt.localAuth}
			require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", idUserU).Update("service_account", tt.serviceAccount).Error)

			w := e.changePassword(t, tokenWith(t, idUserU, "user-u", models.RoleUser), pwBody(t, testPassword, newPassword))

			require.Equal(t, tt.want, w.Code, w.Body.String())
			assert.Equal(t, tt.want == http.StatusOK, e.passwordMatches(t, idUserU, newPassword))
			if tt.want == http.StatusForbidden {
				assert.Equal(t, msgLocalPasswordDisabled, errorOf(t, w))
			}
		})
	}
}

// Failed current-password checks count toward the login throttle of the
// account: five failures lock it for the password change and for the login.
func TestChangePasswordThrottle(t *testing.T) {
	t.Parallel()

	t.Run("five wrong passwords lock the change, also with the right one", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		token, _ := e.login(t, "user-u")
		for i := 0; i < loginFailureThreshold; i++ {
			w := e.changePassword(t, token, pwBody(t, "wrong-password-x", newPassword))
			require.Equal(t, http.StatusUnauthorized, w.Code, "attempt %d", i+1)
		}

		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))

		require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
		assert.Equal(t, msgTooManyLoginAttempts, errorOf(t, w))
		assert.NotEmpty(t, w.Header().Get("Retry-After"))
		assert.True(t, e.passwordMatches(t, idUserU, testPassword), "a locked account changes nothing")
		// The same lock refuses the password login.
		lw := e.do(http.MethodPost, "/api/v1/auth/login", loginBody(t, "user-u", testPassword),
			map[string]string{"X-Requested-With": "XMLHttpRequest"})
		assert.Equal(t, http.StatusUnauthorized, lw.Code, "the login is locked too")
	})

	t.Run("failed logins lock the change", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		token, _ := e.login(t, "user-u")
		for i := 0; i < loginFailureThreshold; i++ {
			lw := e.do(http.MethodPost, "/api/v1/auth/login", loginBody(t, "user-u", "wrong-password-x"),
				map[string]string{"X-Requested-With": "XMLHttpRequest"})
			require.Equal(t, http.StatusUnauthorized, lw.Code)
		}

		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))

		assert.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
		assert.True(t, e.passwordMatches(t, idUserU, testPassword))
	})

	t.Run("a success clears the failures", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		token, _ := e.login(t, "user-u")
		for i := 0; i < loginFailureThreshold-1; i++ {
			w := e.changePassword(t, token, pwBody(t, "wrong-password-x", "Another-Pass-9"))
			require.Equal(t, http.StatusUnauthorized, w.Code)
		}
		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		for i := 0; i < loginFailureThreshold-1; i++ {
			w := e.changePassword(t, token, pwBody(t, "wrong-password-x", "Another-Pass-9"))
			require.Equal(t, http.StatusUnauthorized, w.Code, "failure %d after the success", i+1)
		}
	})

	t.Run("validation errors do not use the budget", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		token, _ := e.login(t, "user-u")
		for i := 0; i < loginFailureThreshold+2; i++ {
			w := e.changePassword(t, token, pwBody(t, "wrong-password-x", "short"))
			require.Equal(t, http.StatusBadRequest, w.Code)
		}
		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("the budget is per account", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		token, _ := e.login(t, "user-u")
		other, _ := e.login(t, "user-v")
		for i := 0; i < loginFailureThreshold; i++ {
			e.changePassword(t, token, pwBody(t, "wrong-password-x", newPassword))
		}
		w := e.changePassword(t, other, pwBody(t, testPassword, newPassword))
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
}

// A failure on the server answers 500 "Internal server error" and nothing
// else; a failure before the write changes nothing.
func TestChangePasswordServerFailuresBeforeTheWrite(t *testing.T) {
	t.Parallel()

	t.Run("user lookup fails", func(t *testing.T) {
		t.Parallel()
		var failLookups atomic.Bool
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, findErr: func(context.Context, string) error {
				if failLookups.Load() {
					return errors.New("dial tcp 10.1.2.3:3306: refused")
				}
				return nil
			}}
		}})
		token, _ := e.login(t, "user-u")
		failLookups.Store(true)

		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))

		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "dial tcp")
		assert.True(t, e.passwordMatches(t, idUserU, testPassword))
		assert.Empty(t, e.auditRows(t))
	})

	t.Run("the password changed while the current one was checked", func(t *testing.T) {
		t.Parallel()
		var reads atomic.Int32
		var e *adminEnv
		e = newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, findErr: func(_ context.Context, id string) error {
				// The 2nd read of the user is the re-read before the write: an
				// admin reset commits between the two.
				if id == idUserU && reads.Add(1) == 2 {
					hash, err := bcrypt.GenerateFromPassword([]byte("Admin-Reset-Pass-1"), bcrypt.MinCost)
					require.NoError(t, err)
					require.NoError(t, e.users.UpdatePassword(context.Background(), idUserU, string(hash)))
				}
				return nil
			}}
		}})
		token, _ := e.login(t, "user-u")
		reads.Store(0)

		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))

		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assert.True(t, e.passwordMatches(t, idUserU, "Admin-Reset-Pass-1"), "the reset is not overwritten")
		assert.Empty(t, e.auditRows(t))
	})

	t.Run("password write fails", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, passErr: errors.New("Error 1213: deadlock")}
		}})
		token, _ := e.login(t, "user-u")
		otherToken, _ := e.login(t, "user-u")

		w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))

		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "deadlock")
		assert.True(t, e.passwordMatches(t, idUserU, testPassword))
		assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(otherToken)), "no session ends before the password is stored")
		assert.Empty(t, e.auditRows(t))
	})
}

// The change commits first and the revocation follows. A failed step answers
// 500, keeps the change, writes the audit entry with revocation_failed, and
// still runs the other steps.
func TestChangePasswordRevocationFailures(t *testing.T) {
	t.Parallel()
	errDial := errors.New("dial tcp 10.1.2.3:3306: refused")
	tests := []struct {
		name string
		opt  adminOpt
		// otherEnds is whether the other session must be unusable anyway (the
		// steps that did not fail still ran).
		otherEnds bool
	}{
		{
			name: "family revoke fails",
			opt: adminOpt{tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, revokeErr: errDial}
			}},
			otherEnds: true, // blocked in the session store, with the long block
		},
		{
			name: "session listing fails",
			opt: adminOpt{tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, listErr: errDial}
			}},
		},
		{
			name: "session block fails",
			opt: adminOpt{store: func(s sessionstore.SessionStore) sessionstore.SessionStore {
				return &faultyStore{SessionStore: s, blockErr: errDial}
			}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t, tt.opt)
			token, _ := e.login(t, "user-u")
			otherToken, _ := e.login(t, "user-u")

			w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))

			require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			assert.Equal(t, msgInternalServerError, errorOf(t, w))
			assert.NotContains(t, w.Body.String(), "dial tcp")
			assert.True(t, e.passwordMatches(t, idUserU, newPassword), "the change stands")
			if tt.otherEnds {
				assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(otherToken)))
			}
			assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(token)), "the current session is kept")

			rows := e.auditRows(t)
			require.Len(t, rows, 1, "the committed change is audited although the answer is 500")
			assert.Equal(t, middleware.AuditActionChangePassword, rows[0].Action)
			assert.Equal(t, idUserU, rows[0].EntityID)
			assert.Contains(t, rows[0].Details, `"revocation_failed":true`)
			assert.Contains(t, rows[0].Details, `"status":500`)
			assert.NotContains(t, rows[0].Details, newPassword)
		})
	}
}

// The revocation finishes with the request that stored the password; a
// repeated request with the new password as the current one ends the sessions
// that a failed first request left.
func TestChangePasswordAgainFinishesTheRevocation(t *testing.T) {
	t.Parallel()
	var failRevoke atomic.Bool
	e := newAdminEnv(t, adminOpt{store: func(s sessionstore.SessionStore) sessionstore.SessionStore {
		return &toggleBlockStore{SessionStore: s, fail: &failRevoke}
	}})
	token, _ := e.login(t, "user-u")
	otherToken, _ := e.login(t, "user-u")
	failRevoke.Store(true)
	w := e.changePassword(t, token, pwBody(t, testPassword, newPassword))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(otherToken)), "the block failed, the other session lives")

	failRevoke.Store(false)
	w = e.changePassword(t, token, pwBody(t, newPassword, "Yet-Another-Pass-2"))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(otherToken)))
	assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(token)))
}

// toggleBlockStore fails every Block* call while fail is set.
type toggleBlockStore struct {
	sessionstore.SessionStore
	fail *atomic.Bool
}

func (s *toggleBlockStore) BlockSessions(ctx context.Context, ids []string, until time.Time) error {
	if s.fail.Load() {
		return errors.New("dial tcp: store down")
	}
	return s.SessionStore.BlockSessions(ctx, ids, until)
}

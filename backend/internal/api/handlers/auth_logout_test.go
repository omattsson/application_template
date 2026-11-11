package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Logout ends the session of the cookie and of the bearer token: the family
// is revoked, the session and the jti are blocked, the sockets of the token
// close and the cookie is deleted with the attributes it was set with.
func TestLogoutEndsTheSession(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	access, raw := s.login(t, "alice")
	otherAccess, otherRaw := s.login(t, "alice") // a second device
	claims := claimsOf(t, access)

	w := s.post(t, "/api/v1/auth/logout", "", withCookie(raw), withBearer(access))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, messageSchema, w.Body.Bytes()))
	assertCookieCleared(t, w)
	blocked, err := s.store.IsTokenBlocked(context.Background(), claims.ID)
	require.NoError(t, err)
	assert.True(t, blocked, "the jti is blocked")
	assert.True(t, s.sessionBlocked(t, claims.SessionID))
	assert.Zero(t, s.familyActive(t, claims.SessionID))
	assert.Equal(t, http.StatusUnauthorized, s.me(t, access))
	rw, _, _ := s.refresh(t, raw)
	assert.Equal(t, http.StatusUnauthorized, rw.Code)
	_, tokens := s.revoker.snapshot()
	assert.Equal(t, []string{claims.ID}, tokens)

	// The other device keeps its session.
	assert.Equal(t, http.StatusOK, s.me(t, otherAccess))
	rw, _, _ = s.refresh(t, otherRaw)
	assert.Equal(t, http.StatusOK, rw.Code)
}

// Logout with only one of the credentials ends that session; an expired
// bearer token is ignored; a request with neither ends nothing. Every case is
// 200, also when repeated (already logged out).
func TestLogoutVariants(t *testing.T) {
	t.Parallel()
	t.Run("cookie only", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		access, raw := s.login(t, "alice")
		w := s.post(t, "/api/v1/auth/logout", "", withCookie(raw))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, http.StatusUnauthorized, s.me(t, access), "the session block ends the access token")
	})
	t.Run("bearer only", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		access, raw := s.login(t, "alice")
		w := s.post(t, "/api/v1/auth/logout", "", withBearer(access))
		require.Equal(t, http.StatusOK, w.Code)
		assertCookieCleared(t, w)
		rw, _, _ := s.refresh(t, raw)
		assert.Equal(t, http.StatusUnauthorized, rw.Code, "the session of the sid is revoked")
	})
	t.Run("expired bearer is ignored", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		_, raw := s.login(t, "alice")
		expired, err := s.h.signToken(sessionTokenOptsForTest("u1", s.row(t, raw).FamilyID, -time.Minute))
		require.NoError(t, err)
		w := s.post(t, "/api/v1/auth/logout", "", withBearer(expired))
		require.Equal(t, http.StatusOK, w.Code)
		assert.False(t, s.row(t, raw).Revoked, "an expired token proves nothing")
	})
	t.Run("nothing to end", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		w := s.post(t, "/api/v1/auth/logout", "")
		require.Equal(t, http.StatusOK, w.Code)
		assertCookieCleared(t, w)
	})
	t.Run("repeated logout", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		access, raw := s.login(t, "alice")
		for i := 0; i < 3; i++ {
			w := s.post(t, "/api/v1/auth/logout", "", withCookie(raw), withBearer(access))
			assert.Equal(t, http.StatusOK, w.Code, "attempt %d", i+1)
		}
	})
	t.Run("unknown or malformed cookie", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		for _, raw := range []string{"garbage", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
			w := s.post(t, "/api/v1/auth/logout", "", withCookie(raw))
			assert.Equal(t, http.StatusOK, w.Code)
		}
	})
	t.Run("a cookie and a bearer of different users end both sessions", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		bobAccess, _ := s.login(t, "bob")
		aliceAccess, aliceRaw := s.login(t, "alice")
		w := s.post(t, "/api/v1/auth/logout", "", withCookie(aliceRaw), withBearer(bobAccess))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, http.StatusUnauthorized, s.me(t, aliceAccess), "the cookie's session ends")
		assert.Equal(t, http.StatusUnauthorized, s.me(t, bobAccess), "the bearer's session ends")
	})
}

// sessionTokenOptsForTest are the claims of a session access token of userID.
func sessionTokenOptsForTest(userID, sid string, ttl time.Duration) middleware.GenerateTokenOptions {
	return middleware.GenerateTokenOptions{UserID: userID, Username: "alice", Role: models.RoleUser, Secret: testJWTSecret, Expiration: ttl, SessionID: sid}
}

// A failure on the server answers 500, but the cookie is still cleared and
// the other steps still run.
func TestLogoutServerFailures(t *testing.T) {
	t.Parallel()
	t.Run("token lookup fails", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		access, raw := s.login(t, "alice")
		s.h.SetRefreshTokenRepo(&faultyTokens{RefreshTokenRepository: s.tokens, findErr: errors.New("dial tcp: secret detail")})
		w := s.post(t, "/api/v1/auth/logout", "", withCookie(raw), withBearer(access))
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
		assertCookieCleared(t, w)
		assert.Equal(t, http.StatusUnauthorized, s.me(t, access), "the bearer's session and jti are still blocked")
	})
	t.Run("revoke fails: long session block", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		access, raw := s.login(t, "alice")
		s.h.SetRefreshTokenRepo(&faultyTokens{RefreshTokenRepository: s.tokens, revokeErr: errors.New("dial tcp")})
		w := s.post(t, "/api/v1/auth/logout", "", withCookie(raw), withBearer(access))
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assertCookieCleared(t, w)
		s.h.SetRefreshTokenRepo(s.tokens)
		rw, _, _ := s.refresh(t, raw)
		assert.Equal(t, http.StatusUnauthorized, rw.Code, "the session block refuses the still active refresh token")
	})
	t.Run("session store fails", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil, withStore(func(st sessionstore.SessionStore) sessionstore.SessionStore {
			return &faultyStore{SessionStore: st, blockErr: errors.New("dial tcp")}
		}))
		access, raw := s.login(t, "alice")
		w := s.post(t, "/api/v1/auth/logout", "", withCookie(raw), withBearer(access))
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assertCookieCleared(t, w)
		assert.True(t, s.row(t, raw).Revoked, "the family is still revoked")
	})
}

// Logout-all ends every session of the user: all families are revoked, all
// access tokens are rejected, the sockets of the user close; other users
// keep theirs. It is idempotent.
func TestLogoutAllEndsEverySession(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	a1, r1 := s.login(t, "alice")
	a2, r2 := s.login(t, "alice")
	bob, bobRaw := s.login(t, "bob")

	w := s.post(t, "/api/v1/auth/logout-all", "", withBearer(a1), withCookie(r1))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, validateJSONSchema(t, messageSchema, w.Body.Bytes()))
	assertCookieCleared(t, w)
	for _, tok := range []string{a1, a2} {
		assert.Equal(t, http.StatusUnauthorized, s.me(t, tok))
		assert.True(t, s.sessionBlocked(t, claimsOf(t, tok).SessionID))
	}
	for _, raw := range []string{r1, r2} {
		rw, _, _ := s.refresh(t, raw)
		assert.Equal(t, http.StatusUnauthorized, rw.Code)
	}
	blocked, err := s.store.IsTokenBlocked(context.Background(), claimsOf(t, a1).ID)
	require.NoError(t, err)
	assert.True(t, blocked)
	users, _ := s.revoker.snapshot()
	assert.Equal(t, []string{"u1"}, users)

	assert.Equal(t, http.StatusOK, s.me(t, bob), "another user is not affected")
	rw, _, _ := s.refresh(t, bobRaw)
	assert.Equal(t, http.StatusOK, rw.Code)

	// Idempotent: a new session can log out everywhere again.
	a3, _ := s.login(t, "alice")
	w = s.post(t, "/api/v1/auth/logout-all", "", withBearer(a3))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, http.StatusUnauthorized, s.me(t, a3))
}

// R18: the user block is inclusive to the millisecond, so logout-all returns
// only after its block millisecond has passed. A login right after it gets a
// token that works, while a token issued before it stays rejected.
func TestLoginRightAfterLogoutAllWorks(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	before, _ := s.login(t, "alice")
	for i := 0; i < 25; i++ {
		access, _ := s.login(t, "alice")
		w := s.post(t, "/api/v1/auth/logout-all", "", withBearer(access))
		require.Equal(t, http.StatusOK, w.Code)

		after, raw := s.login(t, "alice")
		require.Equal(t, http.StatusOK, s.me(t, after), "attempt %d: the login after logout-all works", i+1)
		rw, refreshed, _ := s.refresh(t, raw)
		require.Equal(t, http.StatusOK, rw.Code)
		require.Equal(t, http.StatusOK, s.me(t, refreshed))
		require.Equal(t, http.StatusUnauthorized, s.me(t, access), "the token used for logout-all is rejected")
	}
	assert.Equal(t, http.StatusUnauthorized, s.me(t, before), "a token from before logout-all is rejected")
}

// waitPastMillisecond returns only when the clock is in a later millisecond.
func TestWaitPastMillisecond(t *testing.T) {
	t.Parallel()
	for i := 0; i < 50; i++ {
		at := time.Now()
		waitPastMillisecond(at)
		assert.Greater(t, time.Now().UnixMilli(), at.UnixMilli())
	}
	waitPastMillisecond(time.Now().Add(-time.Hour)) // a past time returns at once
}

func TestLogoutAllFailures(t *testing.T) {
	t.Parallel()
	t.Run("no user in the context", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		s.router.POST("/open-logout-all", s.h.LogoutAll)
		w := s.post(t, "/open-logout-all", "")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
	tests := []struct {
		tokens func(models.RefreshTokenRepository) models.RefreshTokenRepository
		store  func(sessionstore.SessionStore) sessionstore.SessionStore
		name   string
	}{
		{name: "revoke fails", tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
			return &faultyTokens{RefreshTokenRepository: r, revokeErr: errors.New("dial tcp")}
		}},
		{name: "listing fails", tokens: func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
			return &faultyTokens{RefreshTokenRepository: r, listErr: errors.New("dial tcp")}
		}},
		{name: "session store fails", store: func(st sessionstore.SessionStore) sessionstore.SessionStore {
			return &faultyStore{SessionStore: st, blockErr: errors.New("dial tcp")}
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var opts []harnessOpt
			if tt.store != nil {
				opts = append(opts, withStore(tt.store))
			}
			s := newSessionHarness(t, nil, opts...)
			access, _ := s.login(t, "alice")
			if tt.tokens != nil {
				s.h.SetRefreshTokenRepo(tt.tokens(s.tokens))
			}
			w := s.post(t, "/api/v1/auth/logout-all", "", withBearer(access))
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
			assertCookieCleared(t, w)
			users, _ := s.revoker.snapshot()
			assert.Equal(t, []string{"u1"}, users, "the sockets close anyway")
		})
	}
}

// Without a session store and a WebSocket hub, logout and logout-all still
// work (nil-safe).
func TestLogoutWithoutStoreOrHub(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	access, raw := s.login(t, "alice")
	s.h.SetSessionStore(nil)
	s.h.SetWebSocketRevoker(nil)
	assert.Equal(t, http.StatusOK, s.post(t, "/api/v1/auth/logout", "", withCookie(raw), withBearer(access)).Code)
	assert.True(t, s.row(t, raw).Revoked)
	assert.Equal(t, http.StatusOK, s.post(t, "/api/v1/auth/logout-all", "", withBearer(access)).Code)
}

// CleanupExpiredTokens removes only the rows that expired before the
// retention window; active, consumed-but-unexpired and recently expired rows
// stay. A context error is returned so errors.Is finds it.
func TestCleanupExpiredTokens(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, func(c *config.AuthConfig) { c.RefreshReuseGrace = 30 * time.Second })
	now := time.Now().UTC()
	window := s.h.sessionListWindow()
	seed := func(id string, expires time.Time, revoked bool) {
		require.NoError(t, s.tokens.Create(context.Background(), &models.RefreshToken{
			ID: id, UserID: "u1", TokenHash: sha256Hex(id), ExpiresAt: expires, LastActivity: now, Revoked: revoked,
		}))
	}
	seed("active", now.Add(time.Hour), false)
	seed("consumed", now.Add(time.Hour), true)
	seed("recently-expired", now.Add(-window+time.Minute), false)
	seed("long-expired", now.Add(-window-time.Minute), false)
	seed("long-expired-consumed", now.Add(-window-time.Hour), true)

	require.NoError(t, s.h.CleanupExpiredTokens(context.Background()))

	var ids []string
	require.NoError(t, s.tokens.DB.Model(&models.RefreshToken{}).Order("id").Pluck("id", &ids).Error)
	assert.Equal(t, []string{"active", "consumed", "recently-expired"}, ids)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.h.CleanupExpiredTokens(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	h, _, _ := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
	assert.NoError(t, h.CleanupExpiredTokens(context.Background()), "no refresh repository: nothing to do")
}

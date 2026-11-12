package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fillBcryptSem takes every slot of bcryptSem until the test ends. The tests
// that call it are not parallel: the parallel tests of the package only start
// after every serial test finished, so none of them sees the full semaphore.
func fillBcryptSem(t *testing.T) {
	t.Helper()
	n := cap(bcryptSem)
	for i := 0; i < n; i++ {
		bcryptSem <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < n; i++ {
			<-bcryptSem
		}
	})
}

// cancelWhenWaiting returns a context that is cancelled at the moment a
// request starts to wait for a bcrypt slot (the hook of acquireBcrypt): a
// client that leaves while it is queued, without any timing. The test fails
// when no request ever waited, so it cannot pass by a cancel that came too
// early. One use per test (the hook is process-wide; these tests are serial).
func cancelWhenWaiting(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var waited atomic.Int32
	hook := func() {
		waited.Add(1)
		cancel()
	}
	bcryptWaitHook.Store(&hook)
	t.Cleanup(func() {
		bcryptWaitHook.Store(nil)
		cancel()
		assert.Positive(t, waited.Load(), "the request waited for a bcrypt slot before the cancel")
	})
	return ctx
}

// With every bcrypt slot taken, the wait for a slot ends with the context of
// the request: no goroutine queues for CPU that nobody will read.
func TestBcryptWaitEndsWithTheContext(t *testing.T) {
	t.Run("compare, cancelled while it waits", func(t *testing.T) {
		fillBcryptSem(t)
		h, _, rc := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
		ctx := cancelWhenWaiting(t)

		start := time.Now()
		err := h.comparePassword(ctx, []byte(testPasswordHash()), "x")

		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 5*time.Second)
		assert.Empty(t, rc.calls(), "no compare ran")
	})

	t.Run("hash, cancelled while it waits", func(t *testing.T) {
		fillBcryptSem(t)
		ctx := cancelWhenWaiting(t)

		hash, err := hashPassword(ctx, "long-enough-1")

		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, hash)
	})

	t.Run("hash, deadline while it waits", func(t *testing.T) {
		fillBcryptSem(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		_, err := hashPassword(ctx, "long-enough-1")

		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("a context that ended before the call never runs bcrypt, also with a free slot", func(t *testing.T) {
		h, _, rc := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		assert.ErrorIs(t, h.comparePassword(ctx, []byte(testPasswordHash()), "x"), context.Canceled)
		_, err := hashPassword(ctx, "long-enough-1")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, rc.calls())
		assert.Zero(t, len(bcryptSem), "no slot is held")
	})

	t.Run("a live context gets its slot when one is free", func(t *testing.T) {
		h, _, _ := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
		require.NoError(t, h.comparePassword(context.Background(), []byte(testPasswordHash()), testPassword))
		hash, err := hashPassword(context.Background(), "long-enough-1")
		require.NoError(t, err)
		assert.NotEmpty(t, hash)
		assert.Zero(t, len(bcryptSem), "the slots are released")
	})
}

// A request that ends while it waits for a bcrypt slot: 499 for a cancel, 500
// for a deadline; never a credential failure, and the handler does not hang.
func TestBcryptWaitInTheHandlers(t *testing.T) {
	post := func(r http.Handler, ctx context.Context, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("login with a real user: 499", func(t *testing.T) {
		fillBcryptSem(t)
		repo := newMockUserRepo(localUser("u1", "alice"))
		h, logs, _ := newTestAuthHandler(repo, testAuthConfig(), nil)
		ctx := cancelWhenWaiting(t)

		w := post(newAuthRouter(h), ctx, "/api/v1/auth/login", loginBody(t, "alice", testPassword))

		assert.Equal(t, statusClientClosedRequest, w.Code, w.Body.String())
		assert.NotContains(t, logs.String(), "password_mismatch")
	})

	t.Run("login with a real user: a deadline is 500 without detail", func(t *testing.T) {
		fillBcryptSem(t)
		repo := newMockUserRepo(localUser("u1", "alice"))
		h, logs, _ := newTestAuthHandler(repo, testAuthConfig(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		w := post(newAuthRouter(h), ctx, "/api/v1/auth/login", loginBody(t, "alice", testPassword))

		assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
		assert.Contains(t, logs.String(), loginReasonCompareError)
	})

	t.Run("login with an unknown user: the same 499 as a real user (no enumeration)", func(t *testing.T) {
		fillBcryptSem(t)
		h, _, rc := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
		ctx := cancelWhenWaiting(t)

		w := post(newAuthRouter(h), ctx, "/api/v1/auth/login", loginBody(t, "nobody", "whatever-pass"))

		assert.Equal(t, statusClientClosedRequest, w.Code, w.Body.String())
		assert.Empty(t, rc.calls())
	})

	t.Run("register: 499 and no user", func(t *testing.T) {
		fillBcryptSem(t)
		repo := newMockUserRepo()
		h, _, _ := newTestAuthHandler(repo, selfRegistrationConfig(), nil)
		ctx := cancelWhenWaiting(t)

		w := post(newAuthRouter(h), ctx, "/api/v1/auth/register", `{"username":"newuser","password":"long-enough-1"}`)

		assert.Equal(t, statusClientClosedRequest, w.Code, w.Body.String())
		assert.Equal(t, 0, repo.count())
	})

	t.Run("admin creates a user: 499 and no user", func(t *testing.T) {
		e := newAdminEnv(t)
		bearer := e.bearer(t, idAdminA)["Authorization"]
		before, err := e.users.Count(context.Background())
		require.NoError(t, err)
		fillBcryptSem(t)
		ctx := cancelWhenWaiting(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"username":"newbie","password":"Correct-Horse-9","role":"user"}`)).WithContext(ctx)
		req.Header.Set("Authorization", bearer)
		w := httptest.NewRecorder()

		e.router.ServeHTTP(w, req)

		assert.Equal(t, statusClientClosedRequest, w.Code, w.Body.String())
		after, err := e.users.Count(context.Background())
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("admin resets a password: 499 and the hash is kept", func(t *testing.T) {
		e := newAdminEnv(t)
		bearer := e.bearer(t, idAdminA)["Authorization"]
		before := e.user(t, idUserV).PasswordHash
		fillBcryptSem(t)
		ctx := cancelWhenWaiting(t)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+idUserV, strings.NewReader(`{"password":"Brand-New-Pass-1"}`)).WithContext(ctx)
		req.Header.Set("Authorization", bearer)
		w := httptest.NewRecorder()

		e.router.ServeHTTP(w, req)

		assert.Equal(t, statusClientClosedRequest, w.Code, w.Body.String())
		assert.Equal(t, before, e.user(t, idUserV).PasswordHash)
	})

	t.Run("EnsureAdminUser: a cancelled startup context is an error, no user", func(t *testing.T) {
		fillBcryptSem(t)
		repo := newMockUserRepo()
		cfg := testAuthConfig()
		cfg.AdminPassword = "Admin-Pass-123"
		h, _, _ := newTestAuthHandler(repo, cfg, nil)
		ctx := cancelWhenWaiting(t)

		err := h.EnsureAdminUser(ctx)

		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), cfg.AdminPassword)
		assert.Equal(t, 0, repo.count())
	})
}

// Every login branch that waits for a bcrypt slot (the real compare and the
// dummy compare of each anti-enumeration branch) treats a wait that ended with
// the request the same way: the same status and body, no Retry-After, the same
// log reason, and a neutral throttle (the name key counts nothing). So an
// attacker who fills the slots and cancels probes cannot tell a real account
// from an unknown, locked, SSO-only or unusable one, neither by the answer nor
// by a later 429.
func TestLoginBcryptWaitEndsIdenticallyOnEveryBranch(t *testing.T) {
	extID := "sub-1"
	type branch struct {
		name     string
		users    []models.User
		username string
		password string
		// prepare changes the throttle before the probes (a locked account).
		prepare func(h *AuthHandler)
	}
	tooLong := strings.Repeat("a", models.MaxUsernameLen+1)
	branches := []branch{
		{name: "real account", users: []models.User{localUser("u1", "alice")}, username: "alice", password: testPassword},
		{name: "unknown name", username: "nobody", password: testPassword},
		{name: "username longer than the column", username: tooLong, password: testPassword},
		{name: "password over 72 bytes", users: []models.User{localUser("u1", "alice")}, username: "alice", password: strings.Repeat("p", maxPasswordBytes+1)},
		{name: "SSO-only account", users: []models.User{{ID: "u1", Username: "alice", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC, ExternalID: &extID}},
			username: "alice", password: testPassword},
		{name: "account without a password", users: []models.User{{ID: "u1", Username: "alice", Role: models.RoleUser, AuthProvider: models.AuthProviderLocal}},
			username: "alice", password: testPassword},
		{name: "account with a malformed hash", users: []models.User{{ID: "u1", Username: "alice", Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "not-a-bcrypt-hash"}},
			username: "alice", password: testPassword},
		{name: "account key locked or busy", users: []models.User{localUser("u1", "alice")}, username: "alice", password: testPassword,
			prepare: func(h *AuthHandler) {
				// Hold every slot of the account key, as attempts in flight.
				for i := 0; i < loginFailureThreshold; i++ {
					if _, ok := h.throttle.reserve(accountThrottleKey("u1")); !ok {
						panic("could not reserve the account key")
					}
				}
			}},
	}
	modes := []struct {
		name     string
		wantCode int
		wantBody string
		// ctx returns the context of one probe.
		ctx func(t *testing.T) context.Context
	}{
		{name: "client leaves while queued", wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`,
			ctx: func(t *testing.T) context.Context { return cancelWhenWaiting(t) }},
		{name: "deadline while queued", wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`,
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			}},
	}

	for _, m := range modes {
		m := m
		t.Run(m.name, func(t *testing.T) {
			for _, b := range branches {
				b := b
				t.Run(b.name, func(t *testing.T) {
					h, logs, rc := newTestAuthHandler(newMockUserRepo(b.users...), testAuthConfig(), nil)
					if b.prepare != nil {
						b.prepare(h)
					}
					router := newAuthRouter(h)
					baseline := h.throttle.size()
					probe := func(ctx context.Context, password string) *httptest.ResponseRecorder {
						req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody(t, b.username, password))).WithContext(ctx)
						req.Header.Set("Content-Type", "application/json")
						w := httptest.NewRecorder()
						router.ServeHTTP(w, req)
						return w
					}

					// More cancelled waits than the threshold: if any of them
					// counted as a failure the name key would be locked.
					fillBcryptSem(t)
					for i := 0; i < loginFailureThreshold+2; i++ {
						// Each probe has its own wait-hook context; the hook of
						// the previous probe is replaced.
						ctx := m.ctx(t)
						w := probe(ctx, b.password)
						require.Equal(t, m.wantCode, w.Code, "probe %d: %s", i+1, w.Body.String())
						assert.JSONEq(t, m.wantBody, w.Body.String(), "probe %d", i+1)
						assert.Empty(t, w.Header().Get("Retry-After"), "probe %d", i+1)
					}
					assert.Empty(t, rc.calls(), "no compare ran")
					assert.Equal(t, baseline, h.throttle.size(), "the throttle holds nothing new: the outcome is neutral")
					_, locked := h.throttle.check(nameThrottleKey(b.username))
					assert.False(t, locked, "the name key is not locked")
					assert.NotContains(t, logs.String(), `"reason":"`+loginReasonPasswordMismatch)

					// Free the slots: a normal probe of the same name is a plain
					// 401 (never the 429 of a locked name), whatever the branch.
					for i := 0; i < cap(bcryptSem); i++ {
						<-bcryptSem
					}
					defer func() {
						for i := 0; i < cap(bcryptSem); i++ {
							bcryptSem <- struct{}{}
						}
					}()
					bcryptWaitHook.Store(nil)
					w := probe(context.Background(), "a-wrong-password-1")
					assert.NotEqual(t, http.StatusTooManyRequests, w.Code, w.Body.String())
					assert.Equal(t, http.StatusUnauthorized, w.Code, "a normal probe with a wrong password is the generic credential failure")
					assert.JSONEq(t, invalidCredentialsBody, w.Body.String())
				})
			}
		})
	}
}

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// cancelSoon ends the returned context after d, as a client that leaves.
func cancelSoon(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(d, cancel)
	return ctx, cancel
}

// With every bcrypt slot taken, the wait for a slot ends with the context of
// the request: no goroutine queues for CPU that nobody will read.
func TestBcryptWaitEndsWithTheContext(t *testing.T) {
	t.Run("compare, cancelled while it waits", func(t *testing.T) {
		fillBcryptSem(t)
		h, _, rc := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()

		start := time.Now()
		err := h.comparePassword(ctx, []byte(testPasswordHash()), "x")

		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 5*time.Second)
		assert.Empty(t, rc.calls(), "no compare ran")
	})

	t.Run("hash, cancelled while it waits", func(t *testing.T) {
		fillBcryptSem(t)
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()

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
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()

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

	t.Run("login with an unknown user: the dummy compare is skipped and the answer is the generic 401", func(t *testing.T) {
		fillBcryptSem(t)
		h, _, rc := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()

		w := post(newAuthRouter(h), ctx, "/api/v1/auth/login", loginBody(t, "nobody", "whatever-pass"))

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.JSONEq(t, invalidCredentialsBody, w.Body.String())
		assert.Empty(t, rc.calls())
	})

	t.Run("register: 499 and no user", func(t *testing.T) {
		fillBcryptSem(t)
		repo := newMockUserRepo()
		h, _, _ := newTestAuthHandler(repo, selfRegistrationConfig(), nil)
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()

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
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()
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
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()
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
		ctx, cancel := cancelSoon(20 * time.Millisecond)
		defer cancel()

		err := h.EnsureAdminUser(ctx)

		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), cfg.AdminPassword)
		assert.Equal(t, 0, repo.count())
	})
}

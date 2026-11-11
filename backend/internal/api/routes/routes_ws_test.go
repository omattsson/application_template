package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/api/handlers"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wsURL serves router and returns the ws:// URL of /ws.
func wsURL(t *testing.T, router http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
}

// dialWS opens a socket. It returns the connection (nil on a refused
// upgrade) and the HTTP status of the handshake.
func dialWS(t *testing.T, target string, header http.Header) (*gorilla.Conn, int) {
	t.Helper()
	conn, resp, err := gorilla.DefaultDialer.Dial(target, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		require.ErrorIs(t, err, gorilla.ErrBadHandshake, "the server answered the upgrade")
		require.NotNil(t, resp)
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, resp.StatusCode
}

// withToken adds ?token= to the URL.
func withToken(target, tok string) string {
	return target + "?token=" + url.QueryEscape(tok)
}

// waitClosed reads until the server closes the socket and returns the close
// code. It fails when nothing closes it within 5 seconds.
func waitClosed(t *testing.T, conn *gorilla.Conn) int {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *gorilla.CloseError
		require.True(t, errors.As(err, &ce), "the server closed the socket with a close frame, got %v", err)
		return ce.Code
	}
}

// subscribe sends a subscribe frame.
func subscribe(t *testing.T, conn *gorilla.Conn, topic string) {
	t.Helper()
	msg, err := json.Marshal(map[string]any{"type": "subscribe", "payload": map[string]string{"topic": topic}})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(gorilla.TextMessage, msg))
}

// TestWSAuthOffUnchanged: with auth off /ws needs no credential and any topic
// can be subscribed (existing behaviour).
func TestWSAuthOffUnchanged(t *testing.T) {
	t.Parallel()
	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	router := gin.New()
	res, err := Setup(router, Deps{
		Config: testConfig(100, 10, nil), Repository: handlers.NewMockRepository(),
		HealthChecker: health.New(), Hub: hub,
	})
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)
	assert.Nil(t, res.AuthHandler, "no auth handler with auth off")
	assert.Nil(t, res.AuditRecorder, "no audit recorder with auth off")
	target := wsURL(t, router)

	conn, status := dialWS(t, target, http.Header{"Authorization": {"Bearer garbage"}})
	require.Equal(t, http.StatusSwitchingProtocols, status)
	subscribe(t, conn, "admin:anything")
	require.Eventually(t, func() bool { return hub.TopicSubscriberCount("admin:anything") == 1 },
		2*time.Second, 5*time.Millisecond, "every topic can be subscribed with auth off")
}

// TestWSAuthOnUpgrade: the upgrade needs a valid credential.
func TestWSAuthOnUpgrade(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	user := env.createUser(t, "alice", models.RoleUser)
	tok := env.token(t, user)
	key, _ := env.apiKey(t, user)
	target := wsURL(t, env.router)

	tests := []struct {
		name   string
		target string
		header http.Header
		want   int
	}{
		{name: "no credential", target: target, want: http.StatusUnauthorized},
		{name: "garbage query token", target: withToken(target, "garbage"), want: http.StatusUnauthorized},
		{name: "garbage bearer", target: target, header: http.Header{"Authorization": {"Bearer garbage"}}, want: http.StatusUnauthorized},
		{name: "garbage api key", target: target, header: http.Header{"X-Api-Key": {"garbage"}}, want: http.StatusUnauthorized},
		{name: "bearer and api key", target: target, header: http.Header{"Authorization": {"Bearer " + tok}, "X-Api-Key": {key}}, want: http.StatusBadRequest},
		{name: "valid query token", target: withToken(target, tok), want: http.StatusSwitchingProtocols},
		{name: "valid bearer", target: target, header: http.Header{"Authorization": {"Bearer " + tok}}, want: http.StatusSwitchingProtocols},
		{name: "valid api key", target: target, header: http.Header{"X-Api-Key": {key}}, want: http.StatusSwitchingProtocols},
	}

	for _, tt := range tests {
		_, status := dialWS(t, tt.target, tt.header)
		assert.Equal(t, tt.want, status, tt.name)
	}
}

// TestWSLogoutClosesSocketAndRefusesUpgrade is Review Focus 2 (logout): the
// open socket of the logged-out token closes and a new upgrade with that
// token is refused.
func TestWSLogoutClosesSocketAndRefusesUpgrade(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	env.createUser(t, "alice", models.RoleUser)
	tok, _ := env.login(t, "alice")
	target := wsURL(t, env.router)

	conn, status := dialWS(t, withToken(target, tok), nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)

	h := bearer(tok)
	h["X-Requested-With"] = "x"
	w := env.do(t, http.MethodPost, "/api/v1/auth/logout", "", h)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, gorilla.ClosePolicyViolation, waitClosed(t, conn))
	_, status = dialWS(t, withToken(target, tok), nil)
	assert.Equal(t, http.StatusUnauthorized, status)
}

// TestWSDisabledUserSocketClosed is Review Focus 2 (disable): an admin
// disables the user; the user's open socket closes and a new upgrade with
// the old token is refused.
func TestWSDisabledUserSocketClosed(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	user := env.createUser(t, "alice", models.RoleUser)
	admin := env.createUser(t, "root", models.RoleAdmin)
	tok := env.token(t, user)
	key, _ := env.apiKey(t, user)
	target := wsURL(t, env.router)

	jwtConn, status := dialWS(t, withToken(target, tok), nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	keyConn, status := dialWS(t, target, http.Header{"X-Api-Key": {key}})
	require.Equal(t, http.StatusSwitchingProtocols, status)

	w := env.do(t, http.MethodPut, "/api/v1/users/"+user.ID+"/disabled", `{"disabled":true}`, bearer(env.token(t, admin)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, gorilla.ClosePolicyViolation, waitClosed(t, jwtConn))
	assert.Equal(t, gorilla.ClosePolicyViolation, waitClosed(t, keyConn))
	_, status = dialWS(t, withToken(target, tok), nil)
	assert.Equal(t, http.StatusUnauthorized, status)
	_, status = dialWS(t, target, http.Header{"X-Api-Key": {key}})
	assert.Equal(t, http.StatusUnauthorized, status)
}

// TestWSRevalidationClosesSocketRevokedElsewhere: a revocation that this
// replica did not handle (a block written by another replica) closes the
// socket at the next revalidation run.
func TestWSRevalidationClosesSocketRevokedElsewhere(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil, func(_ *authEnv, d *AuthDeps) { d.RevalidateInterval = 50 * time.Millisecond })
	user := env.createUser(t, "alice", models.RoleUser)
	tok := env.token(t, user)
	target := wsURL(t, env.router)

	conn, status := dialWS(t, withToken(target, tok), nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	require.Eventually(t, func() bool { return env.hub.ClientCount() == 1 }, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, env.store.BlockUser(context.Background(), user.ID, time.Now().Add(time.Hour)))
	assert.Equal(t, gorilla.ClosePolicyViolation, waitClosed(t, conn))
}

// TestWSFailsClosedOnStoreError is R20 for the upgrade and the revalidation:
// a store that cannot be read refuses the upgrade (500) and closes the open
// sockets at the next run.
func TestWSFailsClosedOnStoreError(t *testing.T) {
	t.Parallel()
	store := newFailingStore(t)
	env := newAuthEnv(t, nil, func(_ *authEnv, d *AuthDeps) {
		d.SessionStore = store
		d.RevalidateInterval = 50 * time.Millisecond
	})
	user := env.createUser(t, "alice", models.RoleUser)
	tok := env.token(t, user)
	target := wsURL(t, env.router)

	conn, status := dialWS(t, withToken(target, tok), nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)

	store.setFail(true)
	_, status = dialWS(t, withToken(target, tok), nil)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, gorilla.ClosePolicyViolation, waitClosed(t, conn), "the revalidation fails closed")
}

// TestWSTopicSubscriptionsAreAuthorised: a client can subscribe only to the
// topics its identity allows.
func TestWSTopicSubscriptionsAreAuthorised(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	user := env.createUser(t, "alice", models.RoleUser)
	other := env.createUser(t, "bob", models.RoleUser)
	admin := env.createUser(t, "root", models.RoleAdmin)
	target := wsURL(t, env.router)

	userConn, _ := dialWS(t, withToken(target, env.token(t, user)), nil)
	require.NotNil(t, userConn)
	adminConn, _ := dialWS(t, withToken(target, env.token(t, admin)), nil)
	require.NotNil(t, adminConn)

	// Forbidden first, then allowed: the read pump handles the frames in
	// order, so once the allowed ones are in, the forbidden ones were seen.
	for _, topic := range []string{"admin:audit", "user:" + other.ID, "nonsense", "item:7", "user:" + user.ID} {
		subscribe(t, userConn, topic)
	}
	subscribe(t, adminConn, "admin:audit")

	count := func(topic string) int { return env.hub.TopicSubscriberCount(topic) }
	require.Eventually(t, func() bool {
		return count("item:7") == 1 && count("user:"+user.ID) == 1 && count("admin:audit") == 1
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, count("admin:audit"), "only the admin is subscribed to the admin topic")
	assert.Zero(t, count("user:"+other.ID), "a user cannot watch another user's topic")
	assert.Zero(t, count("nonsense"), "an unknown topic is refused")

	// The user gets the item message, not the admin message.
	env.hub.BroadcastToTopic("admin:audit", []byte(`"admin-only"`))
	env.hub.BroadcastToTopic("item:7", []byte(`"item"`))
	require.NoError(t, userConn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, msg, err := userConn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, `"item"`, string(msg))
}

// TestWSAPIKeyDeleteClosesSocket: deleting a key closes the sockets opened
// with it (the API key handler has the hub as its revoker).
func TestWSAPIKeyDeleteClosesSocket(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	user := env.createUser(t, "alice", models.RoleUser)
	key, keyID := env.apiKey(t, user)
	target := wsURL(t, env.router)

	conn, status := dialWS(t, target, http.Header{"X-Api-Key": {key}})
	require.Equal(t, http.StatusSwitchingProtocols, status)

	w := env.do(t, http.MethodDelete, "/api/v1/api-keys/"+keyID, "", bearer(env.token(t, user)))
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Equal(t, gorilla.ClosePolicyViolation, waitClosed(t, conn))
}

// TestWSUpgradeChecksTheAccount: a user disabled or deleted directly in the
// database (no block in the session store) cannot open a socket: the upgrade
// reads the account (RequireActiveUser).
func TestWSUpgradeChecksTheAccount(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, nil)
	user := env.createUser(t, "alice", models.RoleUser)
	tok := env.token(t, user)
	target := wsURL(t, env.router)

	require.NoError(t, env.db.Model(&models.User{}).Where("id = ?", user.ID).Update("disabled", true).Error)
	_, status := dialWS(t, withToken(target, tok), nil)
	assert.Equal(t, http.StatusUnauthorized, status)

	require.NoError(t, env.db.Delete(&models.User{}, "id = ?", user.ID).Error)
	_, status = dialWS(t, withToken(target, tok), nil)
	assert.Equal(t, http.StatusUnauthorized, status)
}

// lateBlockStore reports the user blocked from the second user-block lookup
// on: a revocation that lands between the upgrade check and the
// registration of the socket.
type lateBlockStore struct {
	*failingStore
	calls atomic.Int32
}

func (s *lateBlockStore) IsUserBlocked(ctx context.Context, id string, at time.Time) (bool, error) {
	if s.calls.Add(1) > 1 {
		return true, nil
	}
	return s.failingStore.IsUserBlocked(ctx, id, at)
}

// TestWSRevocationDuringUpgradeClosesTheSocket: the re-check after the
// registration closes a socket whose credential was revoked during the
// upgrade, without waiting for the revalidation (default interval 1 min).
func TestWSRevocationDuringUpgradeClosesTheSocket(t *testing.T) {
	t.Parallel()
	store := &lateBlockStore{failingStore: newFailingStore(t)}
	env := newAuthEnv(t, nil, func(_ *authEnv, d *AuthDeps) { d.SessionStore = store })
	user := env.createUser(t, "alice", models.RoleUser)
	target := wsURL(t, env.router)

	conn, status := dialWS(t, withToken(target, env.token(t, user)), nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	assert.Equal(t, gorilla.ClosePolicyViolation, waitClosed(t, conn))
}

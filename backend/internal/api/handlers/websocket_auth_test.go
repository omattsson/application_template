package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSocketIdentityFromContext(t *testing.T) {
	t.Parallel()
	env := newKeyEnv(t, nil)
	key := env.create(t, "u-dev", `{"name":"ci","expires_in_days":2}`)

	var got websocket.ClientIdentity
	r := gin.New()
	r.GET("/id", middleware.CombinedAuth(middleware.APIKeyAuthDeps{
		JWTSecret: testJWTSecret, APIKeyRepo: env.keys, UserRepo: env.users,
	}), func(c *gin.Context) {
		got = WebSocketIdentityFromContext(c)
		c.Status(http.StatusNoContent)
	})
	r.GET("/open", func(c *gin.Context) {
		got = WebSocketIdentityFromContext(c)
		c.Status(http.StatusNoContent)
	})
	serve := func(path string, h map[string]string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	}

	t.Run("jwt", func(t *testing.T) {
		tok, err := middleware.GenerateTokenWithOpts(middleware.GenerateTokenOptions{
			UserID: "u-alice", Username: "alice", Role: models.RoleUser, Secret: testJWTSecret,
			Expiration: time.Hour, SessionID: "fam-1",
		})
		require.NoError(t, err)
		claims, err := middleware.ValidateJWT(tok, testJWTSecret)
		require.NoError(t, err)
		serve("/id", map[string]string{"Authorization": "Bearer " + tok})
		assert.Equal(t, "u-alice", got.UserID)
		assert.Equal(t, models.RoleUser, got.Role)
		assert.Equal(t, claims.ID, got.TokenID)
		assert.Equal(t, "fam-1", got.SessionID)
		assert.Empty(t, got.APIKeyID)
		assert.True(t, got.ExpiresAt.Equal(claims.ExpiresAt.Time))
		assert.True(t, got.IssuedAt.Equal(claims.IssueTime()), "millisecond issue time")
	})

	t.Run("api key", func(t *testing.T) {
		before := time.Now()
		serve("/id", map[string]string{"X-API-Key": key.RawKey})
		assert.Equal(t, "u-dev", got.UserID)
		assert.Equal(t, models.RoleDevOps, got.Role)
		assert.Equal(t, key.ID, got.APIKeyID)
		assert.Empty(t, got.TokenID)
		require.NotNil(t, key.ExpiresAt)
		assert.True(t, got.ExpiresAt.Equal(*key.ExpiresAt))
		assert.False(t, got.IssuedAt.Before(before.Truncate(time.Millisecond)), "a key socket is issued at the connection time")
	})

	t.Run("no auth", func(t *testing.T) {
		serve("/open", nil)
		assert.Equal(t, websocket.ClientIdentity{}, got)
	})
}

// checkerEnv holds the stores of the revocation checker tests.
type checkerEnv struct {
	keys  *keyEnv
	store sessionstore.SessionStore
	key   createdKey
}

func newCheckerEnv(t *testing.T) *checkerEnv {
	t.Helper()
	keys := newKeyEnv(t, nil)
	store := sessionstore.NewMemory()
	t.Cleanup(store.Stop)
	return &checkerEnv{keys: keys, store: store, key: keys.create(t, "u-alice", `{"name":"ci","expires_in_days":2}`)}
}

// errUsers fails every user lookup.
type errUsers struct{ models.UserRepository }

func (errUsers) FindByID(context.Context, string) (*models.User, error) {
	return nil, errors.New("dial tcp: database down")
}

// errStore fails every blocklist lookup.
type errStore struct{ sessionstore.SessionStore }

func (errStore) IsTokenBlocked(context.Context, string) (bool, error) {
	return false, errors.New("dial tcp: store down")
}

func (errStore) IsSessionBlocked(context.Context, string) (bool, error) {
	return false, errors.New("dial tcp: store down")
}

func (errStore) IsUserBlocked(context.Context, string, time.Time) (bool, error) {
	return false, errors.New("dial tcp: store down")
}

func TestWebSocketRevocationChecker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)

	env := newCheckerEnv(t)
	require.NoError(t, env.store.BlockToken(ctx, "jti-blocked", time.Now().Add(time.Hour)))
	require.NoError(t, env.store.BlockSession(ctx, "fam-blocked", time.Now().Add(time.Hour)))
	require.NoError(t, env.store.BlockUser(ctx, "u-bob", time.Now().Add(time.Hour)))

	ids := []websocket.ClientIdentity{
		{UserID: "u-alice", TokenID: "jti-ok", SessionID: "fam-ok", IssuedAt: past},                 // 0 fine
		{UserID: "u-alice", TokenID: "jti-blocked", IssuedAt: past},                                 // 1 logout
		{UserID: "u-alice", TokenID: "jti-2", SessionID: "fam-blocked", IssuedAt: past},             // 2 session logout
		{UserID: "u-bob", TokenID: "jti-3", IssuedAt: past},                                         // 3 user block
		{UserID: "u-off", TokenID: "jti-4", IssuedAt: time.Now()},                                   // 4 disabled
		{UserID: "u-ghost", TokenID: "jti-5", IssuedAt: time.Now()},                                 // 5 deleted
		{UserID: "u-alice", APIKeyID: env.key.ID, IssuedAt: time.Now()},                             // 6 key fine
		{UserID: "u-alice", APIKeyID: "00000000-0000-4000-8000-00000000dead", IssuedAt: time.Now()}, // 7 key deleted
		{UserID: "u-bob", APIKeyID: env.key.ID, IssuedAt: time.Now()},                               // 8 key of another user
	}
	want := []bool{false, true, true, true, true, true, false, true, true}

	for _, failClosed := range []bool{false, true} {
		check := NewWebSocketRevocationChecker(env.store, env.keys.users, env.keys.keys, failClosed)
		require.NotNil(t, check)
		assert.Equal(t, want, check(ctx, ids), "fail closed %v", failClosed)
	}
}

func TestWebSocketRevocationCheckerLookupErrors(t *testing.T) {
	t.Parallel()
	env := newCheckerEnv(t)
	ids := []websocket.ClientIdentity{
		{UserID: "u-alice", TokenID: "a", IssuedAt: time.Now()},
		{UserID: "u-bob", TokenID: "b", IssuedAt: time.Now()},
	}

	tests := []struct {
		name       string
		store      sessionstore.SessionStore
		users      models.UserRepository
		keys       models.APIKeyRepository
		ids        []websocket.ClientIdentity
		failClosed bool
		want       []bool
	}{
		{name: "store error, fail open", store: errStore{env.store}, users: env.keys.users, ids: ids, want: []bool{false, false}},
		{name: "store error, fail closed", store: errStore{env.store}, users: env.keys.users, ids: ids, failClosed: true, want: []bool{true, true}},
		{name: "user error, fail open", store: env.store, users: errUsers{env.keys.users}, ids: ids, want: []bool{false, false}},
		{name: "user error, fail closed", store: env.store, users: errUsers{env.keys.users}, ids: ids, failClosed: true, want: []bool{true, true}},
		{name: "key error, fail closed", store: env.store, users: env.keys.users,
			keys: failingAPIKeyRepo{APIKeyRepository: env.keys.keys, err: errors.New("dial tcp"), ops: map[string]bool{"find": true}},
			ids:  []websocket.ClientIdentity{{UserID: "u-alice", APIKeyID: env.key.ID, IssuedAt: time.Now()}}, failClosed: true, want: []bool{true}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := NewWebSocketRevocationChecker(tt.store, tt.users, tt.keys, tt.failClosed)
			assert.Equal(t, tt.want, check(context.Background(), tt.ids))
		})
	}

	t.Run("cancelled context (hub shutdown) closes nothing", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		check := NewWebSocketRevocationChecker(env.store, env.keys.users, env.keys.keys, true)
		assert.Equal(t, []bool{false, false}, check(ctx, ids))
	})

	t.Run("no dependencies", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, NewWebSocketRevocationChecker(nil, nil, nil, true))
	})
}

// TestHandleWebSocket_RecheckAfterRegistration: a revocation between the auth
// check and the registration is caught by the re-check: the socket closes.
func TestHandleWebSocket_RecheckAfterRegistration(t *testing.T) {
	t.Parallel()

	for _, revoked := range []bool{false, true} {
		hub := websocket.NewHub()
		go hub.Run()
		handler := NewWebSocketHandler(hub, "*")
		handler.SetIdentityResolver(func(*gin.Context) websocket.ClientIdentity {
			return websocket.ClientIdentity{UserID: "u1"}
		})
		var seen []websocket.ClientIdentity
		handler.SetRecheck(func(_ context.Context, ids []websocket.ClientIdentity) []bool {
			seen = append(seen, ids...)
			return []bool{revoked}
		})
		url := newWSTestServer(t, handler)

		conn, _, err := gorilla.DefaultDialer.Dial(url, nil)
		require.NoError(t, err)
		if revoked {
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
			_, _, err = conn.ReadMessage()
			var ce *gorilla.CloseError
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, gorilla.ClosePolicyViolation, ce.Code)
			waitForHubClients(t, hub, 0)
		} else {
			waitForHubClients(t, hub, 1)
		}
		require.Len(t, seen, 1)
		assert.Equal(t, "u1", seen[0].UserID)
		_ = conn.Close()
		hub.Shutdown()
	}
}

func TestDefaultTopicAuthorizer(t *testing.T) {
	t.Parallel()
	user := websocket.ClientIdentity{UserID: "u1", Role: models.RoleUser}
	devops := websocket.ClientIdentity{UserID: "u2", Role: models.RoleDevOps}
	admin := websocket.ClientIdentity{UserID: "u3", Role: models.RoleAdmin}
	noRole := websocket.ClientIdentity{UserID: "u4"}

	tests := []struct {
		id    websocket.ClientIdentity
		topic string
		want  bool
	}{
		{user, "items", true},
		{user, "item:42", true},
		{noRole, "item:42", false},
		{user, "user:u1", true},
		{user, "user:u2", false},
		{admin, "user:u1", true},
		{user, "admin:audit", false},
		{devops, "admin:audit", false},
		{admin, "admin:audit", true},
		{user, "item:", false},
		{user, "user:", false},
		{admin, "admin:", false},
		{user, "unknown", false},
		{admin, "unknown:1", false},
		{user, "ITEMS", false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, DefaultTopicAuthorizer(tt.id, tt.topic), "%s/%s on %q", tt.id.UserID, tt.id.Role, tt.topic)
	}
}

package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/ctxutil"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditSink is an AuditLogger that records the entries it gets.
type auditSink struct {
	err     error
	gate    chan struct{} // when set, Create waits for it to be closed
	ctxSeen []context.Context
	entries []*models.AuditLog
	mu      sync.Mutex
	calls   atomic.Int64
}

func (s *auditSink) Create(ctx context.Context, log *models.AuditLog) error {
	s.calls.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxSeen = append(s.ctxSeen, ctx)
	if s.err != nil {
		return s.err
	}
	cp := *log
	s.entries = append(s.entries, &cp)
	return nil
}

func (s *auditSink) snapshot() []*models.AuditLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*models.AuditLog(nil), s.entries...)
}

// identity sets the context values of an authenticated caller.
func identity(userID, username, role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if userID != "" {
			c.Set(contextKeyUserID, userID)
			c.Set(contextKeyUsername, username)
			c.Set(contextKeyRole, role)
			c.Set(contextKeyAuthMethod, AuthMethodJWT)
		}
		c.Next()
	}
}

// auditRouter builds an engine with the identity middleware, the audit
// middleware and a few routes.
func auditRouter(rec *AuditRecorder, userID string) *gin.Engine {
	r := gin.New()
	r.Use(RequestID(), identity(userID, "alice", models.RoleAdmin), rec.Middleware())
	ok := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }
	r.GET("/api/v1/items", ok)
	r.POST("/api/v1/items", func(c *gin.Context) {
		SetAuditEntityID(c, "42")
		c.JSON(http.StatusCreated, gin.H{"id": 42, "password": "response-secret"})
	})
	r.PUT("/api/v1/items/:id", ok)
	r.PATCH("/api/v1/items/:id", ok)
	r.DELETE("/api/v1/items/:id", ok)
	r.PUT("/api/v1/users/:id/role", func(c *gin.Context) {
		AddAuditDetail(c, "old_role", "user")
		AddAuditDetail(c, "new_role", "admin")
		AddAuditDetail(c, "method", "FORGED")
		AddAuditDetail(c, "status", 1)
		c.JSON(http.StatusOK, gin.H{})
	})
	r.PUT("/api/v1/users/:id/disabled", func(c *gin.Context) {
		SetAuditAction(c, AuditActionEnable)
		c.JSON(http.StatusOK, gin.H{})
	})
	r.PUT("/api/v1/auth/me/password", func(c *gin.Context) {
		SetAuditEntityID(c, "u-1")
		AddAuditDetail(c, "password_changed", true)
		c.JSON(http.StatusOK, gin.H{"message": "Password changed", "password": "response-secret"})
	})
	r.POST("/api/v1/auth/password-reset/confirm", func(c *gin.Context) {
		if c.Query("actor") != "" {
			SetAuditActor(c, "u-9", "carol")
			SetAuditEntityID(c, "u-9")
			AddAuditDetail(c, "password_reset", true)
		}
		c.Status(http.StatusNoContent)
	})
	r.POST("/api/v1/auth/password-reset", func(c *gin.Context) {
		SetAuditActor(c, "u-9", "carol")
		c.JSON(http.StatusAccepted, gin.H{})
	})
	r.PUT("/api/v1/auth/me/password-fail", func(c *gin.Context) { c.JSON(http.StatusUnauthorized, gin.H{}) })
	r.POST("/api/v1/auth/logout-all", ok)
	r.POST("/api/v1/auth/login", ok)
	r.DELETE("/api/v1/things/:id", func(c *gin.Context) { c.Status(http.StatusForbidden) })
	r.POST("/api/v1/fail", func(c *gin.Context) { c.JSON(http.StatusInternalServerError, gin.H{}) })
	r.POST("/api/v1/skip", func(c *gin.Context) { SkipAudit(c); c.JSON(http.StatusOK, gin.H{}) })
	r.POST("/api/v1/panic", func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	return r
}

func serve(r http.Handler, method, target, body string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.7:4711"
	for _, m := range mods {
		m(req)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func waitRecorded(t *testing.T, rec *AuditRecorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, rec.Shutdown(ctx))
}

func detailsOf(t *testing.T, e *models.AuditLog) map[string]any {
	t.Helper()
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(e.Details), &d))
	return d
}

func TestAuditMutatingRequestWritesOneRow(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)
	r := auditRouter(rec, "u-1")

	w := serve(r, http.MethodPost, "/api/v1/items?token=abc&x=1", `{"name":"n","password":"request-secret"}`,
		func(req *http.Request) {
			req.Header.Set("X-Request-ID", "req-123")
			req.Header.Set("Authorization", "Bearer sekrit-token")
		})
	require.Equal(t, http.StatusCreated, w.Code)
	waitRecorded(t, rec)

	got := sink.snapshot()
	require.Len(t, got, 1)
	e := got[0]
	assert.Equal(t, "u-1", e.UserID)
	assert.Equal(t, "alice", e.Username)
	assert.Equal(t, AuditActionCreate, e.Action)
	assert.Equal(t, AuditEntityItem, e.EntityType)
	assert.Equal(t, "42", e.EntityID)
	assert.NotEmpty(t, e.ID)
	assert.WithinDuration(t, time.Now(), e.Timestamp, 5*time.Second)

	d := detailsOf(t, e)
	assert.Equal(t, "POST", d["method"])
	assert.Equal(t, "/api/v1/items", d["path"], "the route template, never the query string")
	assert.EqualValues(t, 201, d["status"])
	assert.Equal(t, "192.0.2.7", d["client_ip"])
	assert.Equal(t, "req-123", d["request_id"])

	for _, secret := range []string{"request-secret", "response-secret", "sekrit-token", "token=abc", "abc"} {
		assert.NotContains(t, e.Details, secret, "no body, header or query value is recorded")
	}
}

func TestAuditMethodsAndActions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method string
		target string
		action string
		entity string
		id     string
		want   int
	}{
		{http.MethodPut, "/api/v1/items/7", AuditActionUpdate, AuditEntityItem, "7", 1},
		{http.MethodPatch, "/api/v1/items/7", AuditActionUpdate, AuditEntityItem, "7", 1},
		{http.MethodDelete, "/api/v1/items/7", AuditActionDelete, AuditEntityItem, "7", 1},
		{http.MethodGet, "/api/v1/items", "", "", "", 0},
		{http.MethodHead, "/api/v1/items", "", "", "", 0},
		{http.MethodOptions, "/api/v1/items", "", "", "", 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			t.Parallel()
			sink := &auditSink{}
			rec := NewAuditRecorder(sink)
			serve(auditRouter(rec, "u-1"), tt.method, tt.target, "")
			waitRecorded(t, rec)
			got := sink.snapshot()
			require.Len(t, got, tt.want)
			if tt.want == 1 {
				assert.Equal(t, tt.action, got[0].Action)
				assert.Equal(t, tt.entity, got[0].EntityType)
				assert.Equal(t, tt.id, got[0].EntityID)
			}
		})
	}
}

func TestAuditNotRecorded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		user   string
		method string
		target string
	}{
		{"unauthenticated", "", http.MethodPost, "/api/v1/items"},
		{"client error", "u-1", http.MethodDelete, "/api/v1/things/1"},
		{"server error", "u-1", http.MethodPost, "/api/v1/fail"},
		{"handler opts out", "u-1", http.MethodPost, "/api/v1/skip"},
		{"aborted by auth", "u-1", http.MethodPost, "/api/v1/panic"},
		{"auth endpoint logout-all", "u-1", http.MethodPost, "/api/v1/auth/logout-all"},
		{"auth endpoint login", "u-1", http.MethodPost, "/api/v1/auth/login"},
		{"unknown route", "u-1", http.MethodPost, "/api/v1/nope"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := &auditSink{}
			rec := NewAuditRecorder(sink)
			serve(auditRouter(rec, tt.user), tt.method, tt.target, "")
			waitRecorded(t, rec)
			assert.Zero(t, sink.calls.Load())
		})
	}
}

func TestAuditRouteTableAndOverrides(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)
	r := auditRouter(rec, "u-1")

	serve(r, http.MethodPut, "/api/v1/users/0b1c8f0e-0000-4000-8000-000000000001/role", "")
	serve(r, http.MethodPut, "/api/v1/users/0b1c8f0e-0000-4000-8000-000000000002/disabled", "")
	waitRecorded(t, rec)

	got := sink.snapshot()
	require.Len(t, got, 2)
	byID := map[string]*models.AuditLog{}
	for _, e := range got {
		byID[e.EntityID] = e
	}
	role := byID["0b1c8f0e-0000-4000-8000-000000000001"]
	require.NotNil(t, role)
	assert.Equal(t, AuditActionChangeRole, role.Action)
	assert.Equal(t, AuditEntityUser, role.EntityType)
	d := detailsOf(t, role)
	assert.Equal(t, "user", d["old_role"])
	assert.Equal(t, "admin", d["new_role"])
	assert.Equal(t, "PUT", d["method"], "a handler cannot overwrite a reserved key")
	assert.EqualValues(t, 200, d["status"])

	dis := byID["0b1c8f0e-0000-4000-8000-000000000002"]
	require.NotNil(t, dis)
	assert.Equal(t, AuditActionEnable, dis.Action, "SetAuditAction overrides the table action")
	assert.Equal(t, AuditEntityUser, dis.EntityType)
}

// The password change is under /auth/ but has an explicit table entry: it is
// recorded with the action change_password, the entity the handler named and
// no secret.
func TestAuditRecordsThePasswordChange(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)
	r := auditRouter(rec, "u-1")

	serve(r, http.MethodPut, "/api/v1/auth/me/password", `{"current_password":"request-secret","new_password":"request-secret-2"}`)
	serve(r, http.MethodPut, "/api/v1/auth/me/password-fail", "")
	waitRecorded(t, rec)

	got := sink.snapshot()
	require.Len(t, got, 1, "a refused change is not recorded")
	e := got[0]
	assert.Equal(t, AuditActionChangePassword, e.Action)
	assert.Equal(t, AuditEntityUser, e.EntityType)
	assert.Equal(t, "u-1", e.EntityID)
	assert.Equal(t, "u-1", e.UserID)
	d := detailsOf(t, e)
	assert.Equal(t, "/api/v1/auth/me/password", d["path"])
	assert.Equal(t, true, d["password_changed"])
	for _, secret := range []string{"request-secret", "response-secret"} {
		assert.NotContains(t, e.Details, secret)
	}
}

// The password reset has no session: the handler names the account it reset
// (SetAuditActor) and the recorder writes the entry for it.
func TestAuditRecordsThePasswordReset(t *testing.T) {
	t.Parallel()

	t.Run("the handler names the actor", func(t *testing.T) {
		t.Parallel()
		sink := &auditSink{}
		rec := NewAuditRecorder(sink)
		r := auditRouter(rec, "") // no authenticated caller

		w := serve(r, http.MethodPost, "/api/v1/auth/password-reset/confirm?actor=1", `{"token":"request-secret","new_password":"request-secret-2"}`)
		require.Equal(t, http.StatusNoContent, w.Code)
		waitRecorded(t, rec)

		got := sink.snapshot()
		require.Len(t, got, 1)
		e := got[0]
		assert.Equal(t, AuditActionPasswordReset, e.Action)
		assert.Equal(t, AuditEntityUser, e.EntityType)
		assert.Equal(t, "u-9", e.EntityID)
		assert.Equal(t, "u-9", e.UserID, "the actor is the account that was reset")
		assert.Equal(t, "carol", e.Username)
		d := detailsOf(t, e)
		assert.Equal(t, "/api/v1/auth/password-reset/confirm", d["path"])
		assert.Equal(t, true, d["password_reset"])
		assert.NotContains(t, e.Details, "request-secret")
	})

	t.Run("no actor, no entry", func(t *testing.T) {
		t.Parallel()
		sink := &auditSink{}
		rec := NewAuditRecorder(sink)
		r := auditRouter(rec, "")

		serve(r, http.MethodPost, "/api/v1/auth/password-reset/confirm", `{}`)
		waitRecorded(t, rec)
		assert.Empty(t, sink.snapshot(), "a refused confirm names nobody and is not recorded")
	})

	t.Run("the request route is never recorded", func(t *testing.T) {
		t.Parallel()
		sink := &auditSink{}
		rec := NewAuditRecorder(sink)
		r := auditRouter(rec, "")

		serve(r, http.MethodPost, "/api/v1/auth/password-reset", `{"username":"carol"}`)
		waitRecorded(t, rec)
		assert.Empty(t, sink.snapshot())
	})

	t.Run("an authenticated caller wins over an actor", func(t *testing.T) {
		t.Parallel()
		sink := &auditSink{}
		rec := NewAuditRecorder(sink)
		r := auditRouter(rec, "u-1")

		serve(r, http.MethodPost, "/api/v1/auth/password-reset/confirm?actor=1", `{}`)
		waitRecorded(t, rec)
		got := sink.snapshot()
		require.Len(t, got, 1)
		assert.Equal(t, "u-1", got[0].UserID)
	})
}

func TestAuditRouteFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method, path string
		want         AuditRoute
	}{
		{"POST", "/api/v1/items", AuditRoute{Action: AuditActionCreate, EntityType: AuditEntityItem, CRUD: true}},
		{"PUT", "/api/v1/items/:id", AuditRoute{Action: AuditActionUpdate, EntityType: AuditEntityItem, CRUD: true}},
		{"DELETE", "/api/v1/items/:id", AuditRoute{Action: AuditActionDelete, EntityType: AuditEntityItem, CRUD: true}},
		{"POST", "/api/v1/users", AuditRoute{Action: AuditActionCreate, EntityType: AuditEntityUser, CRUD: true}},
		{"PUT", "/api/v1/users/:id", AuditRoute{Action: AuditActionUpdate, EntityType: AuditEntityUser, CRUD: true}},
		{"DELETE", "/api/v1/users/:id", AuditRoute{Action: AuditActionDelete, EntityType: AuditEntityUser, CRUD: true}},
		{"POST", "/api/v1/api-keys", AuditRoute{Action: AuditActionCreate, EntityType: AuditEntityAPIKey, CRUD: true}},
		{"DELETE", "/api/v1/api-keys/:id", AuditRoute{Action: AuditActionDelete, EntityType: AuditEntityAPIKey, CRUD: true}},
		{"PUT", "/api/v1/users/:id/role", auditRoutes["PUT /users/:id/role"]},
		// An explicit table entry wins over the /auth/ skip.
		{"PUT", "/api/v1/auth/me/password", AuditRoute{Action: AuditActionChangePassword, EntityType: AuditEntityUser, IDFromHandler: true}},
		{"POST", "/api/v1/auth/password-reset/confirm", AuditRoute{Action: AuditActionPasswordReset, EntityType: AuditEntityUser, IDFromHandler: true}},
		{"POST", "/api/v1/auth/password-reset", AuditRoute{Skip: true}},
		{"POST", "/api/v1/auth/logout", AuditRoute{Skip: true}},
		{"PUT", "/api/v1/auth/me", AuditRoute{Skip: true}},
		{"POST", "/api/v1/auth/me/password", AuditRoute{Skip: true}},
		{"POST", "/api/v1/auth/oidc/callback", AuditRoute{Skip: true}},
		{"POST", "/api/v1/some-things", AuditRoute{Action: AuditActionCreate, EntityType: "some_things", CRUD: true}},
		{"POST", "", AuditRoute{Action: AuditActionCreate, EntityType: "unknown", CRUD: true}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, AuditRouteFor(tt.method, tt.path))
		})
	}
}

func TestAuditRouteTableUsesKnownValues(t *testing.T) {
	t.Parallel()
	for key, route := range auditRoutes {
		if route.Skip {
			continue
		}
		assert.Contains(t, KnownAuditActions, route.Action, key)
		assert.Contains(t, KnownAuditEntityTypes, route.EntityType, key)
		assert.True(t, route.IDParam != "" || route.IDFromHandler, "%s: the entity ID comes from a path parameter or the handler", key)
	}
	for _, a := range []string{AuditActionCreate, AuditActionUpdate, AuditActionDelete, AuditActionChangeRole, AuditActionDisable, AuditActionEnable, AuditActionChangePassword, AuditActionPasswordReset} {
		assert.Contains(t, KnownAuditActions, a)
	}
	for _, e := range []string{AuditEntityItem, AuditEntityUser, AuditEntityAPIKey} {
		assert.Contains(t, KnownAuditEntityTypes, e)
	}
	assert.True(t, IsKnownAuditAction(AuditActionDisable))
	assert.False(t, IsKnownAuditAction("drop_table"))
	assert.True(t, IsKnownAuditEntityType(AuditEntityAPIKey))
	assert.False(t, IsKnownAuditEntityType("x"))
}

func TestAuditFailedWriteDoesNotFailTheRequest(t *testing.T) {
	// Not parallel: it swaps the default logger.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	sink := &auditSink{err: errors.New("dial tcp 10.0.0.9: connection refused")}
	rec := NewAuditRecorder(sink)
	w := serve(auditRouter(rec, "u-1"), http.MethodPost, "/api/v1/items", "{}")
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.NotContains(t, w.Body.String(), "dial tcp")
	waitRecorded(t, rec)
	assert.EqualValues(t, 1, sink.calls.Load())
	assert.Contains(t, logs.String(), "failed to create audit log")
	assert.Contains(t, logs.String(), `"user_id":"u-1"`)
}

func TestAuditSlowWriteDoesNotDelayTheResponse(t *testing.T) {
	t.Parallel()
	sink := &auditSink{gate: make(chan struct{})}
	rec := NewAuditRecorder(sink)
	r := auditRouter(rec, "u-1")

	start := time.Now()
	w := serve(r, http.MethodPost, "/api/v1/items", "{}")
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Less(t, time.Since(start), 2*time.Second, "the response does not wait for the audit write")

	// Shutdown waits for the write that is still in flight, up to its own
	// context.
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, rec.Shutdown(short), context.DeadlineExceeded)

	close(sink.gate)
	waitRecorded(t, rec)
	assert.Len(t, sink.snapshot(), 1)
}

func TestAuditInFlightWritesAreBounded(t *testing.T) {
	t.Parallel()
	sink := &auditSink{gate: make(chan struct{})}
	rec := newAuditRecorder(sink, 2)
	r := auditRouter(rec, "u-1")

	for i := 0; i < 5; i++ {
		w := serve(r, http.MethodPost, "/api/v1/items", "{}")
		require.Equal(t, http.StatusCreated, w.Code, "a full queue never fails the request")
	}
	assert.EqualValues(t, 3, rec.Dropped(), "writes over the bound are dropped and counted")
	close(sink.gate)
	waitRecorded(t, rec)
	assert.Len(t, sink.snapshot(), 2)

	// A slot is free again once its write ended: a second entry is recorded,
	// not dropped.
	sink2 := &auditSink{}
	rec2 := newAuditRecorder(sink2, 1)
	r2 := auditRouter(rec2, "u-1")
	for i := 1; i <= 2; i++ {
		require.Equal(t, http.StatusCreated, serve(r2, http.MethodPost, "/api/v1/items", "{}").Code)
		require.Eventually(t, func() bool { return len(sink2.snapshot()) == i && len(rec2.sem) == 0 },
			5*time.Second, time.Millisecond, "write %d ended and released its slot", i)
	}
	assert.Zero(t, rec2.Dropped(), "no entry was dropped")
	waitRecorded(t, rec2)
}

func TestAuditAfterShutdownDropsAndDoesNotPanic(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)
	waitRecorded(t, rec)
	w := serve(auditRouter(rec, "u-1"), http.MethodPost, "/api/v1/items", "{}")
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Zero(t, sink.calls.Load())
	assert.EqualValues(t, 1, rec.Dropped())
	// Shutdown twice is fine.
	waitRecorded(t, rec)
}

func TestAuditWriteContextIsDetachedAndBounded(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)

	// The client leaves while the handler runs: the handler cancels the
	// request context itself, so the cancel always comes before the audit
	// middleware submits the entry, with no timing involved.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	r := gin.New()
	r.Use(RequestID(), identity("u-1", "alice", models.RoleAdmin), rec.Middleware())
	r.POST("/api/v1/items", func(c *gin.Context) {
		SetAuditEntityID(c, "42")
		cancelReq()
		c.JSON(http.StatusCreated, gin.H{})
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader("{}")).WithContext(reqCtx)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Error(t, reqCtx.Err(), "the request context ended before the entry was written")
	waitRecorded(t, rec)

	require.Len(t, sink.snapshot(), 1, "a client that left does not skip the entry")
	require.Len(t, sink.ctxSeen, 1)
	deadline, ok := sink.ctxSeen[0].Deadline()
	require.True(t, ok, "the write has a deadline of its own")
	assert.LessOrEqual(t, time.Until(deadline), ctxutil.WriteTimeout)
}

func TestAuditClientIPFollowsTrustedProxies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		proxies []string
		remote  string
		want    string
	}{
		{"no trusted proxy: header ignored", nil, "192.0.2.7:1", "192.0.2.7"},
		{"trusted proxy: header used", []string{"192.0.2.0/24"}, "192.0.2.7:1", "203.0.113.9"},
		{"untrusted peer: header ignored", []string{"10.0.0.0/8"}, "192.0.2.7:1", "192.0.2.7"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := &auditSink{}
			rec := NewAuditRecorder(sink)
			r := auditRouter(rec, "u-1")
			require.NoError(t, r.SetTrustedProxies(tt.proxies))
			serve(r, http.MethodPost, "/api/v1/items", "{}", func(req *http.Request) {
				req.RemoteAddr = tt.remote
				req.Header.Set("X-Forwarded-For", "203.0.113.9")
			})
			waitRecorded(t, rec)
			got := sink.snapshot()
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, detailsOf(t, got[0])["client_ip"])
		})
	}
}

func TestAuditEntriesFitTheirColumns(t *testing.T) {
	t.Parallel()

	t.Run("entity id is cut at a rune boundary", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("é", 100)
		got := truncateEntityID(long)
		assert.LessOrEqual(t, len(got), models.MaxAuditEntityIDLen)
		assert.True(t, strings.HasPrefix(long, got))
		assert.Equal(t, "short", truncateEntityID("short"))
	})
	t.Run("path is bounded before insert", func(t *testing.T) {
		t.Parallel()
		got := truncateAuditText(strings.Repeat("a", 1000), maxAuditPathLen)
		assert.Len(t, got, maxAuditPathLen)
		assert.Equal(t, "/x", truncateAuditText("/x", maxAuditPathLen))
	})
	t.Run("a long route template is stored truncated", func(t *testing.T) {
		t.Parallel()
		sink := &auditSink{}
		rec := NewAuditRecorder(sink)
		r := gin.New()
		r.Use(identity("u-1", "alice", models.RoleAdmin), rec.Middleware())
		long := "/api/v1/" + strings.Repeat("seg/", 200) + ":id"
		r.POST(long, func(c *gin.Context) { c.Status(http.StatusOK) })
		serve(r, http.MethodPost, "/api/v1/"+strings.Repeat("seg/", 200)+"9", "")
		waitRecorded(t, rec)
		got := sink.snapshot()
		require.Len(t, got, 1)
		assert.LessOrEqual(t, len(detailsOf(t, got[0])["path"].(string)), maxAuditPathLen)
	})
	t.Run("oversized extra details are dropped, the entry stays", func(t *testing.T) {
		t.Parallel()
		sink := &auditSink{}
		rec := NewAuditRecorder(sink)
		r := gin.New()
		r.Use(identity("u-1", "alice", models.RoleAdmin), rec.Middleware())
		r.POST("/api/v1/items", func(c *gin.Context) {
			AddAuditDetail(c, "blob", strings.Repeat("x", 100000))
			c.Status(http.StatusOK)
		})
		serve(r, http.MethodPost, "/api/v1/items", "")
		waitRecorded(t, rec)
		got := sink.snapshot()
		require.Len(t, got, 1)
		assert.Less(t, len(got[0].Details), maxAuditDetailsLen)
		assert.NotContains(t, got[0].Details, "blob")
	})
}

func TestAuditManyParallelRequestsAreAllRecorded(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)
	r := auditRouter(rec, "u-1")
	var wg sync.WaitGroup
	for i := 0; i < DefaultAuditMaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(r, http.MethodPut, "/api/v1/items/1", "")
		}()
	}
	wg.Wait()
	waitRecorded(t, rec)
	assert.Len(t, sink.snapshot(), DefaultAuditMaxInFlight)
	assert.Zero(t, rec.Dropped())
}

func TestAuditCommittedRecordsAnErrorAnswer(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)
	r := gin.New()
	r.Use(identity("u-1", "alice", models.RoleAdmin), rec.Middleware())
	r.PUT("/api/v1/users/:id/role", func(c *gin.Context) {
		AuditCommitted(c)
		c.JSON(http.StatusInternalServerError, gin.H{})
	})
	r.DELETE("/api/v1/items/:id", func(c *gin.Context) { c.JSON(http.StatusInternalServerError, gin.H{}) })
	serve(r, http.MethodPut, "/api/v1/users/9/role", "")
	serve(r, http.MethodDelete, "/api/v1/items/9", "")
	waitRecorded(t, rec)
	got := sink.snapshot()
	require.Len(t, got, 1, "only the request that said it committed")
	assert.Equal(t, AuditActionChangeRole, got[0].Action)
	assert.EqualValues(t, 500, detailsOf(t, got[0])["status"])
}

func TestNewAuditMiddlewareConvenienceAndNilRepo(t *testing.T) {
	t.Parallel()

	t.Run("records through the plain constructor", func(t *testing.T) {
		t.Parallel()
		sink := &auditSink{}
		r := gin.New()
		r.Use(identity("u-1", "alice", models.RoleAdmin), NewAuditMiddleware(sink))
		r.POST("/api/v1/items", func(c *gin.Context) { c.Status(http.StatusCreated) })
		require.Equal(t, http.StatusCreated, serve(r, http.MethodPost, "/api/v1/items", "").Code)
		require.Eventually(t, func() bool { return len(sink.snapshot()) == 1 }, 5*time.Second, 5*time.Millisecond)
	})
	t.Run("a nil repository records nothing and breaks nothing", func(t *testing.T) {
		t.Parallel()
		r := gin.New()
		r.Use(identity("u-1", "alice", models.RoleAdmin), NewAuditMiddleware(nil))
		r.POST("/api/v1/items", func(c *gin.Context) { c.Status(http.StatusCreated) })
		assert.Equal(t, http.StatusCreated, serve(r, http.MethodPost, "/api/v1/items", "").Code)
	})
	t.Run("nil recorder shutdown is a no-op", func(t *testing.T) {
		t.Parallel()
		var rec *AuditRecorder
		assert.NoError(t, rec.Shutdown(context.Background()))
		assert.Zero(t, rec.Dropped())
	})
}

package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIdemStore is an in-memory models.IdempotencyRepository with the
// contract of the GORM one: one owner per (scope, key), a lock id, a stale
// takeover and a release of an in-progress row only.
type fakeIdemStore struct {
	rows        map[string]*fakeIdemRow
	claimErr    error
	completeErr error
	releaseErr  error
	now         func() time.Time
	completeCtx []ctxState
	releaseCtx  []ctxState
	claims      int
	completes   int
	releases    int
	mu          sync.Mutex
	next        int
}

// ctxState is what a store call saw of its context at the time of the call.
type ctxState struct {
	err         error
	hasDeadline bool
}

func stateOf(ctx context.Context) ctxState {
	_, ok := ctx.Deadline()
	return ctxState{err: ctx.Err(), hasDeadline: ok}
}

type fakeIdemRow struct {
	lockedAt time.Time
	hash     string
	lockID   string
	resp     models.IdempotencyResponse
}

func newFakeIdemStore() *fakeIdemStore {
	return &fakeIdemStore{rows: map[string]*fakeIdemRow{}, now: time.Now}
}

func (s *fakeIdemStore) Claim(_ context.Context, scope, key, hash string, _, lockTimeout time.Duration) (*models.IdempotencyClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	id := scope + "\x00" + key
	s.next++
	lockID := "lock-" + string(rune('a'+s.next))
	row, ok := s.rows[id]
	switch {
	case !ok, row.resp.StatusCode == 0 && row.hash == hash && !s.now().Before(row.lockedAt.Add(lockTimeout)):
		s.rows[id] = &fakeIdemRow{hash: hash, lockID: lockID, lockedAt: s.now()}
		return &models.IdempotencyClaim{Outcome: models.IdempotencyClaimed, LockID: lockID}, nil
	case row.hash != hash:
		return &models.IdempotencyClaim{Outcome: models.IdempotencyMismatch}, nil
	case row.resp.StatusCode == 0:
		return &models.IdempotencyClaim{Outcome: models.IdempotencyInProgress, RetryAfter: 42 * time.Second}, nil
	default:
		resp := row.resp
		return &models.IdempotencyClaim{Outcome: models.IdempotencyReplay, Response: &resp}, nil
	}
}

func (s *fakeIdemStore) Complete(ctx context.Context, scope, key, lockID string, resp models.IdempotencyResponse, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completes++
	s.completeCtx = append(s.completeCtx, stateOf(ctx))
	if s.completeErr != nil {
		return false, s.completeErr
	}
	row, ok := s.rows[scope+"\x00"+key]
	if !ok || row.lockID != lockID || row.resp.StatusCode != 0 {
		return false, nil
	}
	row.resp = resp
	return true, nil
}

func (s *fakeIdemStore) Release(ctx context.Context, scope, key, lockID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	s.releaseCtx = append(s.releaseCtx, stateOf(ctx))
	if s.releaseErr != nil {
		return s.releaseErr
	}
	id := scope + "\x00" + key
	if row, ok := s.rows[id]; ok && row.lockID == lockID && row.resp.StatusCode == 0 {
		delete(s.rows, id)
	}
	return nil
}

func (s *fakeIdemStore) DeleteExpired(context.Context, int) (int64, error) { return 0, nil }

func (s *fakeIdemStore) row(scope, key string) (*fakeIdemRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[scope+"\x00"+key]
	return r, ok
}

func (s *fakeIdemStore) counts() (claims, completes, releases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.completes, s.releases
}

// idemEnv is a router with the middleware and one POST handler that counts
// its runs.
type idemEnv struct {
	store   *fakeIdemStore
	router  *gin.Engine
	handler gin.HandlerFunc
	runs    int
	mu      sync.Mutex
}

func (e *idemEnv) ran() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

func newIdemEnv(t *testing.T, opts IdempotencyOptions, pre ...gin.HandlerFunc) *idemEnv {
	t.Helper()
	e := &idemEnv{store: newFakeIdemStore()}
	opts.Store = e.store
	if opts.TTL == 0 {
		opts.TTL = time.Hour
	}
	if opts.LockTimeout == 0 {
		opts.LockTimeout = time.Minute
	}
	e.handler = func(c *gin.Context) {
		c.JSON(http.StatusCreated, gin.H{"ok": true})
	}
	router := gin.New()
	router.Use(Recovery())
	for _, p := range pre {
		router.Use(p)
	}
	router.Use(Idempotency(opts))
	counted := func(c *gin.Context) {
		e.mu.Lock()
		e.runs++
		e.mu.Unlock()
		e.handler(c)
	}
	for _, path := range []string{"/api/v1/items", "/api/v1/other", "/api/v1/auth/login"} {
		router.POST(path, counted)
		router.GET(path, counted)
		router.PUT(path, counted)
		router.PATCH(path, counted)
		router.DELETE(path, counted)
	}
	e.router = router
	return e
}

func (e *idemEnv) do(method, target, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body["error"]
}

func TestIdempotencyPassesThroughWithoutKey(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	for i := 0; i < 2; i++ {
		w := e.do(http.MethodPost, "/api/v1/items", "", `{"a":1}`)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.Empty(t, w.Header().Get(IdempotencyReplayedHeader))
	}
	assert.Equal(t, 2, e.ran())
	claims, _, _ := e.store.counts()
	assert.Zero(t, claims, "no key, no query")
}

func TestIdempotencyNeverTouchesOtherMethods(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for i := 0; i < 2; i++ {
			w := e.do(m, "/api/v1/items", "k1", `{}`)
			assert.Equal(t, http.StatusCreated, w.Code, m)
			assert.Empty(t, w.Header().Get(IdempotencyReplayedHeader), m)
		}
	}
	assert.Equal(t, 8, e.ran())
	claims, _, _ := e.store.counts()
	assert.Zero(t, claims)
}

func TestIdempotencyRejectsInvalidKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"tab only", "\t"},
		{"too long", strings.Repeat("k", 256)},
		{"control character", "a\x01b"},
		{"newline inside", "a\nb"},
		{"delete character", "a\x7fb"},
		{"non ASCII", "nyckel-åäö"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newIdemEnv(t, IdempotencyOptions{})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(`{}`))
			req.Header["Idempotency-Key"] = []string{tt.key} // set the map: Set would also refuse nothing, but this skips any canonicalization
			w := httptest.NewRecorder()
			e.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
			assert.NotEmpty(t, errorOf(t, w))
			assert.Zero(t, e.ran())
			claims, _, _ := e.store.counts()
			assert.Zero(t, claims)
		})
	}

	t.Run("two header values", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/items", strings.NewReader(`{}`))
		req.Header.Add(IdempotencyKeyHeader, "a")
		req.Header.Add(IdempotencyKeyHeader, "b")
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Zero(t, e.ran())
	})
}

func TestIdempotencyAcceptsValidKeys(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]string{
		"longest":        strings.Repeat("k", 255),
		"with spaces":    "order 17 / try 1",
		"uuid":           "123e4567-e89b-12d3-a456-426614174000",
		"trimmed around": "  padded  ",
	} {
		name, key := name, key
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newIdemEnv(t, IdempotencyOptions{})
			w := e.do(http.MethodPost, "/api/v1/items", key, `{}`)
			assert.Equal(t, http.StatusCreated, w.Code)
			_, ok := e.store.row(models.IdempotencyScopeShared, strings.TrimSpace(key))
			assert.True(t, ok, "the trimmed key is the stored key")
		})
	}
}

func TestIdempotencyFirstRequestStoresAndReplays(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	e.handler = func(c *gin.Context) {
		c.Header("X-Secret-Thing", "not stored")
		c.JSON(http.StatusCreated, gin.H{"id": 7})
	}

	first := e.do(http.MethodPost, "/api/v1/items?x=1", "key-1", `{"name":"a"}`)
	require.Equal(t, http.StatusCreated, first.Code)
	assert.Empty(t, first.Header().Get(IdempotencyReplayedHeader), "the first answer is not a replay")
	assert.JSONEq(t, `{"id":7}`, first.Body.String())

	row, ok := e.store.row(models.IdempotencyScopeShared, "key-1")
	require.True(t, ok)
	assert.Equal(t, http.StatusCreated, row.resp.StatusCode)
	assert.Contains(t, row.resp.ContentType, "application/json")
	assert.JSONEq(t, `{"id":7}`, string(row.resp.Body))

	second := e.do(http.MethodPost, "/api/v1/items?x=1", "key-1", `{"name":"a"}`)
	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, "true", second.Header().Get(IdempotencyReplayedHeader))
	assert.Equal(t, first.Header().Get("Content-Type"), second.Header().Get("Content-Type"))
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Empty(t, second.Header().Get("X-Secret-Thing"), "only the content type is replayed")
	assert.Equal(t, 1, e.ran(), "the handler ran once")
}

func TestIdempotencyStoresClientErrorsAndRedirects(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusFound, http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			e := newIdemEnv(t, IdempotencyOptions{})
			e.handler = func(c *gin.Context) {
				if status == http.StatusNoContent {
					c.Status(status)
					return
				}
				c.JSON(status, gin.H{"error": "x"})
			}
			a := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
			b := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
			assert.Equal(t, status, a.Code)
			assert.Equal(t, status, b.Code)
			assert.Equal(t, "true", b.Header().Get(IdempotencyReplayedHeader))
			assert.Equal(t, a.Body.String(), b.Body.String())
			assert.Equal(t, 1, e.ran())
		})
	}
}

func TestIdempotencyDifferentRequestIs422(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		body   string
	}{
		{"body", "/api/v1/items", `{"name":"b"}`},
		{"query", "/api/v1/items?x=2", `{"name":"a"}`},
		{"added query", "/api/v1/items?y=1", `{"name":"a"}`},
		{"path", "/api/v1/other", `{"name":"a"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newIdemEnv(t, IdempotencyOptions{})
			require.Equal(t, http.StatusCreated, e.do(http.MethodPost, "/api/v1/items?x=1", "k", `{"name":"a"}`).Code)

			w := e.do(http.MethodPost, tt.target, "k", tt.body)
			assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
			assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
			assert.NotEmpty(t, errorOf(t, w))
			assert.Empty(t, w.Header().Get(IdempotencyReplayedHeader))
			assert.Equal(t, 1, e.ran())
		})
	}
}

func TestIdempotencyInProgressIs409WithRetryAfter(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	started, release := make(chan struct{}), make(chan struct{})
	e.handler = func(c *gin.Context) {
		close(started)
		<-release
		c.JSON(http.StatusCreated, gin.H{"ok": true})
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.do(http.MethodPost, "/api/v1/items", "k", `{}`) }()
	<-started

	w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "42", w.Header().Get("Retry-After"))
	assert.NotEmpty(t, errorOf(t, w))

	other := e.do(http.MethodPost, "/api/v1/items", "k", `{"different":1}`)
	assert.Equal(t, http.StatusUnprocessableEntity, other.Code, "a different request is told so, not asked to wait")

	close(release)
	first := <-done
	assert.Equal(t, http.StatusCreated, first.Code)
	assert.Equal(t, 1, e.ran())

	again := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	assert.Equal(t, "true", again.Header().Get(IdempotencyReplayedHeader))
}

func TestIdempotencyStaleLockIsTakenOver(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{LockTimeout: time.Minute})
	clock := time.Now()
	e.store.now = func() time.Time { return clock }

	// An earlier request claimed the key and never finished.
	claim, err := e.store.Claim(context.Background(), models.IdempotencyScopeShared, "k", hashOfRequest(http.MethodPost, "/api/v1/items", "", []byte(`{}`)), time.Hour, time.Minute)
	require.NoError(t, err)
	require.Equal(t, models.IdempotencyClaimed, claim.Outcome)

	assert.Equal(t, http.StatusConflict, e.do(http.MethodPost, "/api/v1/items", "k", `{}`).Code)
	clock = clock.Add(2 * time.Minute)
	assert.Equal(t, http.StatusCreated, e.do(http.MethodPost, "/api/v1/items", "k", `{}`).Code)
	assert.Equal(t, 1, e.ran())
}

func TestIdempotencyScopes(t *testing.T) {
	t.Parallel()

	t.Run("per user with auth on", func(t *testing.T) {
		t.Parallel()
		user := "user-a"
		setUser := func(c *gin.Context) { SetTestIdentity(c, user, "user", AuthMethodJWT) }
		e := newIdemEnv(t, IdempotencyOptions{PerUser: true}, setUser)

		require.Equal(t, http.StatusCreated, e.do(http.MethodPost, "/api/v1/items", "same", `{}`).Code)
		user = "user-b"
		w := e.do(http.MethodPost, "/api/v1/items", "same", `{}`)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.Empty(t, w.Header().Get(IdempotencyReplayedHeader), "another user does not see the first user's key")
		assert.Equal(t, 2, e.ran())

		_, okA := e.store.row("user-a", "same")
		_, okB := e.store.row("user-b", "same")
		assert.True(t, okA && okB)

		user = "user-a"
		assert.Equal(t, "true", e.do(http.MethodPost, "/api/v1/items", "same", `{}`).Header().Get(IdempotencyReplayedHeader))
		assert.Equal(t, 2, e.ran())
	})

	t.Run("one shared scope with auth off", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.do(http.MethodPost, "/api/v1/items", "same", `{}`)
		_, ok := e.store.row(models.IdempotencyScopeShared, "same")
		assert.True(t, ok)
	})

	t.Run("no user with auth on fails closed", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{PerUser: true})
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, "Internal server error", errorOf(t, w))
		assert.Zero(t, e.ran())
	})
}

func TestIdempotencyReleasesTheKeyOnFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		handler func(c *gin.Context)
		name    string
		status  int
	}{
		{name: "500", status: http.StatusInternalServerError, handler: func(c *gin.Context) { c.JSON(500, gin.H{"error": "Internal server error"}) }},
		{name: "503", status: http.StatusServiceUnavailable, handler: func(c *gin.Context) { c.JSON(503, gin.H{"error": "x"}) }},
		{name: "499", status: 499, handler: func(c *gin.Context) { c.JSON(499, gin.H{"error": "Client closed request"}) }},
		{name: "panic", status: http.StatusInternalServerError, handler: func(*gin.Context) { panic("boom") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newIdemEnv(t, IdempotencyOptions{})
			e.handler = tt.handler

			w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
			assert.Equal(t, tt.status, w.Code)
			assert.Empty(t, w.Header().Get(IdempotencyReplayedHeader))
			_, ok := e.store.row(models.IdempotencyScopeShared, "k")
			assert.False(t, ok, "the key is free again")

			// The client retries: the handler runs again and now succeeds.
			e.handler = func(c *gin.Context) { c.JSON(http.StatusCreated, gin.H{"ok": true}) }
			retry := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
			assert.Equal(t, http.StatusCreated, retry.Code)
			assert.Empty(t, retry.Header().Get(IdempotencyReplayedHeader))
			assert.Equal(t, 2, e.ran())
		})
	}
}

func TestIdempotencyLargeResponseIsSentButNotStored(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	big := strings.Repeat("x", MaxStoredResponseBytes+1)
	e.handler = func(c *gin.Context) {
		c.Header("Content-Type", "text/plain")
		c.Status(http.StatusCreated)
		// Several writes: the buffered part is flushed when the limit is hit.
		for i := 0; i < len(big); i += 100000 {
			end := min(i+100000, len(big))
			_, _ = c.Writer.WriteString(big[i:end])
		}
	}

	w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, big, w.Body.String(), "the whole response reaches the client")
	_, ok := e.store.row(models.IdempotencyScopeShared, "k")
	assert.False(t, ok, "the key is released")
	_, completes, releases := e.store.counts()
	assert.Zero(t, completes)
	assert.Equal(t, 1, releases)

	t.Run("exactly the limit is stored", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		exact := strings.Repeat("y", MaxStoredResponseBytes)
		e.handler = func(c *gin.Context) {
			c.Data(http.StatusOK, "text/plain", []byte(exact))
		}
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, exact, w.Body.String())
		row, ok := e.store.row(models.IdempotencyScopeShared, "k")
		require.True(t, ok)
		assert.Len(t, row.resp.Body, MaxStoredResponseBytes)
	})
}

func TestIdempotencyFlushedResponseIsNotStored(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	e.handler = func(c *gin.Context) {
		c.Status(http.StatusCreated)
		_, _ = c.Writer.WriteString("part one,")
		c.Writer.Flush()
		_, _ = c.Writer.WriteString(" part two")
	}
	w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "part one, part two", w.Body.String())
	_, ok := e.store.row(models.IdempotencyScopeShared, "k")
	assert.False(t, ok)
}

func TestIdempotencyHandlerStillReadsTheBody(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	var got string
	e.handler = func(c *gin.Context) {
		var v struct{ Name string }
		if err := c.ShouldBindJSON(&v); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad"})
			return
		}
		got = v.Name
		c.JSON(http.StatusCreated, gin.H{})
	}
	w := e.do(http.MethodPost, "/api/v1/items", "k", `{"name":"widget"}`)
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "widget", got)
}

func TestIdempotencyOversizedBodyIs413(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	router := gin.New()
	router.Use(MaxBodySize(10))
	router.Use(Idempotency(IdempotencyOptions{Store: e.store, TTL: time.Hour, LockTimeout: time.Minute}))
	router.POST("/x", func(c *gin.Context) { c.Status(http.StatusCreated) })

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", 100)))
	req.ContentLength = -1 // chunked: only a read reveals the size
	req.Header.Set(IdempotencyKeyHeader, "k")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	claims, _, _ := e.store.counts()
	assert.Zero(t, claims, "a request that cannot be read claims nothing")
}

func TestIdempotencySkipsAuthPaths(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{SkipPathPrefixes: []string{"/api/v1/auth/"}})
	for i := 0; i < 2; i++ {
		w := e.do(http.MethodPost, "/api/v1/auth/login", "k", `{}`)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.Empty(t, w.Header().Get(IdempotencyReplayedHeader))
	}
	assert.Equal(t, 2, e.ran())
	claims, _, _ := e.store.counts()
	assert.Zero(t, claims)

	// Another path is still protected.
	e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	assert.Equal(t, 3, e.ran())
}

func TestIdempotencyStoreErrors(t *testing.T) {
	t.Parallel()

	t.Run("a failed claim answers 500 and never runs the handler", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.store.claimErr = errors.New("dial tcp 10.0.0.5:3306: connection refused")
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, "Internal server error", errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "10.0.0.5")
		assert.Zero(t, e.ran())
	})

	t.Run("a cancelled request answers 499", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.store.claimErr = context.Canceled
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, 499, w.Code)
		assert.Zero(t, e.ran())
	})

	t.Run("a failed store of the response still sends the response", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.store.completeErr = errors.New("db down")
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.JSONEq(t, `{"ok":true}`, w.Body.String())
		assert.Equal(t, 1, e.ran())
	})

	t.Run("a failed release still sends the response", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.store.releaseErr = errors.New("db down")
		e.handler = func(c *gin.Context) { c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"}) }
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("a lost lock does not change the response", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.handler = func(c *gin.Context) {
			// Another request took the key over while this one ran.
			e.store.mu.Lock()
			e.store.rows[models.IdempotencyScopeShared+"\x00k"].lockID = "someone-else"
			e.store.mu.Unlock()
			c.JSON(http.StatusCreated, gin.H{"ok": true})
		}
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, http.StatusCreated, w.Code)
		row, _ := e.store.row(models.IdempotencyScopeShared, "k")
		assert.Zero(t, row.resp.StatusCode, "the row of the new owner is untouched")
	})
}

// The store of the response and the release run on a context that the client
// cannot cancel and that has a deadline.
func TestIdempotencyWritesUseADetachedContext(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, handler func(c *gin.Context)) *idemEnv {
		t.Helper()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.handler = handler
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e.router.ServeHTTP(httptest.NewRecorder(), requestWithKey(ctx, "k", `{}`, func() { cancel() }, e))
		return e
	}

	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		e := run(t, func(c *gin.Context) { c.JSON(http.StatusCreated, gin.H{}) })
		require.Len(t, e.store.completeCtx, 1)
		st := e.store.completeCtx[0]
		assert.True(t, st.hasDeadline)
		assert.NoError(t, st.err, "the cancel of the request did not reach the write")
	})

	t.Run("release", func(t *testing.T) {
		t.Parallel()
		e := run(t, func(c *gin.Context) { c.JSON(http.StatusInternalServerError, gin.H{}) })
		require.Len(t, e.store.releaseCtx, 1)
		st := e.store.releaseCtx[0]
		assert.True(t, st.hasDeadline)
		assert.NoError(t, st.err)
	})
}

// requestWithKey builds a request on ctx. The handler of e cancels the
// context (cancel) before it answers, as a client that leaves does.
func requestWithKey(ctx context.Context, key, body string, cancel func(), e *idemEnv) *http.Request {
	inner := e.handler
	e.handler = func(c *gin.Context) {
		cancel()
		inner(c)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/items", bytes.NewReader([]byte(body)))
	req.Header.Set(IdempotencyKeyHeader, key)
	return req
}

func TestIdempotencyHashIsOverMethodPathQueryAndBody(t *testing.T) {
	t.Parallel()
	base := hashOfRequest("POST", "/a", "q=1", []byte("body"))
	assert.Equal(t, base, hashOfRequest("POST", "/a", "q=1", []byte("body")))
	assert.Len(t, base, 64)
	for name, other := range map[string]string{
		"method": hashOfRequest("PUT", "/a", "q=1", []byte("body")),
		"path":   hashOfRequest("POST", "/b", "q=1", []byte("body")),
		"query":  hashOfRequest("POST", "/a", "q=2", []byte("body")),
		"body":   hashOfRequest("POST", "/a", "q=1", []byte("bodY")),
		"shift":  hashOfRequest("POST", "/a", "q=", []byte("1\nbody")),
	} {
		assert.NotEqual(t, base, other, name)
	}
}

// With the audit recorder outside, a replay, a 409 and a 422 write no audit
// entry: only the request that ran does.
func TestIdempotencyReplayWritesNoAuditEntry(t *testing.T) {
	t.Parallel()
	sink := &auditSink{}
	rec := NewAuditRecorder(sink)
	setUser := func(c *gin.Context) { SetTestIdentity(c, "user-a", "user", AuthMethodJWT) }
	e := newIdemEnv(t, IdempotencyOptions{PerUser: true}, setUser, rec.Middleware())

	require.Equal(t, http.StatusCreated, e.do(http.MethodPost, "/api/v1/items", "k", `{"a":1}`).Code)
	replay := e.do(http.MethodPost, "/api/v1/items", "k", `{"a":1}`)
	require.Equal(t, "true", replay.Header().Get(IdempotencyReplayedHeader))
	require.Equal(t, http.StatusUnprocessableEntity, e.do(http.MethodPost, "/api/v1/items", "k", `{"a":2}`).Code)

	// The 409 path: a row in progress.
	_, err := e.store.Claim(context.Background(), "user-a", "busy", hashOfRequest(http.MethodPost, "/api/v1/items", "", []byte(`{}`)), time.Hour, time.Hour)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, e.do(http.MethodPost, "/api/v1/items", "busy", `{}`).Code)

	require.NoError(t, rec.Shutdown(context.Background()))
	sink.mu.Lock()
	defer sink.mu.Unlock()
	assert.Len(t, sink.entries, 1, "one entry for the one request that ran")
	assert.Equal(t, 1, e.ran())
}

func TestIdempotencyWriterKeepsGinSemantics(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})

	t.Run("a handler that writes nothing answers 200", func(t *testing.T) {
		t.Parallel()
		e.handler = func(c *gin.Context) {}
		w := e.do(http.MethodPost, "/api/v1/items", "empty", `{}`)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Body.String())
		row, ok := e.store.row(models.IdempotencyScopeShared, "empty")
		require.True(t, ok)
		assert.Equal(t, http.StatusOK, row.resp.StatusCode)
	})

	t.Run("headers set after the status are sent", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		e.handler = func(c *gin.Context) {
			c.Status(http.StatusCreated)
			c.Header("Location", "/api/v1/items/9")
			c.Header("Content-Type", "application/json")
			_, _ = c.Writer.WriteString(`{"id":9}`)
		}
		w := e.do(http.MethodPost, "/api/v1/items", "loc", `{}`)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.Equal(t, "/api/v1/items/9", w.Header().Get("Location"))
		assert.JSONEq(t, `{"id":9}`, w.Body.String())
	})

	t.Run("the writer reports its state to the code that runs after", func(t *testing.T) {
		t.Parallel()
		e := newIdemEnv(t, IdempotencyOptions{})
		var statusAfter int
		var sizeAfter int
		var writtenAfter bool
		e.router = gin.New()
		e.router.Use(func(c *gin.Context) {
			c.Next()
			statusAfter, sizeAfter, writtenAfter = c.Writer.Status(), c.Writer.Size(), c.Writer.Written()
		})
		e.router.Use(Idempotency(IdempotencyOptions{Store: e.store, TTL: time.Hour, LockTimeout: time.Minute}))
		e.router.POST("/api/v1/items", func(c *gin.Context) { c.String(http.StatusAccepted, "abc") })
		w := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
		assert.Equal(t, http.StatusAccepted, w.Code)
		assert.Equal(t, http.StatusAccepted, statusAfter, "the audit recorder reads the real status")
		assert.Equal(t, 3, sizeAfter)
		assert.True(t, writtenAfter)
	})
}

func TestIdempotencyStoresAnAbortWithoutBody(t *testing.T) {
	t.Parallel()
	e := newIdemEnv(t, IdempotencyOptions{})
	e.handler = func(c *gin.Context) {
		c.AbortWithStatus(http.StatusForbidden)
	}
	a := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	b := e.do(http.MethodPost, "/api/v1/items", "k", `{}`)
	assert.Equal(t, http.StatusForbidden, a.Code)
	assert.Equal(t, http.StatusForbidden, b.Code)
	assert.Empty(t, a.Body.String())
	assert.Empty(t, b.Body.String())
	assert.Equal(t, "true", b.Header().Get(IdempotencyReplayedHeader))
	assert.Equal(t, 1, e.ran())
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/leader"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// authModesSecret is the JWT secret of the auth-on bootstrap tests.
const authModesSecret = "bootstrap-auth-modes-secret-0123"

// adminPassword is the ADMIN_PASSWORD of the auth-on bootstrap tests.
const adminPassword = "bootstrap-admin-password"

// openSQLiteRepo returns the repository of the bootstrap on an in-memory
// SQLite database with every table (the MySQL migrations do not run on
// SQLite).
func openSQLiteRepo(t *testing.T) (models.Repository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.Item{}, &models.User{}, &models.APIKey{}, &models.RefreshToken{},
		&models.AuditLog{}, &sessionstore.SessionEntry{}))
	return models.NewRepository(db), db
}

// authModeConfig returns the bootstrap config with AUTH_ENABLED set.
func authModeConfig(enabled bool) *config.Config {
	cfg := bootstrapTestConfig()
	if enabled {
		cfg.Auth = config.AuthConfig{
			Enabled:                true,
			JWTSecret:              authModesSecret,
			JWTExpiration:          time.Hour,
			AccessTokenExpiration:  15 * time.Minute,
			SessionIdleTimeout:     30 * time.Minute,
			SessionMaxLifetime:     12 * time.Hour,
			RefreshTokenExpiration: 168 * time.Hour,
			RefreshReuseGrace:      30 * time.Second,
			AdminUsername:          "admin",
			AdminPassword:          adminPassword,
			SessionStore:           config.SessionStoreMySQL,
		}
	}
	return cfg
}

// startServices builds the services, serves them and shuts them down at the
// end of the test. It returns the base URL.
func startServices(t *testing.T, cfg *config.Config, repo models.Repository) (*services, string) {
	t.Helper()
	s, router, err := buildServices(cfg, repo)
	require.NoError(t, err)
	srv := startHTTPServer(cfg, router)
	s.servers = append(s.servers, srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx, s)
	})
	return s, "http://" + srv.Addr
}

// call sends a request and returns the status and the body.
func call(t *testing.T, method, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rd)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

// loginAs logs in through the server and returns the access token.
func loginAs(t *testing.T, base, username, password string) string {
	t.Helper()
	status, body := call(t, http.MethodPost, base+"/api/v1/auth/login",
		`{"username":"`+username+`","password":"`+password+`"}`,
		map[string]string{"X-Requested-With": "XMLHttpRequest"})
	require.Equal(t, http.StatusOK, status, body)
	var resp struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	return resp.Token
}

func bearerOf(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

// TestAuthModesThroughBootstrap is the dual-mode table through the real
// bootstrap (buildServices) and a real HTTP server.
func TestAuthModesThroughBootstrap(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)

	t.Run("auth off", func(t *testing.T) {
		repo, _ := openSQLiteRepo(t)
		_, base := startServices(t, authModeConfig(false), repo)

		for _, h := range []map[string]string{nil, {"Authorization": "Bearer garbage"}, {"X-API-Key": "garbage"}} {
			status, body := call(t, http.MethodGet, base+"/api/v1/items", "", h)
			assert.Equal(t, http.StatusOK, status, "%v: %s", h, body)
			status, _ = call(t, http.MethodPost, base+"/api/v1/items", `{"name":"w","price":1}`, h)
			assert.Equal(t, http.StatusCreated, status, "%v", h)
		}
		for _, p := range []string{"/api/v1/auth/me", "/api/v1/users", "/api/v1/audit-logs", "/api/v1/api-keys"} {
			status, _ := call(t, http.MethodGet, base+p, "", nil)
			assert.Equal(t, http.StatusNotFound, status, p)
		}
		status, _ := call(t, http.MethodPost, base+"/api/v1/auth/login", `{}`, nil)
		assert.Equal(t, http.StatusNotFound, status)
		assertOpenRoutes(t, base, false)
	})

	t.Run("auth on", func(t *testing.T) {
		repo, _ := openSQLiteRepo(t)
		_, base := startServices(t, authModeConfig(true), repo)

		status, _ := call(t, http.MethodGet, base+"/api/v1/items", "", nil)
		assert.Equal(t, http.StatusUnauthorized, status, "no token")
		status, _ = call(t, http.MethodGet, base+"/api/v1/items", "", map[string]string{"Authorization": "Bearer garbage"})
		assert.Equal(t, http.StatusUnauthorized, status, "garbage token")

		// EnsureAdminUser created the admin at start (ADMIN_PASSWORD).
		adminTok := loginAs(t, base, "admin", adminPassword)
		status, body := call(t, http.MethodGet, base+"/api/v1/users", "", bearerOf(adminTok))
		assert.Equal(t, http.StatusOK, status, body)

		// The admin creates a user; the user's token works on Items and gets
		// 403 on the admin routes.
		status, body = call(t, http.MethodPost, base+"/api/v1/users",
			`{"username":"alice","password":"alice-password","role":"user"}`, bearerOf(adminTok))
		require.Equal(t, http.StatusCreated, status, body)
		userTok := loginAs(t, base, "alice", "alice-password")
		status, _ = call(t, http.MethodGet, base+"/api/v1/items", "", bearerOf(userTok))
		assert.Equal(t, http.StatusOK, status)
		for _, p := range []string{"/api/v1/users", "/api/v1/audit-logs", "/api/v1/api-keys?all=true"} {
			status, _ = call(t, http.MethodGet, base+p, "", bearerOf(userTok))
			assert.Equal(t, http.StatusForbidden, status, p)
		}

		// Logout: the token is refused afterwards.
		h := bearerOf(userTok)
		h["X-Requested-With"] = "x"
		status, body = call(t, http.MethodPost, base+"/api/v1/auth/logout", "", h)
		require.Equal(t, http.StatusOK, status, body)
		status, _ = call(t, http.MethodGet, base+"/api/v1/items", "", bearerOf(userTok))
		assert.Equal(t, http.StatusUnauthorized, status)

		assertOpenRoutes(t, base, true)
	})
}

// assertOpenRoutes checks the routes that are open in both modes.
func assertOpenRoutes(t *testing.T, base string, authEnabled bool) {
	t.Helper()
	for _, p := range []string{"/health/live", "/health/ready"} {
		status, body := call(t, http.MethodGet, base+p, "", nil)
		assert.Equal(t, http.StatusOK, status, "%s: %s", p, body)
	}
	status, body := call(t, http.MethodGet, base+"/api/v1/ui-config", "", nil)
	require.Equal(t, http.StatusOK, status)
	var ui struct {
		AuthEnabled bool `json:"auth_enabled"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &ui))
	assert.Equal(t, authEnabled, ui.AuthEnabled)
}

// failingSessionStore fails every lookup (R20).
type failingSessionStore struct{ sessionstore.SessionStore }

func (failingSessionStore) IsTokenBlocked(context.Context, string) (bool, error) {
	return false, errors.New("dial tcp: store down")
}

func (failingSessionStore) IsUserBlocked(context.Context, string, time.Time) (bool, error) {
	return false, errors.New("dial tcp: store down")
}

// TestBootstrapFailsClosedOnStoreError is R20 through the bootstrap: the
// production wiring answers 500 when the session store cannot be read.
func TestBootstrapFailsClosedOnStoreError(t *testing.T) {
	// Not parallel: it replaces newSessionStore and sets the Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)
	prev := newSessionStore
	t.Cleanup(func() { newSessionStore = prev })
	newSessionStore = func(kind string, db *gorm.DB) (sessionstore.SessionStore, error) {
		st, err := prev(kind, db)
		if err != nil {
			return nil, err
		}
		return failingSessionStore{st}, nil
	}

	repo, _ := openSQLiteRepo(t)
	cfg := authModeConfig(true)
	cfg.Auth.AdminPassword = ""
	_, base := startServices(t, cfg, repo)

	tok, err := middleware.GenerateToken("00000000-0000-4000-8000-000000000001", "alice", models.RoleUser, authModesSecret, time.Minute)
	require.NoError(t, err)
	status, body := call(t, http.MethodGet, base+"/api/v1/items", "", bearerOf(tok))
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.JSONEq(t, `{"error":"Internal server error"}`, body)
}

// TestBootstrapEnsureAdminUser: ADMIN_PASSWORD creates the admin once; an
// invalid ADMIN_PASSWORD stops the start; no ADMIN_PASSWORD creates nobody.
func TestBootstrapEnsureAdminUser(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)

	countAdmins := func(t *testing.T, db *gorm.DB) int64 {
		t.Helper()
		var n int64
		require.NoError(t, db.Model(&models.User{}).Where("username = ?", "admin").Count(&n).Error)
		return n
	}

	t.Run("valid password: created once, also by a second replica", func(t *testing.T) {
		repo, db := openSQLiteRepo(t)
		startServices(t, authModeConfig(true), repo)
		startServices(t, authModeConfig(true), repo)
		assert.Equal(t, int64(1), countAdmins(t, db))
		var u models.User
		require.NoError(t, db.Where("username = ?", "admin").First(&u).Error)
		assert.Equal(t, models.RoleAdmin, u.Role)
		assert.NotContains(t, u.PasswordHash, adminPassword)
	})

	t.Run("no password: nobody is created", func(t *testing.T) {
		repo, db := openSQLiteRepo(t)
		cfg := authModeConfig(true)
		cfg.Auth.AdminPassword = ""
		startServices(t, cfg, repo)
		assert.Zero(t, countAdmins(t, db))
	})

	t.Run("invalid password stops the start", func(t *testing.T) {
		repo, db := openSQLiteRepo(t)
		cfg := authModeConfig(true)
		cfg.Auth.AdminPassword = "short"
		s, router, err := buildServices(cfg, repo)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ADMIN_PASSWORD must be at least 8 characters")
		assert.NotContains(t, err.Error(), "short")
		assert.Nil(t, s)
		assert.Nil(t, router)
		assert.Zero(t, countAdmins(t, db))
	})

	t.Run("auth off ignores ADMIN_PASSWORD", func(t *testing.T) {
		repo, db := openSQLiteRepo(t)
		cfg := authModeConfig(false)
		cfg.Auth.AdminPassword = "short"
		startServices(t, cfg, repo)
		assert.Zero(t, countAdmins(t, db))
	})
}

// TestBootstrapRefusesAuthWithoutGormRepository: with auth on the bootstrap
// needs the database of the repository for the auth stores.
func TestBootstrapRefusesAuthWithoutGormRepository(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	s, router, err := buildServices(authModeConfig(true), &stubRepository{})
	require.Error(t, err)
	assert.Nil(t, s)
	assert.Nil(t, router)
}

func TestBootstrapLeaderWorkers(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)

	for _, enabled := range []bool{false, true} {
		repo, _ := openSQLiteRepo(t)
		s, _ := startServices(t, authModeConfig(enabled), repo)
		g, ok := s.leader.(*leader.Group)
		require.True(t, ok)
		names := g.Names()
		if enabled {
			assert.Contains(t, names, "refresh-token-cleanup")
			assert.Contains(t, names, "session-store-cleanup")
			assert.True(t, g.Running(), "LEADER_ELECTION_ENABLED=false: the group runs locally")
		} else {
			assert.NotContains(t, names, "refresh-token-cleanup")
			assert.NotContains(t, names, "session-store-cleanup")
		}
	}
}

// syncBuffer is a bytes.Buffer that is safe for concurrent writes.
type syncBuffer struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPeriodicWorker(t *testing.T) {
	// Not parallel: it replaces the default slog logger.
	logs := &syncBuffer{}
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })

	type ctxKey struct{}
	var mu sync.Mutex
	var ctxs []context.Context
	fail := true
	w := periodicWorker("test-cleanup", 5*time.Millisecond, func(ctx context.Context) error {
		mu.Lock()
		ctxs = append(ctxs, ctx)
		n := len(ctxs)
		first := fail
		fail = false
		mu.Unlock()
		if first {
			return errors.New("delete failed")
		}
		if n >= 3 {
			// A run that the end of the term cancels returns the context
			// error: that is not a failure to log.
			<-ctx.Done()
			return errors.Join(errors.New("delete expired rows"), ctx.Err())
		}
		return nil
	})
	assert.Equal(t, "test-cleanup", w.Name)

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "term"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ctxs) >= 3
	}, 2*time.Second, time.Millisecond, "runs at the start of the term and on each tick")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not stop with its context")
	}

	mu.Lock()
	for i, c := range ctxs {
		assert.Equal(t, "term", c.Value(ctxKey{}), "call %d gets the worker context", i)
	}
	mu.Unlock()
	out := logs.String()
	assert.Contains(t, out, "test-cleanup")
	assert.Contains(t, out, "delete failed")
	assert.Equal(t, 1, strings.Count(out, "level=ERROR"), "only the real failure is logged; the end of the term is not an error")
}

func TestNewSessionStore(t *testing.T) {
	t.Parallel()
	_, db := openSQLiteRepo(t)

	for _, kind := range []string{"", config.SessionStoreMySQL, config.SessionStoreMemory} {
		st, err := newSessionStore(kind, db)
		require.NoError(t, err, kind)
		require.NoError(t, st.BlockToken(context.Background(), "jti-"+kind, time.Now().Add(time.Hour)))
		blocked, err := st.IsTokenBlocked(context.Background(), "jti-"+kind)
		require.NoError(t, err)
		assert.True(t, blocked)
		st.Stop()
	}
	var rows int64
	require.NoError(t, db.Model(&sessionstore.SessionEntry{}).Count(&rows).Error)
	assert.Equal(t, int64(2), rows, "mysql (and the default) write to the database; memory does not")

	_, err := newSessionStore("redis", db)
	assert.Error(t, err)
	_, err = newSessionStore(config.SessionStoreMySQL, nil)
	assert.Error(t, err)
}

// TestShutdownOrderWithAuth: the audit recorder drains right after the HTTP
// servers; the session store stops after everything that reads it and
// before the database.
func TestShutdownOrderWithAuth(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	s.auditRecorder = &fakeAudit{rec: rec}
	s.sessionStore = &fakeStore{rec: rec}

	require.NoError(t, shutdown(context.Background(), s))
	assert.Equal(t, []string{
		"server", "audit recorder", "leader group", "in-flight", "hub", "telemetry", "rate limiters", "session store", "database",
	}, rec.list())

	// A failing audit drain is reported; the later steps still run.
	rec2 := &callRecorder{}
	s2 := fakeServices(rec2)
	s2.auditRecorder = &fakeAudit{rec: rec2, err: context.DeadlineExceeded}
	s2.sessionStore = &fakeStore{rec: rec2}
	err := shutdown(context.Background(), s2)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "audit")
	assert.Contains(t, rec2.list(), "database")
}

type fakeAudit struct {
	rec *callRecorder
	err error
}

func (f *fakeAudit) Shutdown(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.rec.add("audit recorder")
	return f.err
}

type fakeStore struct{ rec *callRecorder }

func (f *fakeStore) Stop() { f.rec.add("session store") }

// mainSubprocessEnv marks the test binary run as the main subprocess.
const mainSubprocessEnv = "BACKEND_MAIN_SUBPROCESS"

// TestMainExitsOnInvalidAuthConfig runs main in a subprocess: with an
// invalid auth configuration the process exits non-zero before it opens the
// database, and the error never contains the secret.
func TestMainExitsOnInvalidAuthConfig(t *testing.T) {
	if os.Getenv(mainSubprocessEnv) == "1" {
		main()
		return
	}
	t.Parallel()

	tests := []struct {
		name    string
		env     []string
		wantErr string
		secret  string
	}{
		{name: "auth on without JWT_SECRET", env: []string{"AUTH_ENABLED=true"}, wantErr: "jwt_secret is required"},
		{name: "auth on with a short JWT_SECRET", env: []string{"AUTH_ENABLED=true", "JWT_SECRET=short-secret-15"},
			wantErr: "jwt_secret must be at least 16 characters", secret: "short-secret-15"},
		{name: "AUTH_ENABLED typo", env: []string{"AUTH_ENABLED=ture", "JWT_SECRET=0123456789abcdef0123"}, wantErr: "AUTH_ENABLED must be a boolean"},
		{name: "unknown SESSION_STORE", env: []string{"AUTH_ENABLED=true", "JWT_SECRET=0123456789abcdef0123", "SESSION_STORE=redis"}, wantErr: "SESSION_STORE must be mysql or memory"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// #nosec G204 -- the test binary itself, with a fixed test selector.
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainExitsOnInvalidAuthConfig$")
			cmd.Env = append([]string{
				mainSubprocessEnv + "=1",
				"ENV_FILE=/nonexistent/.env",
				"PATH=" + os.Getenv("PATH"),
				// A database that does not exist: a run that got past the
				// config would fail on it, not hang.
				"DB_HOST=127.0.0.1", "DB_PORT=1",
			}, tt.env...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			cmd.Stdout = &stderr
			err := cmd.Run()

			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the process must exit non-zero: %s", stderr.String())
			assert.NotZero(t, exitErr.ExitCode())
			out := stderr.String()
			assert.Contains(t, out, "Failed to load configuration")
			assert.Contains(t, out, tt.wantErr)
			assert.NotContains(t, out, "Failed to initialize repository", "it stops before the database")
			if tt.secret != "" {
				assert.NotContains(t, out, tt.secret)
			}
		})
	}
}

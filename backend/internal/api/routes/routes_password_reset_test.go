package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/inflight"
	"backend/internal/mail"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	resetRequestPath = "/api/v1/auth/password-reset"
	resetConfirmPath = "/api/v1/auth/password-reset/confirm"
	resetNewPassword = "a-brand-new-password"
)

// resetOn turns the flow on in cfg (auth is already on in authOn).
func resetOn(cfg *config.Config) *config.Config {
	cfg.Mail = config.MailConfig{Enabled: true, Host: "smtp.example.com", Port: 587, TLS: config.SMTPTLSStartTLS, From: "noreply@example.com", Timeout: time.Second}
	cfg.PasswordReset = config.PasswordResetConfig{Enabled: true, URL: "https://app.example.com", TokenTTL: time.Hour, MinInterval: time.Minute}

	return cfg
}

// resetEnv is the auth-on router with the module on and a fake mailer.
type resetEnv struct {
	*authEnv
	mailer  *mail.FakeMailer
	tracker *inflight.Tracker
}

func newResetEnv(t *testing.T, cfg *config.Config) *resetEnv {
	t.Helper()

	if cfg == nil {
		cfg = resetOn(authOn())
	}

	re := &resetEnv{mailer: &mail.FakeMailer{}, tracker: inflight.New()}
	re.authEnv = newAuthEnv(t, cfg, func(env *authEnv, _ *AuthDeps) {
		env.reset = &PasswordResetDeps{
			Tokens:   database.NewGORMPasswordResetTokenRepository(env.db),
			Mailer:   re.mailer,
			InFlight: re.tracker,
		}
	})

	return re
}

// mailedToken requests a mail for username and returns the raw token of it.
func (e *resetEnv) mailedToken(t *testing.T, username string) string {
	t.Helper()

	before := len(e.mailer.Messages())
	w := e.do(t, http.MethodPost, resetRequestPath, `{"username":"`+username+`"}`, nil)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Eventually(t, func() bool { return len(e.mailer.Messages()) > before }, 5*time.Second, 5*time.Millisecond)

	msgs := e.mailer.Messages()
	body := msgs[len(msgs)-1].Body
	i := strings.Index(body, "#token=")
	require.Positive(t, i, body)

	return strings.Fields(body[i+len("#token="):])[0]
}

func TestPasswordResetOffRegistersNothing(t *testing.T) {
	t.Parallel()

	// Auth on, the flow off: the routes answer 404 and no handler exists.
	off := newAuthEnv(t, nil)
	on := newResetEnv(t, nil)

	for _, path := range []string{resetRequestPath, resetConfirmPath} {
		w := off.do(t, http.MethodPost, path, `{"username":"alice"}`, nil)
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}

	offTable, onTable := routeTable(off.router), routeTable(on.router)
	assert.NotContains(t, strings.Join(offTable, "\n"), "password-reset")

	// The only difference of the tables is the two routes.
	var extra []string

	offSet := map[string]bool{}
	for _, r := range offTable {
		offSet[r] = true
	}

	for _, r := range onTable {
		if !offSet[r] {
			extra = append(extra, r)
		}
	}

	assert.ElementsMatch(t, []string{"POST " + resetRequestPath, "POST " + resetConfirmPath}, extra)
	assert.Len(t, onTable, len(offTable)+2)
}

func TestPasswordResetOffAuthOffRegistersNothing(t *testing.T) {
	t.Parallel()

	router, _ := newTestRouter(t, testConfig(100, 10, nil))
	for _, path := range []string{resetRequestPath, resetConfirmPath} {
		w := doFrom(router, testPeer, http.MethodPost, path, `{}`, nil)
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
}

// The mailer alone builds no routes.
func TestMailEnabledAloneRegistersNoRoutes(t *testing.T) {
	t.Parallel()

	cfg := authOn()
	cfg.Mail = config.MailConfig{Enabled: true}
	env := newAuthEnv(t, cfg)

	assert.NotContains(t, strings.Join(routeTable(env.router), "\n"), "password-reset")
}

func TestSetupRefusesPasswordResetWithoutItsDependencies(t *testing.T) {
	t.Parallel()

	var nilTokens *database.GORMPasswordResetTokenRepository

	tests := []struct {
		deps   *PasswordResetDeps
		mutate func(*config.Config)
		name   string
	}{
		{name: "no dependencies"},
		{name: "no token repository", deps: &PasswordResetDeps{Mailer: &mail.FakeMailer{}, InFlight: inflight.New()}},
		{name: "a typed nil token repository", deps: &PasswordResetDeps{Tokens: nilTokens, Mailer: &mail.FakeMailer{}, InFlight: inflight.New()}},
		{name: "no mailer", deps: &PasswordResetDeps{Tokens: &database.GORMPasswordResetTokenRepository{}, InFlight: inflight.New()}},
		{name: "no in-flight tracker", deps: &PasswordResetDeps{Tokens: &database.GORMPasswordResetTokenRepository{}, Mailer: &mail.FakeMailer{}}},
		{
			name: "zero token ttl", deps: &PasswordResetDeps{Tokens: &database.GORMPasswordResetTokenRepository{}, Mailer: &mail.FakeMailer{}, InFlight: inflight.New()},
			mutate: func(c *config.Config) { c.PasswordReset.TokenTTL = 0 },
		},
		{
			name: "no base url", deps: &PasswordResetDeps{Tokens: &database.GORMPasswordResetTokenRepository{}, Mailer: &mail.FakeMailer{}, InFlight: inflight.New()},
			mutate: func(c *config.Config) { c.PasswordReset.URL = "" },
		},
		{
			name: "auth off", deps: &PasswordResetDeps{Tokens: &database.GORMPasswordResetTokenRepository{}, Mailer: &mail.FakeMailer{}, InFlight: inflight.New()},
			mutate: func(c *config.Config) { c.Auth.Enabled = false },
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := resetOn(authOn())
			if tt.mutate != nil {
				tt.mutate(cfg)
			}

			router := gin.New()
			_, err := Setup(router, Deps{
				Config: cfg, Repository: handlers.NewMockRepository(), HealthChecker: health.New(),
				Auth: &AuthDeps{}, PasswordReset: tt.deps,
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPasswordResetDepsMissing)
			assert.Empty(t, router.Routes(), "nothing is registered")
		})
	}
}

func TestPasswordResetFlowThroughTheRouter(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t, nil)
	alice := e.createUser(t, "alice", models.RoleUser)
	require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", alice.ID).Update("email", "alice@example.com").Error)

	oldToken, cookie := e.login(t, "alice")
	require.NotNil(t, cookie)

	// No X-Requested-With, no credentials: public, like login's non-cookie side.
	raw := e.mailedToken(t, "alice")

	w := e.do(t, http.MethodPost, resetConfirmPath, `{"token":"`+raw+`","new_password":"`+resetNewPassword+`"}`, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Empty(t, w.Result().Cookies(), "no cookie is set")
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), "the global middleware still applies")

	// The old access token and the old session are dead; the new password works.
	assert.Equal(t, http.StatusUnauthorized, e.do(t, http.MethodGet, "/api/v1/auth/me", "", bearer(oldToken)).Code)
	w = e.do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"X-Requested-With": "x", "Cookie": cookie.Name + "=" + cookie.Value})
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	w = e.do(t, http.MethodPost, "/api/v1/auth/login", `{"username":"alice","password":"`+resetNewPassword+`"}`,
		map[string]string{"X-Requested-With": "XMLHttpRequest"})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The audit entry is written through the recorder.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.res.AuditRecorder.Shutdown(ctx))

	var resets []models.AuditLog

	for _, a := range e.auditEntries(t) {
		if a.Action == middleware.AuditActionPasswordReset {
			resets = append(resets, a)
		}
	}

	require.Len(t, resets, 1)
	assert.Equal(t, alice.ID, resets[0].UserID)
	assert.Equal(t, alice.ID, resets[0].EntityID)
	assert.Equal(t, middleware.AuditEntityUser, resets[0].EntityType)
	assert.NotContains(t, resets[0].Details, raw)
}

// Each route has its own per-IP budget, apart from login and from each other.
func TestPasswordResetRoutesHaveTheirOwnRateLimits(t *testing.T) {
	t.Parallel()

	cfg := resetOn(authOn())
	cfg.Server.LoginRateLimit = 2
	e := newResetEnv(t, cfg)

	codes := func(path, body string) []int {
		var out []int
		for range 3 {
			out = append(out, e.do(t, http.MethodPost, path, body, nil).Code)
		}

		return out
	}

	assert.Equal(t, []int{http.StatusAccepted, http.StatusAccepted, http.StatusTooManyRequests}, codes(resetRequestPath, `{"username":"nobody"}`))
	assert.Equal(t, []int{http.StatusBadRequest, http.StatusBadRequest, http.StatusTooManyRequests}, codes(resetConfirmPath, `{"token":"x","new_password":"`+resetNewPassword+`"}`),
		"the confirm budget is not used up by the request route")

	// Login keeps its own budget.
	w := e.do(t, http.MethodPost, "/api/v1/auth/login", `{"username":"nobody","password":"x"}`, map[string]string{"X-Requested-With": "x"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// The idempotency middleware never stores a response of these routes.
func TestPasswordResetRoutesIgnoreTheIdempotencyKey(t *testing.T) {
	t.Parallel()

	cfg := idempotencyOn(resetOn(authOn()))
	re := &resetEnv{mailer: &mail.FakeMailer{}, tracker: inflight.New()}
	re.authEnv = newAuthEnv(t, cfg, func(env *authEnv, _ *AuthDeps) {
		require.NoError(t, env.db.AutoMigrate(&models.IdempotencyKey{}))
		env.idem = database.NewGORMIdempotencyRepository(env.db)
		env.reset = &PasswordResetDeps{
			Tokens:   database.NewGORMPasswordResetTokenRepository(env.db),
			Mailer:   re.mailer,
			InFlight: re.tracker,
		}
	})
	e := re
	alice := e.createUser(t, "alice", models.RoleUser)
	require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", alice.ID).Update("email", "alice@example.com").Error)

	hdr := map[string]string{middleware.IdempotencyKeyHeader: "same-key"}
	raw := e.mailedToken(t, "alice")
	body := `{"token":"` + raw + `","new_password":"` + resetNewPassword + `"}`

	first := e.do(t, http.MethodPost, resetConfirmPath, body, hdr)
	second := e.do(t, http.MethodPost, resetConfirmPath, body, hdr)
	assert.Equal(t, http.StatusNoContent, first.Code)
	assert.Equal(t, http.StatusBadRequest, second.Code, "a repeat is not replayed: the token is used")
	assert.Empty(t, second.Header().Get(middleware.IdempotencyReplayedHeader))

	for range 2 {
		w := e.do(t, http.MethodPost, resetRequestPath, `{"username":"alice"}`, hdr)
		assert.Equal(t, http.StatusAccepted, w.Code)
		assert.Empty(t, w.Header().Get(middleware.IdempotencyReplayedHeader))
	}

	var n int64
	require.NoError(t, e.db.Model(&models.IdempotencyKey{}).Count(&n).Error)
	assert.Zero(t, n, "nothing was stored")
}

func TestPasswordResetOnEveryRouteIsAuthenticatedOrListed(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t, nil)

	public := map[string]bool{
		"GET /health": true, "GET /health/live": true, "GET /health/ready": true,
		"GET /api/v1/ui-config":          true,
		"POST /api/v1/auth/login":        true,
		"POST /api/v1/auth/register":     true,
		"POST /api/v1/auth/refresh":      true,
		"POST /api/v1/auth/logout":       true,
		"GET /api/v1/auth/oidc/login":    true,
		"GET /api/v1/auth/oidc/callback": true,
		"GET /ws":                        true,
		"POST " + resetRequestPath:       true,
		"POST " + resetConfirmPath:       true,
	}

	for _, r := range e.router.Routes() {
		if public[r.Method+" "+r.Path] {
			continue
		}

		path := strings.ReplaceAll(r.Path, ":id", "00000000-0000-4000-8000-000000000001")
		w := e.do(t, r.Method, path, `{}`, map[string]string{"X-Requested-With": "x"})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s %s", r.Method, r.Path)
	}
}

func TestPasswordResetOnAuditRoutesAreKnown(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t, nil)

	for _, r := range e.router.Routes() {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.Path, "/api/v1/auth/password-reset") {
			continue
		}

		ar := middleware.AuditRouteFor(r.Method, r.Path)
		if r.Path == resetRequestPath {
			assert.True(t, ar.Skip, "the request changes nothing and is not audited")
			continue
		}

		assert.False(t, ar.Skip)
		assert.True(t, middleware.IsKnownAuditAction(ar.Action), ar.Action)
		assert.True(t, middleware.IsKnownAuditEntityType(ar.EntityType), ar.EntityType)
	}
}

func TestUIConfigPasswordResetField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cfg    func() *config.Config
		oidc   bool
		wantOn bool
	}{
		{name: "off", cfg: func() *config.Config { return authOn() }},
		{name: "on", cfg: func() *config.Config { return resetOn(authOn()) }, wantOn: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newResetEnv(t, tt.cfg())
			w := e.do(t, http.MethodGet, "/api/v1/ui-config", "", nil)
			require.Equal(t, http.StatusOK, w.Code)

			var got map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			assert.Equal(t, tt.wantOn, got["password_reset_enabled"])
		})
	}
}

package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"backend/internal/api/handlers"
	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const jobsPath = "/api/v1/jobs"

// stubJobRepo serves a fixed page. A nil embedded interface fails the test
// loudly if the handler calls anything else.
type stubJobRepo struct {
	models.JobRepository
	jobs []models.Job
}

func (s *stubJobRepo) ListPaged(context.Context, models.JobFilter, int, int) ([]models.Job, int64, error) {
	return s.jobs, int64(len(s.jobs)), nil
}

func newStubJobRepo() *stubJobRepo {
	now := time.Now().UTC()
	return &stubJobRepo{jobs: []models.Job{{
		ID: 1, Type: "email.send", Status: models.JobStatusDead, MaxAttempts: 5, LastError: "boom",
		Payload: "secret-payload", RunAt: now, CreatedAt: now, UpdatedAt: now,
	}}}
}

// jobsOn turns the module on in cfg.
func jobsOn(cfg *config.Config) *config.Config {
	cfg.Jobs = config.JobsConfig{Enabled: true, Workers: 4, MaxAttempts: 5}
	return cfg
}

// jobsRouter builds an auth-off router with the module switched as asked.
func jobsRouter(t *testing.T, on bool) *gin.Engine {
	t.Helper()
	cfg := testConfig(1000, 10, nil)
	deps := Deps{Config: cfg, Repository: handlers.NewMockRepository()}
	if on {
		jobsOn(cfg)
		deps.Jobs = newStubJobRepo()
	}
	hc := health.New()
	hc.SetReady(true)
	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	deps.HealthChecker, deps.Hub = hc, hub

	router := gin.New()
	res, err := Setup(router, deps)
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)
	return router
}

func hasRoute(router *gin.Engine, method, path string) bool {
	for _, r := range router.Routes() {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

func TestJobsRouteAuthOff(t *testing.T) {
	t.Parallel()

	t.Run("module off: no route, 404", func(t *testing.T) {
		t.Parallel()
		router := jobsRouter(t, false)
		assert.False(t, hasRoute(router, http.MethodGet, jobsPath))
		assert.Equal(t, http.StatusNotFound, doFrom(router, testPeer, http.MethodGet, jobsPath, "", nil).Code)
		// Everything else is unchanged.
		assert.Equal(t, http.StatusOK, doFrom(router, testPeer, http.MethodGet, "/api/v1/items", "", nil).Code)
	})

	t.Run("module on: open like every other route, no payload", func(t *testing.T) {
		t.Parallel()
		router := jobsRouter(t, true)
		w := doFrom(router, testPeer, http.MethodGet, jobsPath+"?status=dead", "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "secret-payload")

		var body handlers.JobListResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Len(t, body.Data, 1)
		assert.Equal(t, "boom", body.Data[0].LastError)

		assert.Equal(t, http.StatusBadRequest, doFrom(router, testPeer, http.MethodGet, jobsPath+"?status=x", "", nil).Code)
		// Read only.
		assert.Equal(t, http.StatusNotFound, doFrom(router, testPeer, http.MethodPost, jobsPath, `{}`, nil).Code)
	})
}

func TestJobsRouteAuthOn(t *testing.T) {
	t.Parallel()

	t.Run("module off: 404 for an admin", func(t *testing.T) {
		t.Parallel()
		env := newAuthEnv(t, nil)
		admin := env.createUser(t, "root", models.RoleAdmin)
		assert.False(t, hasRoute(env.router, http.MethodGet, jobsPath))
		assert.Equal(t, http.StatusNotFound, env.do(t, http.MethodGet, jobsPath, "", bearer(env.token(t, admin))).Code)
	})

	t.Run("module on: admin only, no audit entry", func(t *testing.T) {
		t.Parallel()
		env := newAuthEnv(t, jobsOn(authOn()), func(e *authEnv, _ *AuthDeps) { e.jobs = newStubJobRepo() })
		user := env.createUser(t, "alice", models.RoleUser)
		devops := env.createUser(t, "dave", models.RoleDevOps)
		admin := env.createUser(t, "root", models.RoleAdmin)
		adminKey, _ := env.apiKey(t, admin)
		userKey, _ := env.apiKey(t, user)

		tests := []struct {
			name    string
			headers map[string]string
			want    int
		}{
			{"no credentials", nil, http.StatusUnauthorized},
			{"garbage bearer", map[string]string{"Authorization": "Bearer garbage"}, http.StatusUnauthorized},
			{"user", bearer(env.token(t, user)), http.StatusForbidden},
			{"devops", bearer(env.token(t, devops)), http.StatusForbidden},
			{"user API key", map[string]string{"X-API-Key": userKey}, http.StatusForbidden},
			{"admin", bearer(env.token(t, admin)), http.StatusOK},
			{"admin API key", map[string]string{"X-API-Key": adminKey}, http.StatusOK},
		}
		for _, tt := range tests {
			w := env.do(t, http.MethodGet, jobsPath, "", tt.headers)
			assert.Equal(t, tt.want, w.Code, "%s: %s", tt.name, w.Body.String())
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, env.res.AuditRecorder.Shutdown(ctx))
		assert.Empty(t, env.auditEntries(t), "a read is not audited")
	})

	t.Run("every route of the module-on router still answers 401 without credentials", func(t *testing.T) {
		t.Parallel()
		env := newAuthEnv(t, jobsOn(authOn()), func(e *authEnv, _ *AuthDeps) { e.jobs = newStubJobRepo() })
		require.True(t, hasRoute(env.router, http.MethodGet, jobsPath))
		for _, r := range env.router.Routes() {
			if !strings.HasPrefix(r.Path, "/api/v1/jobs") {
				continue
			}
			assert.Equal(t, http.StatusUnauthorized, env.do(t, r.Method, r.Path, `{}`, nil).Code)
		}
	})
}

func TestSetupRefusesJobsWithoutRepository(t *testing.T) {
	t.Parallel()
	cfg := jobsOn(testConfig(1000, 10, nil))

	for name, repo := range map[string]models.JobRepository{
		"nil":       nil,
		"typed nil": (*stubJobRepo)(nil),
	} {
		router := gin.New()
		res, err := Setup(router, Deps{Config: cfg, Repository: handlers.NewMockRepository(), Jobs: repo})
		require.ErrorIs(t, err, ErrJobsDepsMissing, name)
		assert.Nil(t, res, name)
		assert.Empty(t, router.Routes(), "%s: nothing is registered", name)
	}
}

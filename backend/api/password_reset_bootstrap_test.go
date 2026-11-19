package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/leader"
	"backend/internal/mail"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransport is a mailer with a Ping that the test controls.
type fakeTransport struct {
	mail.FakeMailer
	pingErr atomic.Value // error
	pings   atomic.Int32
}

func (f *fakeTransport) Ping(context.Context) error {
	f.pings.Add(1)

	if err, _ := f.pingErr.Load().(error); err != nil {
		return err
	}

	return nil
}

// useFakeMailer replaces newMailer for the test.
func useFakeMailer(t *testing.T) *fakeTransport {
	t.Helper()

	ft := &fakeTransport{}
	old := newMailer
	newMailer = func(config.MailConfig) (mailTransport, error) { return ft, nil }
	t.Cleanup(func() { newMailer = old })

	return ft
}

func mailConfig(auth, mailOn, resetOn bool) *config.Config {
	cfg := authModeConfig(auth)
	cfg.Mail = config.MailConfig{Enabled: mailOn, Host: "smtp.example.com", Port: 587, TLS: config.SMTPTLSStartTLS, From: "noreply@example.com", Timeout: time.Second}
	cfg.PasswordReset = config.PasswordResetConfig{Enabled: resetOn, URL: "https://app.example.com", TokenTTL: time.Hour, MinInterval: time.Minute}

	return cfg
}

// With both modules off nothing is built: no mailer, no check, no worker, no
// routes, no query on the token table (the test database has no such table).
func TestBootstrapPasswordResetOff(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)

	built := false
	old := newMailer
	newMailer = func(config.MailConfig) (mailTransport, error) { built = true; return &fakeTransport{}, nil }

	t.Cleanup(func() { newMailer = old })

	repo, db := openSQLiteRepo(t)
	require.False(t, db.Migrator().HasTable("password_reset_tokens"))

	s, url := startServices(t, mailConfig(true, false, false), repo)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.NotContains(t, g.Names(), "password-reset-cleanup")
	assert.False(t, built)

	code, body := call(t, http.MethodPost, url+"/api/v1/auth/password-reset", `{"username":"admin"}`, nil)
	assert.Equal(t, http.StatusNotFound, code, body)

	code, body = call(t, http.MethodGet, url+"/health/ready", "", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.NotContains(t, body, "mail")
}

// MAIL_ENABLED alone: the mailer and its check, no routes, no worker.
func TestBootstrapMailOnly(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)

	ft := useFakeMailer(t)
	repo, _ := openSQLiteRepo(t)

	s, url := startServices(t, mailConfig(true, true, false), repo)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.NotContains(t, g.Names(), "password-reset-cleanup")

	code, _ := call(t, http.MethodPost, url+"/api/v1/auth/password-reset", `{"username":"admin"}`, nil)
	assert.Equal(t, http.StatusNotFound, code)

	code, body := call(t, http.MethodGet, url+"/health/ready", "", nil)
	assert.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, "mail")
	assert.Positive(t, ft.pings.Load())

	// An unreachable SMTP server does not take the replica out of service: the
	// check reports DEGRADED, readiness stays UP (200), and the public answer
	// names no host or error. The result is cached for mail.HealthCacheTTL, so
	// build a fresh service.
	ft2 := useFakeMailer(t)
	ft2.pingErr.Store(errors.New("dial tcp 10.9.9.9:587: i/o timeout"))
	repo2, _ := openSQLiteRepo(t)
	_, url2 := startServices(t, mailConfig(true, true, false), repo2)

	code, body = call(t, http.MethodGet, url2+"/health/ready", "", nil)
	assert.Equal(t, http.StatusOK, code, body)
	assert.NotContains(t, body, "10.9.9.9")

	var ready health.HealthStatus
	require.NoError(t, json.Unmarshal([]byte(body), &ready))
	assert.Equal(t, "UP", ready.Status)
	assert.Equal(t, health.StatusDegraded, ready.Checks["mail"].Status)
	assert.Equal(t, mail.ErrUnavailable.Error(), ready.Checks["mail"].Message)
	assert.Equal(t, "UP", ready.Checks["database"].Status)
}

func TestBootstrapPasswordResetOn(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)

	ft := useFakeMailer(t)
	repo, db := openSQLiteRepo(t)
	require.NoError(t, db.AutoMigrate(&models.PasswordResetToken{}))
	require.NoError(t, db.Create(&models.User{ID: "u-1", Username: "alice", Email: "alice@example.com", PasswordHash: "x", Role: models.RoleUser}).Error)

	s, url := startServices(t, mailConfig(true, true, true), repo)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.Contains(t, g.Names(), "password-reset-cleanup")
	assert.True(t, g.Running())

	code, body := call(t, http.MethodPost, url+"/api/v1/auth/password-reset", `{"username":"alice"}`, nil)
	require.Equal(t, http.StatusAccepted, code, body)
	require.Eventually(t, func() bool { return len(ft.Messages()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "alice@example.com", ft.Messages()[0].To)
	assert.Contains(t, ft.Messages()[0].Body, "https://app.example.com/reset-password#token=")

	code, body = call(t, http.MethodGet, url+"/api/v1/ui-config", "", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"password_reset_enabled":true`)

	// The tracker is the one that shutdown waits for.
	assert.NotNil(t, s.inFlight)
}

func TestBootstrapMailerFailureStopsTheStart(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)

	old := newMailer
	newMailer = func(config.MailConfig) (mailTransport, error) { return nil, errors.New("bad smtp settings") }

	t.Cleanup(func() { newMailer = old })

	repo, _ := openSQLiteRepo(t)
	_, _, err := buildServices(mailConfig(true, true, true), repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad smtp settings")
}

// The module needs the database of the repository.
func TestBootstrapPasswordResetNeedsTheDatabase(t *testing.T) {
	t.Parallel()

	_, err := passwordResetTokens(mailConfig(true, true, true), nil)
	require.Error(t, err)

	repo, err := passwordResetTokens(mailConfig(true, true, false), nil)
	require.NoError(t, err)
	assert.Nil(t, repo)
}

type staleRepo struct {
	models.PasswordResetTokenRepository
	calls   int
	results []int64
	err     error
	limits  []int
	cutoffs []time.Duration
}

func (s *staleRepo) DeleteStale(_ context.Context, retention time.Duration, limit int) (int64, error) {
	s.limits = append(s.limits, limit)
	s.cutoffs = append(s.cutoffs, retention)

	if s.err != nil {
		return 0, s.err
	}

	n := s.results[min(s.calls, len(s.results)-1)]
	s.calls++

	return n, nil
}

func TestPasswordResetCleanup(t *testing.T) {
	t.Parallel()

	t.Run("loops until a short batch", func(t *testing.T) {
		t.Parallel()

		r := &staleRepo{results: []int64{passwordResetCleanupBatch, passwordResetCleanupBatch, 3}}
		require.NoError(t, passwordResetCleanup(r)(context.Background()))
		assert.Equal(t, 3, r.calls)
		assert.Equal(t, []time.Duration{passwordResetUsedRetention, passwordResetUsedRetention, passwordResetUsedRetention}, r.cutoffs)
		assert.Equal(t, []int{passwordResetCleanupBatch, passwordResetCleanupBatch, passwordResetCleanupBatch}, r.limits)
	})

	t.Run("a backlog is bounded per run", func(t *testing.T) {
		t.Parallel()

		r := &staleRepo{results: []int64{passwordResetCleanupBatch}}
		require.NoError(t, passwordResetCleanup(r)(context.Background()))
		assert.Equal(t, passwordResetCleanupMaxBatches, r.calls)
	})

	t.Run("a cancelled context returns at once", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		r := &staleRepo{results: []int64{0}}
		require.ErrorIs(t, passwordResetCleanup(r)(ctx), context.Canceled)
		assert.Zero(t, len(r.limits))
	})

	t.Run("an error is returned", func(t *testing.T) {
		t.Parallel()

		r := &staleRepo{err: errors.New("db down")}
		require.Error(t, passwordResetCleanup(r)(context.Background()))
	})
}

func TestDefaultMailerBuildsTheSMTPMailer(t *testing.T) {
	t.Parallel()

	m, err := defaultMailer(config.MailConfig{Enabled: true, Host: "smtp.example.com", Port: 587, TLS: config.SMTPTLSStartTLS, From: "noreply@example.com", Timeout: time.Second})
	require.NoError(t, err)
	assert.True(t, strings.Contains(mailAddress(m), "smtp.example.com"))
}

func mailAddress(m mailTransport) string {
	if a, ok := m.(interface{ Address() string }); ok {
		return a.Address()
	}

	return ""
}

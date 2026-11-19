package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/inflight"
	"backend/internal/mail"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
)

const (
	resetRequestPath = "/api/v1/auth/password-reset"
	resetConfirmPath = "/api/v1/auth/password-reset/confirm"
	resetNewPassword = "Brand-New-Pass-7"
	// resetUser is the fixture user that every flow test resets (local, enabled,
	// has an email, no service account).
	resetUser = "user-u"
)

// resetEnv is the password reset over the admin test environment: real
// repositories on SQLite, the session store, the revoker, the audit recorder,
// the token table and a fake mailer.
type resetEnv struct {
	*adminEnv
	tokens  *database.GORMPasswordResetTokenRepository
	mailer  *mail.FakeMailer
	tracker *inflight.Tracker
	reset   *PasswordResetHandler
	deps    PasswordResetDeps
	resetLg *syncBuffer
}

type resetOpt struct {
	adminOpt
	tokens func(models.PasswordResetTokenRepository) models.PasswordResetTokenRepository
}

func newResetEnv(t *testing.T, opts ...resetOpt) *resetEnv {
	t.Helper()

	var opt resetOpt
	if len(opts) > 0 {
		opt = opts[0]
	}

	e := newAdminEnv(t, opt.adminOpt)
	require.NoError(t, e.db.AutoMigrate(&models.PasswordResetToken{}))

	re := &resetEnv{adminEnv: e, mailer: &mail.FakeMailer{}, tracker: inflight.New(), resetLg: &syncBuffer{}}
	re.tokens = database.NewGORMPasswordResetTokenRepository(e.db)

	var tokens models.PasswordResetTokenRepository = re.tokens
	if opt.tokens != nil {
		tokens = opt.tokens(tokens)
	}

	re.deps = PasswordResetDeps{
		Users:    e.auth.userRepo,
		Tokens:   tokens,
		Mailer:   re.mailer,
		InFlight: re.tracker,
		Auth:     e.auth,
		Access:   e.handler,
		AppTitle: "Test App",
		Config: config.PasswordResetConfig{
			Enabled:     true,
			URL:         "https://app.example.com/ui",
			TokenTTL:    time.Hour,
			MinInterval: time.Minute,
		},
		Logger: slog.New(slog.NewJSONHandler(re.resetLg, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	re.reset = NewPasswordResetHandler(re.deps)

	e.router.POST(resetRequestPath, re.reset.Request)
	e.router.POST(resetConfirmPath, e.recorder.Middleware(), re.reset.Confirm)

	return re
}

// request asks for a reset mail for username and waits until the background
// work of the request has finished.
func (e *resetEnv) request(t *testing.T, username string) *httptest.ResponseRecorder {
	t.Helper()

	w := e.requestNoWait(username)
	e.waitIdle(t)

	return w
}

func (e *resetEnv) requestNoWait(username string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": username})

	return e.do(http.MethodPost, resetRequestPath, string(body), nil)
}

func (e *resetEnv) waitIdle(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return len(e.reset.sem) == 0 }, 5*time.Second, 2*time.Millisecond, "background work did not finish")
}

func (e *resetEnv) confirm(token, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"token": token, "new_password": password})

	return e.do(http.MethodPost, resetConfirmPath, string(body), nil)
}

// rawTokenFromMail extracts the raw token from the link of the last mail.
func rawTokenFromMail(t *testing.T, msg mail.Message) string {
	t.Helper()

	for _, line := range strings.Split(msg.Body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			u, err := url.Parse(line)
			require.NoError(t, err)
			require.Equal(t, "/ui/reset-password", u.Path)
			require.Empty(t, u.RawQuery, "the token is not in the query")

			frag, err := url.ParseQuery(u.Fragment)
			require.NoError(t, err)
			require.NotEmpty(t, frag.Get("token"))

			return frag.Get("token")
		}
	}

	require.Fail(t, "no link in the mail", msg.Body)

	return ""
}

// issueToken requests a mail for resetUser and returns the raw token.
func (e *resetEnv) issueToken(t *testing.T, username string) string {
	t.Helper()

	before := len(e.mailer.Messages())
	w := e.request(t, username)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	msgs := e.mailer.Messages()
	require.Len(t, msgs, before+1)

	return rawTokenFromMail(t, msgs[len(msgs)-1])
}

func (e *resetEnv) tokenRows(t *testing.T, userID string) []models.PasswordResetToken {
	t.Helper()

	var rows []struct {
		TokenHash string
		Used      bool
	}
	require.NoError(t, e.db.Model(&models.PasswordResetToken{}).Select("token_hash, used_at IS NOT NULL AS used").Where("user_id = ?", userID).Scan(&rows).Error)

	out := make([]models.PasswordResetToken, 0, len(rows))
	for _, r := range rows {
		row := models.PasswordResetToken{TokenHash: r.TokenHash}
		if r.Used {
			now := time.Now()
			row.UsedAt = &now
		}

		out = append(out, row)
	}

	return out
}

func (e *resetEnv) setUser(t *testing.T, id string, col string, v any) {
	t.Helper()
	require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", id).Update(col, v).Error)
}

const resetAcceptedSchema = `{
"type": "object",
"required": ["message"],
"additionalProperties": false,
"properties": {"message": {"type": "string", "minLength": 1}}
}`

// The heart of the module: the answer to a request does not depend on whether
// the account exists, is eligible, is throttled or got a mail.
func TestPasswordResetRequestAnswersTheSameForEveryone(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	// A user with no email, a disabled user, an SSO user and the service account.
	e.setUser(t, idUserV, "email", "")
	e.setUser(t, idDevops, "disabled", true)
	e.setUser(t, idAdminB, "service_account", true)

	type outcome struct {
		code    int
		body    string
		headers http.Header
	}

	do := func(username string) outcome {
		w := e.request(t, username)

		h := w.Header().Clone()
		h.Del("X-Request-Id") // differs per request by design

		return outcome{w.Code, w.Body.String(), h}
	}

	// The first request for user-u is eligible; the second is throttled.
	eligible := do(resetUser)
	throttled := do(resetUser)
	cases := map[string]outcome{
		"eligible":        eligible,
		"throttled":       throttled,
		"unknown":         do("nobody-here"),
		"unknown case":    do("No-Such-User"),
		"no email":        do("user-v"),
		"disabled":        do("devops"),
		"sso":             do("oidc-o"),
		"service account": do("admin-b"),
	}

	// A mail failure answers the same too.
	e.mailer.SetError(errors.New("smtp down"))
	e.setUser(t, idAdminA, "email", "admin-a@example.com")
	cases["mail failure"] = do("admin-a")
	e.mailer.SetError(nil)

	require.Equal(t, http.StatusAccepted, eligible.code)

	schema := gojsonschema.NewStringLoader(resetAcceptedSchema)

	for name, got := range cases {
		assert.Equal(t, eligible.code, got.code, name)
		assert.Equal(t, eligible.body, got.body, name)
		assert.Equal(t, eligible.headers, got.headers, "%s: same headers", name)

		res, err := gojsonschema.Validate(schema, gojsonschema.NewStringLoader(got.body))
		require.NoError(t, err, name)
		assert.True(t, res.Valid(), "%s: %v", name, res.Errors())
	}

	assert.Equal(t, "no-store", eligible.headers.Get("Cache-Control"))

	msgs := e.mailer.Messages()
	require.Len(t, msgs, 1, "only the eligible, unthrottled request got a mail")
	assert.Equal(t, "user-u@example.com", msgs[0].To)
}

// Nothing of the lookup, the throttle, the insert or the send runs on the
// request path: the answer is complete while each of them is held.
func TestPasswordResetRequestDoesNoWorkBeforeTheAnswer(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	var lookups, issues atomic.Int32

	e := newResetEnv(t, resetOpt{
		adminOpt: adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &gatedUsers{UserRepository: r, gate: gate, calls: &lookups}
		}},
		tokens: func(r models.PasswordResetTokenRepository) models.PasswordResetTokenRepository {
			return &countingTokens{PasswordResetTokenRepository: r, issues: &issues}
		},
	})
	e.mailer.SetDelay(200 * time.Millisecond)

	for _, name := range []string{resetUser, "nobody-here"} {
		start := time.Now()
		w := e.requestNoWait(name)
		elapsed := time.Since(start)

		assert.Equal(t, http.StatusAccepted, w.Code)
		// A generous bound only: the gate (the lookup is held until the end of
		// the test) and the counters below prove that nothing ran before the
		// answer; a wall-clock bound that is tight would flake under load.
		assert.Less(t, elapsed, 30*time.Second, "the answer does not wait for the held lookup")
	}

	assert.Zero(t, issues.Load(), "no token was stored before the answer")
	assert.Empty(t, e.mailer.Messages())

	close(gate)
	e.waitIdle(t)

	assert.Equal(t, int32(2), lookups.Load(), "the work ran after the answer")
	assert.Equal(t, int32(1), issues.Load())
	require.Len(t, e.mailer.Messages(), 1)
}

type gatedUsers struct {
	models.UserRepository
	gate  chan struct{}
	calls *atomic.Int32
}

func (g *gatedUsers) FindByUsername(ctx context.Context, name string) (*models.User, error) {
	g.calls.Add(1)

	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return g.UserRepository.FindByUsername(ctx, name)
}

type countingTokens struct {
	models.PasswordResetTokenRepository
	issues *atomic.Int32
}

func (c *countingTokens) Issue(ctx context.Context, userID, hash string, ttl, minInterval time.Duration) (bool, error) {
	c.issues.Add(1)

	return c.PasswordResetTokenRepository.Issue(ctx, userID, hash, ttl, minInterval)
}

func TestPasswordResetMailContent(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	raw := e.issueToken(t, resetUser)
	msg := e.mailer.Messages()[0]

	assert.Equal(t, "user-u@example.com", msg.To)
	assert.NotEmpty(t, msg.Subject)
	assert.NotContains(t, msg.Subject, "\n")
	assert.Contains(t, msg.Body, "user-u", "the username")
	assert.Contains(t, msg.Body, "1 hour", "the validity")
	assert.Contains(t, strings.ToLower(msg.Body), "ignore", "the line that it can be ignored")
	assert.Contains(t, msg.Body, "https://app.example.com/ui/reset-password#token="+raw, "the token is in the fragment")
	assert.NotContains(t, msg.Body, "?token", "never in the query")
	assert.NotContains(t, msg.Body, testPasswordHash(), "no hash")
	assert.NotContains(t, msg.Body, testPassword, "no password")
	assert.Len(t, raw, 43, "256 random bits, URL-safe")
	assert.Regexp(t, `^[A-Za-z0-9_-]{43}$`, raw)

	// Only the SHA-256 of the token is stored.
	rows := e.tokenRows(t, idUserU)
	require.Len(t, rows, 1)
	assert.Equal(t, sha256Hex(raw), rows[0].TokenHash)
	var n int64
	require.NoError(t, e.db.Model(&models.PasswordResetToken{}).Where("token_hash = ? OR id = ? OR user_id = ?", raw, raw, raw).Count(&n).Error)
	assert.Zero(t, n, "the raw token is stored nowhere")

	// Not in the log either.
	assert.NotContains(t, e.resetLg.String(), raw)
	assert.NotContains(t, e.logs.String(), raw)
}

func TestPasswordResetValidityText(t *testing.T) {
	t.Parallel()

	tests := map[time.Duration]string{
		5 * time.Minute:  "5 minutes",
		time.Hour:        "1 hour",
		90 * time.Minute: "1 hour 30 minutes",
		2 * time.Hour:    "2 hours",
		24 * time.Hour:   "24 hours",
	}

	for d, want := range tests {
		assert.Equal(t, want, validityText(d), d.String())
	}
}

func TestPasswordResetRequestMinIntervalIsPerUser(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	e.request(t, resetUser)
	e.request(t, resetUser)
	assert.Len(t, e.mailer.Messages(), 1, "one mail per user in the interval")

	e.request(t, "user-v")
	assert.Len(t, e.mailer.Messages(), 2, "another user is not limited")
}

func TestPasswordResetRequestReplacesEarlierTokens(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	first := e.issueToken(t, resetUser)

	// The interval has passed (the repository reads the database clock).
	require.NoError(t, e.db.Exec("UPDATE password_reset_tokens SET created_at = '2000-01-01 00:00:00.000'").Error)
	second := e.issueToken(t, resetUser)
	require.NotEqual(t, first, second)

	assert.Equal(t, http.StatusBadRequest, e.confirm(first, resetNewPassword).Code, "the earlier token is dead")
	assert.Equal(t, http.StatusNoContent, e.confirm(second, resetNewPassword).Code)
}

func TestPasswordResetRequestValidation(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	tests := []struct {
		name, body string
		want       int
	}{
		{"missing username", `{}`, http.StatusBadRequest},
		{"empty username", `{"username":""}`, http.StatusBadRequest},
		{"blank username", `{"username":"   "}`, http.StatusBadRequest},
		{"username too long", `{"username":"` + strings.Repeat("a", models.MaxUsernameLen+1) + `"}`, http.StatusBadRequest},
		{"username is not a string", `{"username":5}`, http.StatusBadRequest},
		{"unknown field", `{"username":"user-u","email":"x@example.com"}`, http.StatusBadRequest},
		{"not json", `username=user-u`, http.StatusBadRequest},
		{"trailing data", `{"username":"user-u"}{"username":"x"}`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		{"body over the limit", `{"username":"` + strings.Repeat("a", 2<<20) + `"}`, http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := e.do(http.MethodPost, resetRequestPath, tt.body, nil)
			assert.Equal(t, tt.want, w.Code, w.Body.String())
		})
	}

	e.waitIdle(t)
	assert.Empty(t, e.mailer.Messages())
}

func TestPasswordResetRequestShutdownAndBackpressure(t *testing.T) {
	t.Parallel()

	t.Run("after shutdown began the answer is the same and nothing runs", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		require.NoError(t, e.tracker.Wait(context.Background()))

		w := e.requestNoWait(resetUser)
		assert.Equal(t, http.StatusAccepted, w.Code)
		assert.Len(t, e.reset.sem, 0, "the slot is given back")
		assert.Empty(t, e.mailer.Messages())
	})

	t.Run("a full queue drops the work and answers the same", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.mailer.SetDelay(300 * time.Millisecond)

		for range cap(e.reset.sem) {
			e.reset.sem <- struct{}{}
		}

		want := e.requestNoWait("nobody-here")
		got := e.requestNoWait(resetUser)
		assert.Equal(t, http.StatusAccepted, got.Code)
		assert.Equal(t, want.Body.String(), got.Body.String())
		assert.Equal(t, headersWithoutRequestID(want), headersWithoutRequestID(got))

		for range cap(e.reset.sem) {
			<-e.reset.sem
		}

		assert.Empty(t, e.mailer.Messages())
		assert.Empty(t, e.tokenRows(t, idUserU))
	})
}

func TestPasswordResetFailuresAreLoggedOnceAndNeverChangeTheAnswer(t *testing.T) {
	t.Parallel()

	t.Run("mail failure", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.mailer.SetError(errors.New("smtp: 451 try later"))

		var codes []int
		for _, name := range []string{resetUser, "user-v", "admin-a"} {
			codes = append(codes, e.request(t, name).Code)
		}

		assert.Equal(t, []int{202, 202, 202}, codes)
		logs := e.resetLg.String()
		assert.Equal(t, 1, strings.Count(logs, `"level":"ERROR"`), "one Error line per interval: %s", logs)
		assert.NotContains(t, logs, "user-u@example.com", "no address in the log")
	})

	t.Run("store failure", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t, resetOpt{tokens: func(r models.PasswordResetTokenRepository) models.PasswordResetTokenRepository {
			return &faultyResetTokens{PasswordResetTokenRepository: r, issueErr: errors.New("dial tcp: database down")}
		}})

		assert.Equal(t, http.StatusAccepted, e.request(t, resetUser).Code)
		assert.Empty(t, e.mailer.Messages())
		assert.Contains(t, e.resetLg.String(), `"level":"ERROR"`)
	})

	t.Run("lookup failure", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t, resetOpt{adminOpt: adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &failingFindUsers{UserRepository: r}
		}}})

		assert.Equal(t, http.StatusAccepted, e.request(t, resetUser).Code)
		assert.Empty(t, e.mailer.Messages())
	})
}

type faultyResetTokens struct {
	models.PasswordResetTokenRepository
	issueErr, consumeErr, deleteErr error
}

func (f *faultyResetTokens) Issue(ctx context.Context, userID, hash string, ttl, minInterval time.Duration) (bool, error) {
	if f.issueErr != nil {
		return false, f.issueErr
	}

	return f.PasswordResetTokenRepository.Issue(ctx, userID, hash, ttl, minInterval)
}

func (f *faultyResetTokens) Consume(ctx context.Context, hash string) (string, bool, error) {
	if f.consumeErr != nil {
		return "", false, f.consumeErr
	}

	return f.PasswordResetTokenRepository.Consume(ctx, hash)
}

func (f *faultyResetTokens) DeleteUnusedForUser(ctx context.Context, userID string) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}

	return f.PasswordResetTokenRepository.DeleteUnusedForUser(ctx, userID)
}

type failingFindUsers struct{ models.UserRepository }

func (f *failingFindUsers) FindByUsername(context.Context, string) (*models.User, error) {
	return nil, errors.New("dial tcp: database down")
}

// Who gets a mail: local, enabled, with an email, not a service account.
func TestPasswordResetEligibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		username string
		prepare  func(t *testing.T, e *resetEnv)
		wantMail bool
	}{
		{name: "local user", username: resetUser, wantMail: true},
		{name: "admin", username: "admin-a", wantMail: true},
		{name: "unknown user", username: "nobody"},
		{name: "disabled user", username: resetUser, prepare: func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "disabled", true) }},
		{name: "service account", username: resetUser, prepare: func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "service_account", true) }},
		{name: "no email", username: resetUser, prepare: func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "email", "") }},
		{name: "blank email", username: resetUser, prepare: func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "email", "  ") }},
		{name: "email that is not an address", username: resetUser, prepare: func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "email", "a@b.c\nBcc: x@example.com") }},
		{name: "sso user", username: "oidc-o"},
		{name: "oidc provider on a local looking user", username: resetUser, prepare: func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "auth_provider", models.AuthProviderOIDC) }},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newResetEnv(t)
			if tt.prepare != nil {
				tt.prepare(t, e)
			}

			w := e.request(t, tt.username)
			assert.Equal(t, http.StatusAccepted, w.Code)

			if tt.wantMail {
				assert.Len(t, e.mailer.Messages(), 1)
				return
			}

			assert.Empty(t, e.mailer.Messages())

			var n int64
			require.NoError(t, e.db.Model(&models.PasswordResetToken{}).Count(&n).Error)
			assert.Zero(t, n, "no token is stored for an account that gets no mail")
		})
	}
}

func TestPasswordResetConfirmSuccessEndsEveryAccess(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	// The user has a session, an access token, an API key and a socket.
	oldToken, _ := e.login(t, resetUser)
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/whoami", "", bearerOf(oldToken)).Code)
	rawKey, keyID := e.newKey(t, idUserU)
	require.EqualValues(t, 1, e.activeRefreshTokens(t, idUserU))

	// The account is locked by failed logins.
	for range loginFailureThreshold {
		w := e.rawLogin(resetUser, "wrong-password")
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}
	w := e.rawLogin(resetUser, "wrong-password")
	require.Equal(t, http.StatusTooManyRequests, w.Code, "the name is locked now")

	raw := e.issueToken(t, resetUser)

	w = e.confirm(raw, resetNewPassword)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Empty(t, w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Empty(t, w.Result().Cookies(), "no cookie is set or read")

	// The old access token is dead (user block), the refresh tokens are revoked.
	assert.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/api/v1/whoami", "", bearerOf(oldToken)).Code)
	assert.Zero(t, e.activeRefreshTokens(t, idUserU))

	// The sockets were closed.
	users, _ := e.revoker.snapshot()
	assert.Contains(t, users, idUserU)

	// The API key survives (a documented rule of every password path).
	assert.EqualValues(t, 1, e.keyCount(t, idUserU))
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/whoami", "", keyHeader(rawKey)).Code)
	_ = keyID

	// The new password works at once, also though the account was locked; the
	// old one does not.
	w = e.rawLogin(resetUser, resetNewPassword)
	assert.Equal(t, http.StatusOK, w.Code, "the lock was cleared: %s", w.Body.String())
	w = e.rawLogin(resetUser, testPassword)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// A token that was issued after the reset (a new login) is not blocked.
	newToken := e.loginToken(t, resetUser, resetNewPassword)
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/whoami", "", bearerOf(newToken)).Code)

	// The token is single use.
	w = e.confirm(raw, "Another-Pass-1234")
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// One audit entry, with the reset account as actor and no secret.
	rows := e.auditRows(t)
	var resets []models.AuditLog
	for _, r := range rows {
		if r.Action == middleware.AuditActionPasswordReset {
			resets = append(resets, r)
		}
	}
	require.Len(t, resets, 1)
	assert.Equal(t, idUserU, resets[0].UserID)
	assert.Equal(t, idUserU, resets[0].EntityID)
	assert.Equal(t, middleware.AuditEntityUser, resets[0].EntityType)
	assert.Equal(t, resetUser, resets[0].Username)
	assert.NotContains(t, resets[0].Details, raw)
	assert.NotContains(t, resets[0].Details, resetNewPassword)
}

// rawLogin posts a login with the headers of the web UI.
func (e *adminEnv) rawLogin(username, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)

	return w
}

func (e *adminEnv) loginToken(t *testing.T, username, password string) string {
	t.Helper()

	w := e.rawLogin(username, password)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp LoginResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	return resp.Token
}

func headersWithoutRequestID(w *httptest.ResponseRecorder) http.Header {
	h := w.Header().Clone()
	h.Del("X-Request-Id")

	return h
}

// A bad password does not consume the token.
func TestPasswordResetConfirmValidatesThePasswordFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, password, want string
	}{
		{"empty", "", msgPasswordTooShort},
		{"too short", "short", msgPasswordTooShort},
		{"seven characters", "1234567", msgPasswordTooShort},
		{"over 72 bytes", strings.Repeat("a", 73), msgPasswordTooLong},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newResetEnv(t)
			raw := e.issueToken(t, resetUser)

			w := e.confirm(raw, tt.password)
			require.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, tt.want, errorOf(t, w))

			rows := e.tokenRows(t, idUserU)
			require.Len(t, rows, 1)
			assert.Nil(t, rows[0].UsedAt, "the token is not consumed")

			// The same token still works with a good password.
			assert.Equal(t, http.StatusNoContent, e.confirm(raw, resetNewPassword).Code)
		})
	}
}

func TestPasswordResetConfirmAcceptsTheLimits(t *testing.T) {
	t.Parallel()

	for _, pw := range []string{"12345678", strings.Repeat("a", 72), "åäö-åäöx"} {
		pw := pw
		t.Run(pw, func(t *testing.T) {
			t.Parallel()
			e := newResetEnv(t)
			raw := e.issueToken(t, resetUser)

			assert.Equal(t, http.StatusNoContent, e.confirm(raw, pw).Code)
			assert.Equal(t, http.StatusOK, e.rawLogin(resetUser, pw).Code)
		})
	}
}

// Every bad token gets the same answer.
func TestPasswordResetConfirmBadTokensAnswerTheSame(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	valid := e.issueToken(t, resetUser)
	require.Equal(t, http.StatusNoContent, e.confirm(valid, resetNewPassword).Code)

	require.NoError(t, e.db.Exec("UPDATE password_reset_tokens SET created_at = '2000-01-01 00:00:00.000'").Error)
	expired := e.issueToken(t, "user-v")
	require.NoError(t, e.db.Exec("UPDATE password_reset_tokens SET expires_at = '2000-01-01 00:00:00.000' WHERE user_id = ?", idUserV).Error)

	tests := map[string]string{
		"used":                   valid,
		"expired":                expired,
		"unknown":                strings.Repeat("A", 43),
		"empty":                  "",
		"too short":              "abc",
		"too long":               strings.Repeat("A", 44),
		"not url safe":           strings.Repeat("+", 43),
		"with a space":           strings.Repeat("A", 42) + " ",
		"the hash of a real one": sha256Hex(valid),
	}

	var want string

	for name, token := range tests {
		w := e.confirm(token, resetNewPassword)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
		assert.Equal(t, "Invalid or expired token", errorOf(t, w), name)

		if want == "" {
			want = w.Body.String()
		}

		assert.Equal(t, want, w.Body.String(), name)
	}
}

func TestPasswordResetConfirmChecksTheUserAgain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(t *testing.T, e *resetEnv)
	}{
		{"disabled after the mail", func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "disabled", true) }},
		{"became a service account", func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "service_account", true) }},
		{"lost the email", func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "email", "") }},
		{"linked to sso", func(t *testing.T, e *resetEnv) { e.setUser(t, idUserU, "auth_provider", models.AuthProviderOIDC) }},
		{"deleted", func(t *testing.T, e *resetEnv) { require.NoError(t, e.users.Delete(context.Background(), idUserU)) }},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newResetEnv(t)
			raw := e.issueToken(t, resetUser)
			before := e.userHash(t, idUserU)
			tt.prepare(t, e)

			w := e.confirm(raw, resetNewPassword)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, "Invalid or expired token", errorOf(t, w))

			if e.userExists(idUserU) {
				assert.Equal(t, before, e.userHash(t, idUserU), "the password is unchanged")
			}
		})
	}
}

func (e *adminEnv) userHash(t *testing.T, id string) string {
	t.Helper()

	var hash string
	require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", id).Pluck("password_hash", &hash).Error)

	return hash
}

func TestPasswordResetConfirmRequestValidation(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)
	raw := e.issueToken(t, resetUser)

	tests := []struct {
		name, body string
		want       int
	}{
		{"missing fields", `{}`, http.StatusBadRequest},
		{"missing token", `{"new_password":"` + resetNewPassword + `"}`, http.StatusBadRequest},
		{"unknown field", `{"token":"` + raw + `","new_password":"` + resetNewPassword + `","username":"admin-a"}`, http.StatusBadRequest},
		{"not json", `token=x`, http.StatusBadRequest},
		{"token is not a string", `{"token":5,"new_password":"` + resetNewPassword + `"}`, http.StatusBadRequest},
		{"body over the limit", `{"token":"` + strings.Repeat("a", 2<<20) + `"}`, http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := e.do(http.MethodPost, resetConfirmPath, tt.body, nil)
			assert.Equal(t, tt.want, w.Code, w.Body.String())
		})
	}

	rows := e.tokenRows(t, idUserU)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].UsedAt, "no refused request consumed the token")
}

func TestPasswordResetConfirmInternalErrors(t *testing.T) {
	t.Parallel()

	t.Run("token store failure", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t, resetOpt{tokens: func(r models.PasswordResetTokenRepository) models.PasswordResetTokenRepository {
			return &faultyResetTokens{PasswordResetTokenRepository: r, consumeErr: errors.New("dial tcp 10.0.0.5:3306: connection refused")}
		}})

		w := e.confirm(strings.Repeat("A", 43), resetNewPassword)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "10.0.0.5")
	})

	t.Run("user lookup failure", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		raw := e.issueToken(t, resetUser)
		e.deps.Users = &failingByIDUsers{UserRepository: e.deps.Users}
		e.reset = NewPasswordResetHandler(e.deps)
		e.router.POST("/x/confirm", e.reset.Confirm)

		body, _ := json.Marshal(map[string]string{"token": raw, "new_password": resetNewPassword})
		w := e.do(http.MethodPost, "/x/confirm", string(body), nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
	})

	t.Run("the password cannot be stored", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		raw := e.issueToken(t, resetUser)
		e.deps.Users = &failingPasswordUsers{UserRepository: e.deps.Users}
		e.reset = NewPasswordResetHandler(e.deps)
		e.router.POST("/x/confirm", e.reset.Confirm)

		body, _ := json.Marshal(map[string]string{"token": raw, "new_password": resetNewPassword})
		w := e.do(http.MethodPost, "/x/confirm", string(body), nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "disk")
	})

	t.Run("a revocation step fails: 500, the change stands and is audited", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t, resetOpt{adminOpt: adminOpt{store: func(s sessionstore.SessionStore) sessionstore.SessionStore {
			return failingBlockStore{SessionStore: s}
		}}})
		raw := e.issueToken(t, resetUser)

		w := e.confirm(raw, resetNewPassword)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))

		assert.Equal(t, http.StatusOK, e.rawLogin(resetUser, resetNewPassword).Code, "the new password stands")

		rows := e.auditRows(t)
		var found bool
		for _, r := range rows {
			if r.Action == middleware.AuditActionPasswordReset {
				found = true

				assert.Contains(t, r.Details, "revocation_failed")
			}
		}

		assert.True(t, found, "the committed change is recorded although the answer is an error")
	})
}

type failingBlockStore struct{ sessionstore.SessionStore }

func (failingBlockStore) BlockUser(context.Context, string, time.Time) error {
	return errors.New("session store down")
}

type failingByIDUsers struct{ models.UserRepository }

func (f *failingByIDUsers) FindByID(context.Context, string) (*models.User, error) {
	return nil, errors.New("dial tcp: database down")
}

type failingPasswordUsers struct{ models.UserRepository }

func (f *failingPasswordUsers) UpdatePassword(context.Context, string, string) error {
	return errors.New("disk full")
}

// Two confirms with one token at the same time: one wins.
func TestPasswordResetConfirmIsSingleUse(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)
	raw := e.issueToken(t, resetUser)

	var wg sync.WaitGroup

	var ok, bad atomic.Int32

	for range 6 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			switch e.confirm(raw, resetNewPassword).Code {
			case http.StatusNoContent:
				ok.Add(1)
			case http.StatusBadRequest:
				bad.Add(1)
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, int32(1), ok.Load())
	assert.Equal(t, int32(5), bad.Load())
}

// A password change (own or by an admin) makes the unused tokens useless.
func TestPasswordChangesDropUnusedResetTokens(t *testing.T) {
	t.Parallel()

	t.Run("admin sets a password", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(e.tokens)
		raw := e.issueToken(t, resetUser)

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"password":"Admin-Set-Pass-1"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		assert.Empty(t, e.tokenRows(t, idUserU))
		assert.Equal(t, http.StatusBadRequest, e.confirm(raw, resetNewPassword).Code)
	})

	t.Run("the user changes the own password", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.auth.SetPasswordResetTokens(e.tokens)
		raw := e.issueToken(t, resetUser)

		body := `{"current_password":"` + testPassword + `","new_password":"Own-Change-Pass-1"}`
		w := e.do(http.MethodPut, "/api/v1/auth/me/password", body, e.bearer(t, idUserU))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		assert.Empty(t, e.tokenRows(t, idUserU))
		assert.Equal(t, http.StatusBadRequest, e.confirm(raw, resetNewPassword).Code)
	})

	// A token mailed to the old address must not work for the new mailbox
	// owner, and a disabled account must not get a token back by being enabled.
	t.Run("admin changes the email", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(e.tokens)
		raw := e.issueToken(t, resetUser)

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"email":"new-owner@example.com"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		assert.Empty(t, e.tokenRows(t, idUserU))
		assert.Equal(t, http.StatusBadRequest, e.confirm(raw, resetNewPassword).Code)
	})

	t.Run("admin clears the email", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(e.tokens)
		e.issueToken(t, resetUser)

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"email":""}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Empty(t, e.tokenRows(t, idUserU))
	})

	t.Run("an unchanged email or another field keeps the token", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(e.tokens)
		e.issueToken(t, resetUser)

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"email":"user-u@example.com"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Len(t, e.tokenRows(t, idUserU), 1, "the same address")

		w = e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"display_name":"Somebody Else"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Len(t, e.tokenRows(t, idUserU), 1, "the display name")
	})

	t.Run("another user's tokens are kept", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(e.tokens)
		e.issueToken(t, resetUser)
		e.issueToken(t, "user-v")

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"email":"new-owner@example.com"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Empty(t, e.tokenRows(t, idUserU))
		assert.Len(t, e.tokenRows(t, idUserV), 1)
	})

	t.Run("disable drops the tokens, so an enable cannot revive them", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(e.tokens)
		raw := e.issueToken(t, resetUser)

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":true}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Empty(t, e.tokenRows(t, idUserU))

		w = e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":false}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, http.StatusBadRequest, e.confirm(raw, resetNewPassword).Code, "the old link stays dead")
	})

	t.Run("enable alone keeps the tokens", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(e.tokens)
		e.issueToken(t, resetUser)

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":false}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Len(t, e.tokenRows(t, idUserU), 1)
	})

	t.Run("a failing cleanup does not fail an email change or a disable", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(&faultyResetTokens{PasswordResetTokenRepository: e.tokens, deleteErr: errors.New("db down")})

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"email":"new-owner@example.com"}`)
		assert.Equal(t, http.StatusOK, w.Code)

		w = e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":true}`)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("a failing cleanup does not fail the change", func(t *testing.T) {
		t.Parallel()
		e := newResetEnv(t)
		e.handler.SetPasswordResetTokens(&faultyResetTokens{PasswordResetTokenRepository: e.tokens, deleteErr: errors.New("db down")})

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"password":"Admin-Set-Pass-1"}`)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// The token and the password never reach a log line.
func TestPasswordResetNeverLogsSecrets(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)
	e.mailer.SetError(errors.New("smtp: 550 mailbox unavailable"))
	e.request(t, resetUser)
	e.mailer.SetError(nil)
	require.NoError(t, e.db.Exec("UPDATE password_reset_tokens SET created_at = '2000-01-01 00:00:00.000'").Error)
	raw := e.issueToken(t, resetUser)

	require.Equal(t, http.StatusNoContent, e.confirm(raw, resetNewPassword).Code)
	e.confirm(raw, "second-try-password")

	for _, logs := range []string{e.resetLg.String(), e.logs.String()} {
		assert.NotContains(t, logs, raw)
		assert.NotContains(t, logs, resetNewPassword)
		assert.NotContains(t, logs, "second-try-password")
		assert.NotContains(t, logs, sha256Hex(raw), "not even the hash")
	}
}

func TestPasswordResetHandlerConstruction(t *testing.T) {
	t.Parallel()

	e := newResetEnv(t)
	assert.Equal(t, passwordResetMaxInFlight, cap(e.reset.sem))

	// Without a logger the default one is used (no panic).
	d := e.deps
	d.Logger = nil
	h := NewPasswordResetHandler(d)
	assert.NotNil(t, h.log())
}

// echoRejectServer is a minimal SMTP server that refuses every recipient with
// an answer that echoes the address, the way many real servers do.
func echoRejectServer(t *testing.T) (host string, port int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer conn.Close()

				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				r := bufio.NewReader(conn)
				write := func(l string) { _, _ = conn.Write([]byte(l + "\r\n")) }

				write("220 fake ESMTP")

				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}

					verb := strings.ToUpper(strings.TrimSpace(line))

					switch {
					case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
						write("250 fake")
					case strings.HasPrefix(verb, "RCPT TO:"):
						write("550 5.1.1 " + strings.TrimSpace(line[len("RCPT TO:"):]) + " no such mailbox")
					case verb == "QUIT":
						write("221 bye")

						return
					case verb == "DATA":
						write("503 no recipients")
					default:
						write("250 ok")
					}
				}
			}()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)

	return "127.0.0.1", addr.Port
}

// A real SMTP refusal that echoes the recipient: go-mail's error text holds the
// address, so nothing may log the error text. The log gets the classification.
func TestPasswordResetSendFailureLogsNoAddress(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)

	host, port := echoRejectServer(t)
	smtp, err := mail.NewSMTP(config.MailConfig{Enabled: true, Host: host, Port: port, TLS: config.SMTPTLSNone, From: "noreply@example.com", Timeout: 3 * time.Second})
	require.NoError(t, err)

	d := e.deps
	d.Mailer = smtp
	h := NewPasswordResetHandler(d)
	e.router.POST("/x/request", h.Request)

	body, _ := json.Marshal(map[string]string{"username": resetUser})
	w := e.do(http.MethodPost, "/x/request", string(body), nil)
	require.Equal(t, http.StatusAccepted, w.Code)
	require.Eventually(t, func() bool { return len(h.sem) == 0 }, 5*time.Second, 5*time.Millisecond)

	// The token is stored (the mail failed after it), so the raw token is
	// nowhere: the test cannot know it, but the hash and the address must not be
	// in the log either.
	logs := e.resetLg.String()
	require.Contains(t, logs, "Failed to send a password reset mail", logs)
	assert.NotContains(t, logs, "user-u@example.com")
	assert.NotContains(t, logs, "example.com")
	assert.NotContains(t, logs, "no such mailbox")
	assert.NotContains(t, logs, "affected recipient")
	assert.Contains(t, logs, `"smtp_code":550`)
	assert.Contains(t, logs, `"stage":"send"`)
	assert.Contains(t, logs, `"user_id":"`+idUserU+`"`)

	rows := e.tokenRows(t, idUserU)
	require.Len(t, rows, 1)
	assert.NotContains(t, logs, rows[0].TokenHash)
	assert.NotContains(t, e.logs.String(), "user-u@example.com")
}

// A Mailer of unknown origin may return any text: the handler never logs it.
func TestPasswordResetSendFailureTextIsNeverLogged(t *testing.T) {
	t.Parallel()
	e := newResetEnv(t)
	e.mailer.SetError(errors.New("550 user-u@example.com refused, token=abc"))

	assert.Equal(t, http.StatusAccepted, e.request(t, resetUser).Code)

	logs := e.resetLg.String()
	require.Contains(t, logs, "Failed to send a password reset mail")
	assert.NotContains(t, logs, "user-u@example.com")
	assert.NotContains(t, logs, "refused")
	assert.Contains(t, logs, `"kind":"other"`)
}

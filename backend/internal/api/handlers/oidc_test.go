package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/auth"
	"backend/internal/auth/authtest"
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	oidcLoginPath    = "/api/v1/auth/oidc/login"
	oidcCallbackPath = "/api/v1/auth/oidc/callback"

	bodyLoginFailed  = `{"error":"Login failed"}`
	bodyProviderDown = `{"error":"Identity provider unavailable"}`
	bodyNotPermitted = `{"error":"Login not permitted"}`
	bodyInternal     = `{"error":"Internal server error"}`
)

// browser is a minimal cookie jar: it keeps the cookies the server sets and
// sends back those whose path matches the request, like a browser does for a
// first-party navigation.
type browser struct {
	cookies map[string]*http.Cookie
}

func newBrowser() *browser { return &browser{cookies: map[string]*http.Cookie{}} }

func (b *browser) store(w *httptest.ResponseRecorder) {
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(b.cookies, ck.Name)
			continue
		}
		b.cookies[ck.Name] = ck
	}
}

func (b *browser) apply(req *http.Request) {
	for _, ck := range b.cookies {
		if strings.HasPrefix(req.URL.Path, ck.Path) {
			req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
		}
	}
}

// oidcUsers wraps the real user repository with hooks.
type oidcUsers struct {
	models.UserRepository
	onFindByID         func(ctx context.Context)
	onFindByExternalID func(ctx context.Context)
	findExternalErr    error
	createErr          error
	updateProfileErr   error
	updates            []models.UserProfileUpdate
	mu                 sync.Mutex
}

func (u *oidcUsers) FindByID(ctx context.Context, id string) (*models.User, error) {
	if u.onFindByID != nil {
		u.onFindByID(ctx)
	}
	return u.UserRepository.FindByID(ctx, id)
}

func (u *oidcUsers) FindByExternalID(ctx context.Context, provider, externalID string) (*models.User, error) {
	if u.findExternalErr != nil {
		return nil, u.findExternalErr
	}
	user, err := u.UserRepository.FindByExternalID(ctx, provider, externalID)
	if err != nil && u.onFindByExternalID != nil {
		u.onFindByExternalID(ctx)
	}
	return user, err
}

func (u *oidcUsers) Create(ctx context.Context, user *models.User) error {
	if u.createErr != nil {
		return u.createErr
	}
	return u.UserRepository.Create(ctx, user)
}

func (u *oidcUsers) UpdateProfile(ctx context.Context, id string, upd models.UserProfileUpdate) error {
	u.mu.Lock()
	u.updates = append(u.updates, upd)
	u.mu.Unlock()
	if u.updateProfileErr != nil {
		return u.updateProfileErr
	}
	return u.UserRepository.UpdateProfile(ctx, id, upd)
}

// failingStore fails chosen session store calls.
type failingStore struct {
	sessionstore.SessionStore
	saveErr    error
	consumeErr error
}

func (f *failingStore) SaveOIDCState(ctx context.Context, state string, data sessionstore.OIDCStateData, ttl time.Duration) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	return f.SessionStore.SaveOIDCState(ctx, state, data, ttl)
}

func (f *failingStore) ConsumeOIDCState(ctx context.Context, state string) (*sessionstore.OIDCStateData, error) {
	if f.consumeErr != nil {
		return nil, f.consumeErr
	}
	return f.SessionStore.ConsumeOIDCState(ctx, state)
}

type oidcHarness struct {
	idp     *authtest.IdP
	auth    *AuthHandler
	h       *OIDCHandler
	db      *gorm.DB
	rawUser *database.GORMUserRepository
	users   *oidcUsers
	tokens  *database.GORMRefreshTokenRepository
	store   sessionstore.SessionStore
	logs    *syncBuffer
	cfg     *config.OIDCConfig
	authCfg *config.AuthConfig
	router  *gin.Engine
	revoker *recordingRevoker
}

type oidcOpt func(*oidcSetup)

type oidcSetup struct {
	oidc       func(c *config.OIDCConfig)
	auth       func(c *config.AuthConfig)
	wrapStore  func(sessionstore.SessionStore) sessionstore.SessionStore
	wrapTokens func(models.RefreshTokenRepository) models.RefreshTokenRepository
	noRefresh  bool
	idpPath    string
}

func openOIDCDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.RefreshToken{}))
	return db
}

func newOIDCHarness(t *testing.T, opts ...oidcOpt) *oidcHarness {
	t.Helper()
	var setup oidcSetup
	for _, o := range opts {
		o(&setup)
	}
	idp := authtest.New(t, setup.idpPath)

	cfg := &config.OIDCConfig{
		Enabled: true, Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		RedirectURL: idp.RedirectURL, Scopes: []string{"openid", "profile", "email"}, RoleClaim: "roles",
		AdminRoles: []string{"platform-admins"}, StateTTL: 5 * time.Minute, FrontendPath: "/auth/callback",
	}
	if setup.oidc != nil {
		setup.oidc(cfg)
	}
	authCfg := testAuthConfig()
	authCfg.RefreshTokenExpiration = 168 * time.Hour
	authCfg.RefreshReuseGrace = 30 * time.Second
	authCfg.SecureCookies = true
	if setup.auth != nil {
		setup.auth(authCfg)
	}

	db := openOIDCDB(t)
	rawUser := database.NewGORMUserRepository(db)
	users := &oidcUsers{UserRepository: rawUser}
	tokens := database.NewGORMRefreshTokenRepository(db)
	store := sessionstore.NewMemory()
	t.Cleanup(store.Stop)
	var st sessionstore.SessionStore = store
	if setup.wrapStore != nil {
		st = setup.wrapStore(st)
	}

	authH, logs, _ := newTestAuthHandler(users, authCfg, cfg)
	if !setup.noRefresh {
		var rt models.RefreshTokenRepository = tokens
		if setup.wrapTokens != nil {
			rt = setup.wrapTokens(rt)
		}
		authH.SetRefreshTokenRepo(rt)
	}
	authH.SetSessionStore(st)
	revoker := &recordingRevoker{}
	authH.SetWebSocketRevoker(revoker)

	provider, err := auth.NewProvider(cfg, auth.Options{AllowInsecureLoopbackForTesting: true})
	require.NoError(t, err)
	h := NewOIDCHandler(provider, authH, users, st, cfg)
	h.blockRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}

	r := gin.New()
	r.GET(oidcLoginPath, h.Login)
	r.GET(oidcCallbackPath, h.Callback)
	r.POST("/api/v1/auth/refresh", authH.Refresh)
	authed := r.Group("/api/v1/auth", middleware.AuthRequiredWithOptions(middleware.JWTAuthOptions{
		JWTSecret: testJWTSecret, SessionStore: st, FailClosedOnStoreError: true,
	}))
	authed.GET("/me", authH.GetCurrentUser)

	return &oidcHarness{
		idp: idp, auth: authH, h: h, db: db, rawUser: rawUser, users: users, tokens: tokens,
		store: st, logs: logs, cfg: cfg, authCfg: authCfg, router: r, revoker: revoker,
	}
}

func (s *oidcHarness) get(b *browser, target string, opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	b.apply(req)
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	b.store(w)
	return w
}

// start opens the login URL in the browser and returns the authorization URL
// of the identity provider.
func (s *oidcHarness) start(t *testing.T, b *browser) string {
	t.Helper()
	w := s.get(b, oidcLoginPath)
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	return w.Header().Get("Location")
}

// finish plays the user at the provider and then the browser's return to the
// callback with the code and state.
func (s *oidcHarness) finish(t *testing.T, b *browser, authURL string, claims map[string]any, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	cb, err := url.Parse(s.idp.Authorize(t, authURL, claims))
	require.NoError(t, err)
	return s.get(b, cb.RequestURI(), opts...)
}

// signIn runs a whole login in a fresh browser.
func (s *oidcHarness) signIn(t *testing.T, claims map[string]any) (*browser, *httptest.ResponseRecorder) {
	t.Helper()
	b := newBrowser()
	authURL := s.start(t, b)
	return b, s.finish(t, b, authURL, claims)
}

func oidcClaims(kv ...any) map[string]any {
	c := map[string]any{
		"sub":                "idp-user-1",
		"email":              "alice@example.com",
		"email_verified":     true,
		"name":               "Alice Example",
		"preferred_username": "alice",
		"roles":              []string{"staff"},
	}
	for i := 0; i+1 < len(kv); i += 2 {
		c[kv[i].(string)] = kv[i+1]
	}
	return c
}

func (s *oidcHarness) userBySub(t *testing.T, sub string) *models.User {
	t.Helper()
	u, err := s.rawUser.FindByExternalID(context.Background(), models.AuthProviderOIDC, auth.ExternalID(s.idp.Issuer, sub))
	require.NoError(t, err)
	return u
}

func (s *oidcHarness) userCount(t *testing.T) int64 {
	t.Helper()
	n, err := s.rawUser.Count(context.Background())
	require.NoError(t, err)
	return n
}

func (s *oidcHarness) tokenRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, s.db.Model(&models.RefreshToken{}).Count(&n).Error)
	return n
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, ck := range w.Result().Cookies() {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

func sha256B64(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func sha256Hexs(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// assertNoSessionOrUser checks that a refused callback created no user, no
// refresh token and no cookie that grants anything.
func assertNoSession(t *testing.T, s *oidcHarness, w *httptest.ResponseRecorder, wantUsers int64) {
	t.Helper()
	assert.Nil(t, cookieNamed(w, hostRefreshCookie), "no refresh cookie")
	assert.Zero(t, s.tokenRows(t), "no refresh token row")
	assert.Equal(t, wantUsers, s.userCount(t))
}

// ---------------------------------------------------------------------------

func TestOIDCDisabledAnswers404(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []oidcOpt
	}{
		{name: "OIDC off", opts: []oidcOpt{func(s *oidcSetup) { s.oidc = func(c *config.OIDCConfig) { c.Enabled = false } }}},
		{name: "auth off", opts: []oidcOpt{func(s *oidcSetup) { s.auth = func(c *config.AuthConfig) { c.Enabled = false } }}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t, tt.opts...)
			for _, path := range []string{oidcLoginPath, oidcCallbackPath + "?state=x&code=y"} {
				w := s.get(newBrowser(), path)
				assert.Equal(t, http.StatusNotFound, w.Code, path)
				assert.JSONEq(t, `{"error":"Not found"}`, w.Body.String())
				assert.Empty(t, w.Result().Cookies())
			}
			assert.Zero(t, s.idp.Hits("discovery"), "a disabled login never contacts the provider")
		})
	}
}

func TestOIDCLoginRedirectsToTheProvider(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()

	w := s.get(b, oidcLoginPath)

	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, s.idp.Server.URL+"/authorize", loc.Scheme+"://"+loc.Host+loc.Path)
	q := loc.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, s.idp.ClientID, q.Get("client_id"))
	assert.Equal(t, s.idp.RedirectURL, q.Get("redirect_uri"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"))
	assert.NotEmpty(t, q.Get("state"))
	assert.NotEmpty(t, q.Get("nonce"))
	assert.NotContains(t, w.Header().Get("Location"), s.idp.ClientSecret)
	assert.Empty(t, w.Body.String(), "no body that could leak anything")
}

// The state is stored on the server under its hash with the verifier, the nonce
// and the hash of the browser binding value; none of it is predictable.
func TestOIDCLoginStoresTheStateAndBindsTheBrowser(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()

	w := s.get(b, oidcLoginPath)

	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	q := loc.Query()
	binding := cookieNamed(w, hostBindingCookie)
	require.NotNil(t, binding, "the login sets the browser binding cookie")
	assert.True(t, binding.HttpOnly)
	assert.True(t, binding.Secure)
	assert.Equal(t, http.SameSiteLaxMode, binding.SameSite, "Lax: the callback is a cross-site top-level navigation from the provider, which a Strict cookie would not travel with")
	assert.Equal(t, "/", binding.Path, "Path=/: required by the __Host- prefix, and a proxy may strip a path prefix")
	assert.Empty(t, binding.Domain, "host-only")
	assert.Equal(t, int((5 * time.Minute).Seconds()), binding.MaxAge, "short lived: as long as the state")
	raw, err := base64.RawURLEncoding.DecodeString(binding.Value)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(raw), 16, "at least 128 bits")

	data, err := s.store.ConsumeOIDCState(context.Background(), q.Get("state"))
	require.NoError(t, err)
	require.NotNil(t, data, "the state is stored server side")
	assert.Equal(t, sha256Hexs(binding.Value), data.BindingHash, "the hash of the binding value is stored, not the value")
	assert.Equal(t, q.Get("nonce"), data.Nonce)
	assert.Equal(t, sha256B64(data.CodeVerifier), q.Get("code_challenge"), "the stored verifier belongs to the challenge that was sent")
	assert.GreaterOrEqual(t, len(data.CodeVerifier), 43)
	assert.NotEqual(t, binding.Value, q.Get("state"))
	assert.NotContains(t, w.Header().Get("Location"), data.CodeVerifier)
	assert.Empty(t, data.RedirectURL, "no redirect target is kept: the callback goes to the fixed path")
}

func TestOIDCLoginValuesAreUniquePerLogin(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		w := s.get(newBrowser(), oidcLoginPath)
		loc, err := url.Parse(w.Header().Get("Location"))
		require.NoError(t, err)
		for _, v := range []string{loc.Query().Get("state"), loc.Query().Get("nonce"), loc.Query().Get("code_challenge"), cookieNamed(w, hostBindingCookie).Value} {
			assert.False(t, seen[v], "no value repeats between logins")
			seen[v] = true
		}
	}
}

func TestOIDCBindingCookieSecureFollowsTheSetting(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t, func(o *oidcSetup) { o.auth = func(c *config.AuthConfig) { c.SecureCookies = false } })

	w := s.get(newBrowser(), oidcLoginPath)

	require.Equal(t, http.StatusFound, w.Code)
	ck := cookieNamed(w, oidcBindingCookieName)
	require.NotNil(t, ck, "without Secure the cookie has no __Host- prefix")
	assert.False(t, ck.Secure)
	assert.Equal(t, "/", ck.Path)
	assert.Nil(t, cookieNamed(w, hostBindingCookie))
}

func TestOIDCLoginIgnoresRequestParameters(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)

	w := s.get(newBrowser(), oidcLoginPath+"?redirect=https://evil.example.com&next=/x&state=attacker&nonce=attacker&code_challenge=attacker")

	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	assert.NotEqual(t, "attacker", loc.Query().Get("state"))
	assert.NotEqual(t, "attacker", loc.Query().Get("nonce"))
	assert.NotEqual(t, "attacker", loc.Query().Get("code_challenge"))
	assert.NotContains(t, w.Header().Get("Location"), "evil.example.com")
}

func TestOIDCLoginFailures(t *testing.T) {
	t.Parallel()
	t.Run("the provider is unavailable", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t)
		s.idp.Configure(func(k *authtest.Knobs) {
			k.DiscoveryHook = func(d map[string]any) { d["issuer"] = "https://evil.example.com/" + authtest.LeakMarker }
		})

		w := s.get(newBrowser(), oidcLoginPath)

		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.JSONEq(t, bodyProviderDown, w.Body.String())
		assert.Empty(t, w.Result().Cookies(), "no binding cookie without a state")
		assert.NotContains(t, s.logs.String(), authtest.LeakMarker, "provider text is not logged")
	})
	t.Run("the state cannot be stored", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t, func(o *oidcSetup) {
			o.wrapStore = func(st sessionstore.SessionStore) sessionstore.SessionStore {
				return &failingStore{SessionStore: st, saveErr: errors.New("db password=hunter2 unreachable")}
			}
		})

		w := s.get(newBrowser(), oidcLoginPath)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, bodyInternal, w.Body.String())
		assert.Empty(t, w.Result().Cookies())
		assert.Contains(t, s.logs.String(), "unreachable", "the cause is logged")
	})
	t.Run("a cancelled request", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		w := s.get(newBrowser(), oidcLoginPath, withContext(ctx))

		assert.Equal(t, statusClientClosedRequest, w.Code)
		assert.Empty(t, w.Result().Cookies())
	})
	t.Run("refresh sessions are not enabled", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t, func(o *oidcSetup) { o.noRefresh = true })

		w := s.get(newBrowser(), oidcLoginPath)

		assert.Equal(t, http.StatusNotImplemented, w.Code)
		assert.Empty(t, w.Result().Cookies())
		assert.Zero(t, s.idp.Hits("discovery"))
	})
}

// ---------------------------------------------------------------------------

func TestOIDCCallbackProvisionsAUserAndStartsASession(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()
	authURL := s.start(t, b)

	w := s.finish(t, b, authURL, oidcClaims())

	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	assert.Equal(t, "/auth/callback", w.Header().Get("Location"), "the fixed frontend path, with nothing in it")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Empty(t, w.Body.String())

	u := s.userBySub(t, "idp-user-1")
	assert.Equal(t, models.AuthProviderOIDC, u.AuthProvider)
	assert.Empty(t, u.PasswordHash, "an OIDC account has no password")
	assert.Equal(t, models.RoleUser, u.Role)
	assert.Equal(t, "alice@example.com", u.Email)
	assert.Equal(t, "alice@example.com", u.Username)
	assert.Equal(t, "Alice Example", u.DisplayName)
	assert.False(t, u.Disabled)
	assert.False(t, u.ServiceAccount)
	require.NotNil(t, u.ExternalID)
	assert.Equal(t, auth.ExternalID(s.idp.Issuer, "idp-user-1"), *u.ExternalID)
	assert.NotContains(t, *u.ExternalID, "idp-user-1", "the stored identity is a digest")

	// The session is the one a password login starts: the refresh cookie.
	ck := cookieNamed(w, hostRefreshCookie)
	require.NotNil(t, ck)
	assertCookieAttributes(t, ck)
	assert.Equal(t, int64(1), s.tokenRows(t))
}

func TestOIDCCallbackClearsTheBindingCookie(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()
	authURL := s.start(t, b)
	require.Contains(t, b.cookies, hostBindingCookie)

	w := s.finish(t, b, authURL, oidcClaims())

	require.Equal(t, http.StatusFound, w.Code)
	cleared := cookieNamed(w, hostBindingCookie)
	require.NotNil(t, cleared)
	assert.Empty(t, cleared.Value)
	assert.Equal(t, -1, cleared.MaxAge)
	assert.Equal(t, "/", cleared.Path)
	assert.NotContains(t, b.cookies, hostBindingCookie)
	assert.True(t, cleared.Secure, "cleared with the attributes it was set with")
	assert.True(t, cleared.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cleared.SameSite)
	assert.Empty(t, cleared.Domain)
}

// Without SECURE_COOKIES the cookies have no __Host- prefix (a browser would
// drop it without Secure): the whole login works under the plain names, and
// the binding cookie is cleared with the attributes it was set with.
func TestOIDCLoginWithPlainCookieNames(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t, func(o *oidcSetup) { o.auth = func(c *config.AuthConfig) { c.SecureCookies = false } })
	b := newBrowser()
	authURL := s.start(t, b)
	set := b.cookies[oidcBindingCookieName]
	require.NotNil(t, set)

	w := s.finish(t, b, authURL, oidcClaims())

	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	refresh := cookieNamed(w, refreshTokenCookieName)
	require.NotNil(t, refresh, "the session cookie has the plain name")
	assert.Equal(t, "/", refresh.Path)
	assert.False(t, refresh.Secure)
	cleared := cookieNamed(w, oidcBindingCookieName)
	require.NotNil(t, cleared)
	assert.Equal(t, -1, cleared.MaxAge)
	assert.Equal(t, set.Path, cleared.Path)
	assert.Equal(t, set.Secure, cleared.Secure)
	assert.Equal(t, set.HttpOnly, cleared.HttpOnly)
	assert.Equal(t, set.SameSite, cleared.SameSite)
	assert.Nil(t, cookieNamed(w, hostBindingCookie))
	assert.Nil(t, cookieNamed(w, hostRefreshCookie))
}

// After the redirect the web UI has no token yet. It asks for one with the
// refresh cookie, with the same-site header that every cookie endpoint needs.
func TestOIDCSessionWorksWithRefreshAndMe(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b, w := s.signIn(t, oidcClaims())
	require.Equal(t, http.StatusFound, w.Code)
	require.Contains(t, b.cookies, hostRefreshCookie)

	refresh := func(opts ...reqOpt) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
		b.apply(req)
		for _, o := range opts {
			o(req)
		}
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		return rec
	}

	assert.Equal(t, http.StatusForbidden, refresh().Code, "the CSRF rule applies: no header, no token")
	rec := refresh(withHeader("X-Requested-With", "XMLHttpRequest"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RefreshResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	claims, err := middleware.ValidateJWT(resp.Token, testJWTSecret)
	require.NoError(t, err)
	u := s.userBySub(t, "idp-user-1")
	assert.Equal(t, u.ID, claims.UserID)
	assert.Equal(t, models.AuthProviderOIDC, claims.AuthProvider)
	assert.NotEmpty(t, claims.SessionID)
	me := bearerGet(t, s.router, "/api/v1/auth/me", resp.Token)
	assert.Equal(t, http.StatusOK, me.Code)
}

func TestOIDCCallbackAdminRoleMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		claims map[string]any
		oidc   func(c *config.OIDCConfig)
		want   string
	}{
		{name: "a configured admin role", claims: oidcClaims("roles", []string{"x", "platform-admins"}), want: models.RoleAdmin},
		{name: "another case is not an admin", claims: oidcClaims("roles", []string{"Platform-Admins"}), want: models.RoleUser},
		{name: "no admin roles configured: nobody is an admin", claims: oidcClaims("roles", []string{"admin", "platform-admins"}), oidc: func(c *config.OIDCConfig) { c.AdminRoles = nil }, want: models.RoleUser},
		{name: "a devops role", claims: oidcClaims("roles", []string{"builders"}), oidc: func(c *config.OIDCConfig) { c.DevOpsRoles = []string{"builders"} }, want: models.RoleDevOps},
		{name: "no roles", claims: oidcClaims("roles", []string{}), want: models.RoleUser},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t, func(o *oidcSetup) { o.oidc = tt.oidc })

			_, w := s.signIn(t, tt.claims)

			require.Equal(t, http.StatusFound, w.Code)
			assert.Equal(t, tt.want, s.userBySub(t, "idp-user-1").Role)
		})
	}
}

func TestOIDCRoleIsSyncedOnEveryLogin(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)

	_, w := s.signIn(t, oidcClaims("roles", []string{"platform-admins"}))
	require.Equal(t, http.StatusFound, w.Code)
	require.Equal(t, models.RoleAdmin, s.userBySub(t, "idp-user-1").Role)

	_, w = s.signIn(t, oidcClaims("roles", []string{"staff"}))
	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, models.RoleUser, s.userBySub(t, "idp-user-1").Role, "a demotion at the provider takes effect at the next login")
	assert.Contains(t, s.logs.String(), "lowers the role of an admin", "an IdP-driven admin demotion is logged as a warning")
	assert.Equal(t, 1, strings.Count(s.logs.String(), "lowers the role of an admin"), "once, not on the promotion or an unchanged login")

	_, w = s.signIn(t, oidcClaims("roles", []string{"platform-admins"}))
	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, models.RoleAdmin, s.userBySub(t, "idp-user-1").Role)
	assert.Equal(t, int64(1), s.userCount(t), "the same identity is the same account")
}

// refreshToken trades the refresh cookie of the browser for an access token.
func (s *oidcHarness) refreshToken(b *browser) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	b.apply(req)
	withHeader("X-Requested-With", "XMLHttpRequest")(req)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	b.store(rec)
	var resp RefreshResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp.Token
}

func (s *oidcHarness) meStatus(t *testing.T, token string) int {
	t.Helper()
	return bearerGet(t, s.router, "/api/v1/auth/me", token).Code
}

// TestOIDCDemotionEndsTheOtherSessionsOfTheUser: tokens carry the role claim,
// so a demotion by the identity provider must end every access token and
// session of the user that was issued before the login, as ChangeUserRole
// does, and close the sockets. The session of the login itself is not caught.
func TestOIDCDemotionEndsTheOtherSessionsOfTheUser(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	old, w := s.signIn(t, oidcClaims("roles", []string{"platform-admins"}))
	require.Equal(t, http.StatusFound, w.Code)
	code, oldToken := s.refreshToken(old)
	require.Equal(t, http.StatusOK, code)
	claims, err := middleware.ValidateJWT(oldToken, testJWTSecret)
	require.NoError(t, err)
	require.Equal(t, models.RoleAdmin, claims.Role)
	require.Equal(t, http.StatusOK, s.meStatus(t, oldToken))

	fresh, w := s.signIn(t, oidcClaims("roles", []string{"staff"}))
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())

	assert.Equal(t, http.StatusUnauthorized, s.meStatus(t, oldToken), "the access token with the admin claim is rejected")
	code, _ = s.refreshToken(old)
	assert.Equal(t, http.StatusUnauthorized, code, "the old session cannot get a new token")
	code, freshToken := s.refreshToken(fresh)
	require.Equal(t, http.StatusOK, code, "the session of this very login is not caught by the block")
	freshClaims, err := middleware.ValidateJWT(freshToken, testJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, models.RoleUser, freshClaims.Role)
	assert.Equal(t, http.StatusOK, s.meStatus(t, freshToken))
	users, _ := s.revoker.snapshot()
	assert.Equal(t, []string{s.userBySub(t, "idp-user-1").ID}, users, "the sockets of the user are closed")
}

// TestOIDCRoleChangesThatDoNotLowerTheRoleKeepTheSessions: only a lowered role
// is a revocation: a promotion, an unchanged role and a first login are not.
func TestOIDCRoleChangesThatDoNotLowerTheRoleKeepTheSessions(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t, func(o *oidcSetup) { o.oidc = func(c *config.OIDCConfig) { c.DevOpsRoles = []string{"builders"} } })
	old, w := s.signIn(t, oidcClaims())
	require.Equal(t, http.StatusFound, w.Code)
	code, oldToken := s.refreshToken(old)
	require.Equal(t, http.StatusOK, code)

	for _, roles := range [][]string{{"staff"}, {"builders"}, {"platform-admins"}} {
		_, w = s.signIn(t, oidcClaims("roles", roles))
		require.Equal(t, http.StatusFound, w.Code)
	}

	assert.Equal(t, http.StatusOK, s.meStatus(t, oldToken), "no block for a first login, an unchanged role or a promotion")
	users, _ := s.revoker.snapshot()
	assert.Empty(t, users)
}

// TestOIDCDevOpsDemotionAlsoEndsTheSessions: any lowered role counts.
func TestOIDCDevOpsDemotionAlsoEndsTheSessions(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t, func(o *oidcSetup) { o.oidc = func(c *config.OIDCConfig) { c.DevOpsRoles = []string{"builders"} } })
	old, w := s.signIn(t, oidcClaims("roles", []string{"builders"}))
	require.Equal(t, http.StatusFound, w.Code)
	code, oldToken := s.refreshToken(old)
	require.Equal(t, http.StatusOK, code)

	_, w = s.signIn(t, oidcClaims("roles", []string{"staff"}))
	require.Equal(t, http.StatusFound, w.Code)

	assert.Equal(t, http.StatusUnauthorized, s.meStatus(t, oldToken))
}

// TestOIDCDemotionBlockIsRetried: the block is written with the same bounded
// retries as an admin role change.
func TestOIDCDemotionBlockIsRetried(t *testing.T) {
	t.Parallel()
	var flaky *flakyBlockStore
	s := newOIDCHarness(t, func(o *oidcSetup) {
		o.wrapStore = func(st sessionstore.SessionStore) sessionstore.SessionStore {
			flaky = &flakyBlockStore{SessionStore: st}
			return flaky
		}
	})
	old, w := s.signIn(t, oidcClaims("roles", []string{"platform-admins"}))
	require.Equal(t, http.StatusFound, w.Code)
	code, oldToken := s.refreshToken(old)
	require.Equal(t, http.StatusOK, code)
	flaky.setPlan(func(call int) bool { return call == 1 })

	_, w = s.signIn(t, oidcClaims("roles", []string{"staff"}))

	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	assert.Equal(t, 2, flaky.callCount(), "one failure, one success")
	assert.Equal(t, http.StatusUnauthorized, s.meStatus(t, oldToken))
	assert.Contains(t, s.logs.String(), "Retrying the user block")
}

// TestOIDCDemotionWithAFailedBlockRefusesTheLogin: the stored role is lowered
// but the old tokens are not ended; no new session starts, the sockets are
// closed anyway and the client sees the generic 500.
func TestOIDCDemotionWithAFailedBlockRefusesTheLogin(t *testing.T) {
	t.Parallel()
	var flaky *flakyBlockStore
	s := newOIDCHarness(t, func(o *oidcSetup) {
		o.wrapStore = func(st sessionstore.SessionStore) sessionstore.SessionStore {
			flaky = &flakyBlockStore{SessionStore: st}
			return flaky
		}
	})
	_, w := s.signIn(t, oidcClaims("roles", []string{"platform-admins"}))
	require.Equal(t, http.StatusFound, w.Code)
	rows := s.tokenRows(t)
	flaky.setPlan(always)

	b, w := s.signIn(t, oidcClaims("roles", []string{"staff"}))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, bodyInternal, w.Body.String())
	assert.Nil(t, cookieNamed(w, hostRefreshCookie))
	assert.NotContains(t, b.cookies, hostRefreshCookie)
	assert.Equal(t, 1+len(s.h.blockRetryDelays), flaky.callCount(), "bounded retries")
	users, _ := s.revoker.snapshot()
	assert.Len(t, users, 1, "the sockets are closed even when the block failed")
	assert.LessOrEqual(t, s.tokenRows(t), rows, "no new session")
}

func TestOIDCProfileIsSyncedAndOnlyChangesAreWritten(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	_, w := s.signIn(t, oidcClaims())
	require.Equal(t, http.StatusFound, w.Code)

	// Same data: no write.
	before := len(s.users.updates)
	_, w = s.signIn(t, oidcClaims())
	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, before, len(s.users.updates), "an unchanged profile is not written")

	// Changed name and email.
	_, w = s.signIn(t, oidcClaims("name", "Alice Renamed", "email", "alice.new@example.com"))
	require.Equal(t, http.StatusFound, w.Code)
	u := s.userBySub(t, "idp-user-1")
	assert.Equal(t, "Alice Renamed", u.DisplayName)
	assert.Equal(t, "alice.new@example.com", u.Email)
	assert.Equal(t, "alice@example.com", u.Username, "the username does not follow the email")
	last := s.users.updates[len(s.users.updates)-1]
	assert.Nil(t, last.LinkProvider, "an existing account is never re-linked")
	assert.Nil(t, last.ExternalID)

	// An unverified email does not replace or clear the stored one.
	_, w = s.signIn(t, oidcClaims("email", "attacker@example.com", "email_verified", false))
	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "alice.new@example.com", s.userBySub(t, "idp-user-1").Email)
}

func TestOIDCSyncFailureRefusesTheLogin(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	_, w := s.signIn(t, oidcClaims("roles", []string{"platform-admins"}))
	require.Equal(t, http.StatusFound, w.Code)
	rows := s.tokenRows(t)
	s.users.updateProfileErr = errors.New("write failed")

	b, w := s.signIn(t, oidcClaims("roles", []string{"staff"}))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, bodyInternal, w.Body.String())
	assert.Nil(t, cookieNamed(w, hostRefreshCookie), "no session with an unsynced (still elevated) role")
	assert.Equal(t, rows, s.tokenRows(t))
	assert.NotContains(t, b.cookies, hostRefreshCookie)
}

func TestOIDCDisabledUserIsRefused(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	_, w := s.signIn(t, oidcClaims())
	require.Equal(t, http.StatusFound, w.Code)
	u := s.userBySub(t, "idp-user-1")
	require.NoError(t, s.db.Model(&models.User{}).Where("id = ?", u.ID).Update("disabled", true).Error)
	rows := s.tokenRows(t)

	_, w = s.signIn(t, oidcClaims("roles", []string{"platform-admins"}))

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, bodyNotPermitted, w.Body.String())
	assert.Nil(t, cookieNamed(w, hostRefreshCookie))
	assert.Equal(t, rows, s.tokenRows(t))
	assert.Equal(t, models.RoleUser, s.userBySub(t, "idp-user-1").Role, "a disabled account is not changed by a login")
}

func TestOIDCUserDisabledWhileTheLoginRuns(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	_, w := s.signIn(t, oidcClaims())
	require.Equal(t, http.StatusFound, w.Code)
	u := s.userBySub(t, "idp-user-1")
	rows := s.tokenRows(t)
	s.users.onFindByID = func(ctx context.Context) {
		_ = s.db.WithContext(ctx).Model(&models.User{}).Where("id = ?", u.ID).Update("disabled", true).Error
	}

	_, w = s.signIn(t, oidcClaims())

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, bodyNotPermitted, w.Body.String())
	assert.Nil(t, cookieNamed(w, hostRefreshCookie))
	assert.Equal(t, rows, s.tokenRows(t))
}

// ---------------------------------------------------------------------------
// Account identity: an OIDC login never takes over an existing account.

func localAdmin(username string) models.User {
	return models.User{
		ID: "local-" + username, Username: username, PasswordHash: testPasswordHash(), DisplayName: "Local " + username,
		Email: "local-" + strings.ReplaceAll(username, "@", "-at-") + "@example.com", Role: models.RoleAdmin, AuthProvider: models.AuthProviderLocal,
	}
}

func TestOIDCLoginNeverLinksToALocalAccount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		local models.User
		// claims of the OIDC login that collides with the local account
		claims map[string]any
	}{
		{name: "username equals the verified email", local: localAdmin("alice@example.com"), claims: oidcClaims()},
		{name: "username equals the preferred username", local: localAdmin("alice"), claims: oidcClaims()},
		{name: "the email field of a local account equals the email", local: func() models.User {
			u := localAdmin("bob")
			u.Email = "alice@example.com"
			return u
		}(), claims: oidcClaims()},
		{name: "username equals the subject", local: localAdmin("idp-user-1"), claims: oidcClaims()},
		{name: "unverified email equals the local username", local: localAdmin("alice@example.com"), claims: oidcClaims("email_verified", false)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t)
			local := tt.local
			require.NoError(t, s.rawUser.Create(context.Background(), &local))

			_, w := s.signIn(t, tt.claims)

			require.Equal(t, http.StatusFound, w.Code, w.Body.String())
			after, err := s.rawUser.FindByID(context.Background(), local.ID)
			require.NoError(t, err)
			assert.Equal(t, models.AuthProviderLocal, after.AuthProvider, "the local account is still local")
			assert.Nil(t, after.ExternalID)
			assert.Equal(t, local.PasswordHash, after.PasswordHash, "its password is untouched")
			assert.Equal(t, models.RoleAdmin, after.Role)
			assert.Equal(t, local.Email, after.Email)
			assert.Equal(t, local.DisplayName, after.DisplayName)

			oidcUser := s.userBySub(t, "idp-user-1")
			assert.NotEqual(t, local.ID, oidcUser.ID, "a separate account")
			assert.Equal(t, models.RoleUser, oidcUser.Role, "with the role of the claims, not of the local account")
			assert.Equal(t, int64(2), s.userCount(t))
			var rt models.RefreshToken
			require.NoError(t, s.db.First(&rt).Error)
			assert.Equal(t, oidcUser.ID, rt.UserID, "the session belongs to the OIDC account")
		})
	}
}

func TestOIDCUnverifiedEmailIsNotUsed(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)

	_, w := s.signIn(t, oidcClaims("email_verified", false))

	require.Equal(t, http.StatusFound, w.Code)
	u := s.userBySub(t, "idp-user-1")
	assert.Empty(t, u.Email, "an unverified address is not stored")
	assert.True(t, strings.HasPrefix(u.Username, "oidc_"), "nor used as the username: %s", u.Username)
	assert.NotContains(t, u.Username, "alice")
}

func TestOIDCSameSubjectAtAnotherIssuerIsAnotherAccount(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	_, w := s.signIn(t, oidcClaims())
	require.Equal(t, http.StatusFound, w.Code)
	first := s.userBySub(t, "idp-user-1")
	// A user of another issuer with the same sub has another external ID.
	other := models.User{Username: "other-issuer-user", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC}
	ext := auth.ExternalID("https://other-issuer.example.com", "idp-user-1")
	other.ExternalID = &ext
	require.NoError(t, s.rawUser.Create(context.Background(), &other))

	_, w = s.signIn(t, oidcClaims())

	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, first.ID, s.userBySub(t, "idp-user-1").ID, "the login resolves to the user of its own issuer")
	assert.Equal(t, int64(2), s.userCount(t))
}

func TestOIDCLongEmailFallsBackToAGeneratedUsername(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	long := strings.Repeat("a", 95) + "@example.com" // 107 characters: over the username column

	_, w := s.signIn(t, oidcClaims("email", long))

	require.Equal(t, http.StatusFound, w.Code)
	u := s.userBySub(t, "idp-user-1")
	assert.Equal(t, long, u.Email)
	assert.True(t, strings.HasPrefix(u.Username, "oidc_"))
}

func TestOIDCGeneratedUsernamesAreUnpredictable(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	names := map[string]bool{}
	for i := 0; i < 5; i++ {
		_, w := s.signIn(t, oidcClaims("sub", "user-"+string(rune('a'+i)), "email_verified", false))
		require.Equal(t, http.StatusFound, w.Code)
		u := s.userBySub(t, "user-"+string(rune('a'+i)))
		assert.False(t, names[u.Username])
		names[u.Username] = true
		assert.NotContains(t, u.Username, auth.ExternalID(s.idp.Issuer, "user-"+string(rune('a'+i)))[:12], "not derived from the digest of the identity")
	}
}

func TestOIDCUsernameClaimedByALocalAccountUsesAnotherName(t *testing.T) {
	t.Parallel()
	// A local user that took a generated-looking name cannot block the login:
	// the generator retries with a new random name.
	s := newOIDCHarness(t)
	local := localAdmin("alice@example.com")
	require.NoError(t, s.rawUser.Create(context.Background(), &local))

	_, w := s.signIn(t, oidcClaims())

	require.Equal(t, http.StatusFound, w.Code)
	assert.True(t, strings.HasPrefix(s.userBySub(t, "idp-user-1").Username, "oidc_"))
}

func TestOIDCNoUsernameAvailable(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	// Every create fails with a duplicate that is not this identity.
	s.users.createErr = dberrors.NewDatabaseError("create", dberrors.ErrDuplicateKey)

	_, w := s.signIn(t, oidcClaims())

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, bodyNotPermitted, w.Body.String())
	assertNoSession(t, s, w, 0)
}

// ---------------------------------------------------------------------------
// Provisioning races.

// Another request creates the same identity between the lookup and the
// create: the duplicate key is mapped, the user is read again, one user exists.
func TestOIDCProvisioningRaceIsResolvedByReReading(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	var once sync.Once
	s.users.onFindByExternalID = func(ctx context.Context) {
		once.Do(func() {
			ext := auth.ExternalID(s.idp.Issuer, "idp-user-1")
			racer := &models.User{Username: "racer@example.com", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC, ExternalID: &ext}
			require.NoError(t, s.rawUser.Create(ctx, racer))
		})
	}

	_, w := s.signIn(t, oidcClaims())

	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	assert.Equal(t, int64(1), s.userCount(t), "one user for the identity")
	u := s.userBySub(t, "idp-user-1")
	var rt models.RefreshToken
	require.NoError(t, s.db.First(&rt).Error)
	assert.Equal(t, u.ID, rt.UserID)
}

func TestOIDCConcurrentFirstLoginsMakeOneUser(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	const n = 10
	type flow struct {
		b   *browser
		cb  string
		rec *httptest.ResponseRecorder
	}
	flows := make([]*flow, n)
	for i := range flows {
		b := newBrowser()
		authURL := s.start(t, b)
		cb, err := url.Parse(s.idp.Authorize(t, authURL, oidcClaims()))
		require.NoError(t, err)
		flows[i] = &flow{b: b, cb: cb.RequestURI()}
	}

	var wg sync.WaitGroup
	for _, f := range flows {
		wg.Add(1)
		go func(f *flow) {
			defer wg.Done()
			f.rec = s.get(f.b, f.cb)
		}(f)
	}
	wg.Wait()

	for _, f := range flows {
		assert.Equal(t, http.StatusFound, f.rec.Code, f.rec.Body.String())
	}
	assert.Equal(t, int64(1), s.userCount(t), "one user for ten concurrent first logins")
	assert.Equal(t, int64(n), s.tokenRows(t), "each login has its own session")
}

// ---------------------------------------------------------------------------
// State, binding and errors from the provider.

func TestOIDCCallbackStateErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query func(state string) string
	}{
		{name: "no state", query: func(string) string { return "?code=abc" }},
		{name: "empty state", query: func(string) string { return "?state=&code=abc" }},
		{name: "unknown state", query: func(string) string { return "?state=" + strings.Repeat("A", 43) + "&code=abc" }},
		{name: "a state that is too long", query: func(string) string { return "?state=" + strings.Repeat("A", 5000) + "&code=abc" }},
		{name: "a state with other characters", query: func(string) string { return "?state=" + url.QueryEscape("a b'\"<>") + "&code=abc" }},
		{name: "the hash of the state is not the state", query: func(state string) string { return "?state=" + sha256Hexs(state) + "&code=abc" }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t)
			b := newBrowser()
			authURL := s.start(t, b)
			loc, err := url.Parse(authURL)
			require.NoError(t, err)

			w := s.get(b, oidcCallbackPath+tt.query(loc.Query().Get("state")))

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.JSONEq(t, bodyLoginFailed, w.Body.String())
			assert.Zero(t, s.idp.Hits("token"), "the provider is not asked to exchange anything")
			assertNoSession(t, s, w, 0)
		})
	}
}

func TestOIDCCallbackStateWorksOnce(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()
	authURL := s.start(t, b)
	cb, err := url.Parse(s.idp.Authorize(t, authURL, oidcClaims()))
	require.NoError(t, err)
	binding := b.cookies[hostBindingCookie]

	first := s.get(b, cb.RequestURI())
	require.Equal(t, http.StatusFound, first.Code)
	replay := newBrowser()
	replay.cookies[hostBindingCookie] = binding // the replaying browser even has the cookie
	second := s.get(replay, cb.RequestURI())

	assert.Equal(t, http.StatusBadRequest, second.Code, "a reused state is refused")
	assert.JSONEq(t, bodyLoginFailed, second.Body.String())
	assert.Equal(t, 1, s.idp.Hits("token"), "the code was exchanged once")
}

func TestOIDCCallbackExpiredState(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t, func(o *oidcSetup) { o.oidc = func(c *config.OIDCConfig) { c.StateTTL = 60 * time.Millisecond } })
	b := newBrowser()
	authURL := s.start(t, b)
	time.Sleep(200 * time.Millisecond)

	w := s.finish(t, b, authURL, oidcClaims())

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.JSONEq(t, bodyLoginFailed, w.Body.String())
	assert.Zero(t, s.idp.Hits("token"))
}

// The login CSRF: an attacker starts a login, authorizes at the provider with
// their own account and gets a callback URL (code and state). If the victim's
// browser opens it, the victim's browser has no binding cookie of that login,
// so the callback is refused and the attacker's session is never planted.
func TestOIDCCallbackRefusesABrowserThatDidNotStartTheLogin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cookie func(attacker *browser, victim *browser)
	}{
		{name: "no binding cookie", cookie: func(a, v *browser) {}},
		{name: "the binding cookie of another login", cookie: func(a, v *browser) {
			// The victim has an unrelated login in progress.
			w := httptest.NewRecorder()
			http.SetCookie(w, &http.Cookie{Name: hostBindingCookie, Value: strings.Repeat("v", 43), Path: "/", MaxAge: 300})
			v.store(w)
		}},
		{name: "an empty binding cookie", cookie: func(a, v *browser) {
			v.cookies[hostBindingCookie] = &http.Cookie{Name: hostBindingCookie, Value: "", Path: "/"}
		}},
		{name: "the hash of the binding value instead of the value", cookie: func(a, v *browser) {
			v.cookies[hostBindingCookie] = &http.Cookie{Name: hostBindingCookie, Value: sha256Hexs(a.cookies[hostBindingCookie].Value), Path: "/"}
		}},
		{name: "a binding cookie scoped to another path is not sent", cookie: func(a, v *browser) {
			v.cookies[hostBindingCookie] = &http.Cookie{Name: hostBindingCookie, Value: a.cookies[hostBindingCookie].Value, Path: "/elsewhere"}
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t)
			attacker, victim := newBrowser(), newBrowser()
			authURL := s.start(t, attacker)
			cb, err := url.Parse(s.idp.Authorize(t, authURL, oidcClaims("sub", "attacker-sub", "roles", []string{"platform-admins"})))
			require.NoError(t, err)
			tt.cookie(attacker, victim)

			w := s.get(victim, cb.RequestURI())

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.JSONEq(t, bodyLoginFailed, w.Body.String(), "the same answer as every other refusal")
			assert.Zero(t, s.idp.Hits("token"), "the code is not exchanged")
			assertNoSession(t, s, w, 0)
			assert.NotContains(t, victim.cookies, hostRefreshCookie)

			// The state is burnt: the real browser cannot complete it either, so
			// a failed attack leaves nothing to retry.
			retry := s.get(attacker, cb.RequestURI())
			assert.Equal(t, http.StatusBadRequest, retry.Code)
		})
	}
}

func TestOIDCCallbackRefusesAStateStoredWithoutABinding(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	state := strings.Repeat("S", 43)
	require.NoError(t, s.store.SaveOIDCState(context.Background(), state, sessionstore.OIDCStateData{CodeVerifier: "v", Nonce: "n"}, time.Minute))
	b := newBrowser()
	b.cookies[hostBindingCookie] = &http.Cookie{Name: hostBindingCookie, Value: "", Path: "/"}

	w := s.get(b, oidcCallbackPath+"?state="+state+"&code=abc")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Zero(t, s.idp.Hits("token"))
}

func TestOIDCCallbackProviderErrorParameter(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()
	authURL := s.start(t, b)
	loc, err := url.Parse(authURL)
	require.NoError(t, err)
	state := loc.Query().Get("state")

	w := s.get(b, oidcCallbackPath+"?error=access_denied&error_description="+url.QueryEscape("user said no <script>alert(1)</script>")+"&state="+state)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.JSONEq(t, bodyLoginFailed, w.Body.String(), "the description of the provider is not reflected")
	assert.NotContains(t, w.Body.String(), "script")
	assert.NotContains(t, s.logs.String(), "<script>", "nor logged raw")
	assert.Zero(t, s.idp.Hits("token"))
	assertNoSession(t, s, w, 0)

	// The state was consumed by the error callback.
	again := s.get(b, oidcCallbackPath+"?code=abc&state="+state)
	assert.Equal(t, http.StatusBadRequest, again.Code)
	assert.Zero(t, s.idp.Hits("token"))
}

func TestOIDCCallbackProviderErrorWithoutAKnownState(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)

	w := s.get(newBrowser(), oidcCallbackPath+"?error=server_error&error_description=x")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.JSONEq(t, bodyLoginFailed, w.Body.String())
}

func TestOIDCCallbackWithoutACode(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()
	authURL := s.start(t, b)
	loc, err := url.Parse(authURL)
	require.NoError(t, err)

	for _, q := range []string{"&code=", "", "&code=" + strings.Repeat("c", 5000)} {
		w := s.get(b, oidcCallbackPath+"?state="+loc.Query().Get("state")+q)
		assert.Equal(t, http.StatusBadRequest, w.Code, q)
		assert.JSONEq(t, bodyLoginFailed, w.Body.String())
		// Re-arm: each case needs a fresh state.
		authURL = s.start(t, b)
		loc, err = url.Parse(authURL)
		require.NoError(t, err)
	}
	assert.Zero(t, s.idp.Hits("token"))
}

func TestOIDCCallbackProviderFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		knob     func(k *authtest.Knobs)
		wantCode int
		wantBody string
	}{
		{name: "the provider rejects the code", knob: func(k *authtest.Knobs) { k.TokenStatus = http.StatusBadRequest }, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "the provider has a server error", knob: func(k *authtest.Knobs) { k.TokenStatus = http.StatusInternalServerError }, wantCode: http.StatusBadGateway, wantBody: bodyProviderDown},
		{name: "no id_token", knob: func(k *authtest.Knobs) { k.OmitIDToken = true }, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "another nonce", knob: func(k *authtest.Knobs) {
			k.ClaimsHook = func(c jwt.MapClaims) { c["nonce"] = "another" }
		}, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "no nonce", knob: func(k *authtest.Knobs) {
			k.ClaimsHook = func(c jwt.MapClaims) { delete(c, "nonce") }
		}, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "wrong issuer", knob: func(k *authtest.Knobs) {
			k.ClaimsHook = func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" }
		}, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "wrong audience", knob: func(k *authtest.Knobs) {
			k.ClaimsHook = func(c jwt.MapClaims) { c["aud"] = "other" }
		}, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "expired", knob: func(k *authtest.Knobs) {
			k.ClaimsHook = func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }
		}, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "alg none", knob: func(k *authtest.Knobs) { k.Signing = authtest.SignNone }, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "HS256 with the client secret", knob: func(k *authtest.Knobs) { k.Signing = authtest.SignHS256 }, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "an unknown signing key", knob: func(k *authtest.Knobs) { k.Signing = authtest.SignWrongKey }, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
		{name: "no sub", knob: func(k *authtest.Knobs) {
			k.ClaimsHook = func(c jwt.MapClaims) { delete(c, "sub") }
		}, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t)
			s.idp.Configure(tt.knob)
			b := newBrowser()
			authURL := s.start(t, b)
			cb, err := url.Parse(s.idp.Authorize(t, authURL, oidcClaims()))
			require.NoError(t, err)
			code := cb.Query().Get("code")

			w := s.get(b, cb.RequestURI())

			assert.Equal(t, tt.wantCode, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assertNoSession(t, s, w, 0)
			assert.NotContains(t, w.Body.String(), authtest.LeakMarker)
			logs := s.logs.String()
			assert.NotEmpty(t, logs, "the failure is logged")
			for _, secret := range []string{s.idp.ClientSecret, code, authtest.LeakMarker, "alice@example.com"} {
				assert.NotContains(t, logs, secret, "no secret, code or claim in the logs")
			}
		})
	}
}

// A token endpoint that refuses the client credentials (401) is a problem of
// this server's configuration, not a login the user can retry: 502 (the same
// answer as an unavailable provider), logged at Error level, no secret in the
// answer or the log. Another 4xx stays "login failed" at Warn level.
func TestOIDCCallbackRefusedClientCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		wantCode  int
		wantBody  string
		wantLevel string
	}{
		{name: "401: the client credentials are refused", status: http.StatusUnauthorized, wantCode: http.StatusBadGateway, wantBody: bodyProviderDown, wantLevel: "ERROR"},
		{name: "400: the code is refused", status: http.StatusBadRequest, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed, wantLevel: "WARN"},
		{name: "403: another 4xx stays a rejected login", status: http.StatusForbidden, wantCode: http.StatusBadRequest, wantBody: bodyLoginFailed, wantLevel: "WARN"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t)
			s.idp.Configure(func(k *authtest.Knobs) { k.TokenStatus = tt.status })
			b := newBrowser()
			authURL := s.start(t, b)
			cb, err := url.Parse(s.idp.Authorize(t, authURL, oidcClaims()))
			require.NoError(t, err)
			code := cb.Query().Get("code")

			w := s.get(b, cb.RequestURI())

			assert.Equal(t, tt.wantCode, w.Code)
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assertNoSession(t, s, w, 0)
			var found []logRecord
			for _, r := range recordsOf(t, s.logs) {
				if strings.HasPrefix(r.Msg, "OIDC provider refused") || strings.HasPrefix(r.Msg, "OIDC provider failure") || strings.HasPrefix(r.Msg, "OIDC login rejected") {
					found = append(found, r)
				}
			}
			require.Len(t, found, 1)
			assert.Equal(t, tt.wantLevel, found[0].Level)
			for _, secret := range []string{s.idp.ClientSecret, code, authtest.LeakMarker} {
				assert.NotContains(t, s.logs.String(), secret)
			}
		})
	}
}

func TestOIDCCallbackCancelledRequest(t *testing.T) {
	t.Parallel()
	t.Run("before the state is read", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t)
		b := newBrowser()
		authURL := s.start(t, b)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		w := s.finish(t, b, authURL, oidcClaims(), withContext(ctx))

		assert.Equal(t, statusClientClosedRequest, w.Code)
		assertNoSession(t, s, w, 0)
	})
	t.Run("during the token exchange", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t)
		s.idp.Configure(func(k *authtest.Knobs) { k.TokenDelay = 5 * time.Second })
		b := newBrowser()
		authURL := s.start(t, b)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(150*time.Millisecond, cancel)
		start := time.Now()

		w := s.finish(t, b, authURL, oidcClaims(), withContext(ctx))

		assert.Equal(t, statusClientClosedRequest, w.Code)
		assert.Less(t, time.Since(start), 3*time.Second)
		assertNoSession(t, s, w, 0)
	})
}

func TestOIDCCallbackExchangeTimeIsBounded(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	s.h.exchangeTimeout = 200 * time.Millisecond
	s.idp.Configure(func(k *authtest.Knobs) { k.TokenDelay = 5 * time.Second })
	b := newBrowser()
	authURL := s.start(t, b)
	start := time.Now()

	w := s.finish(t, b, authURL, oidcClaims())

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.JSONEq(t, bodyProviderDown, w.Body.String())
	assert.Less(t, time.Since(start), 3*time.Second)
	assertNoSession(t, s, w, 0)
}

func TestOIDCCallbackInternalErrors(t *testing.T) {
	t.Parallel()
	t.Run("the state store fails", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t, func(o *oidcSetup) {
			o.wrapStore = func(st sessionstore.SessionStore) sessionstore.SessionStore {
				return &failingStore{SessionStore: st, consumeErr: errors.New("connection to 10.0.0.5 refused")}
			}
		})
		b := newBrowser()
		authURL := s.start(t, b)

		w := s.finish(t, b, authURL, oidcClaims())

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, bodyInternal, w.Body.String())
		assert.NotContains(t, w.Body.String(), "10.0.0.5")
		assertNoSession(t, s, w, 0)
	})
	t.Run("the user lookup fails", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t)
		s.users.findExternalErr = errors.New("table users is corrupt")

		_, w := s.signIn(t, oidcClaims())

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, bodyInternal, w.Body.String())
		assertNoSession(t, s, w, 0)
	})
	t.Run("the user cannot be created", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t)
		s.users.createErr = errors.New("disk full")

		_, w := s.signIn(t, oidcClaims())

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, bodyInternal, w.Body.String())
		assertNoSession(t, s, w, 0)
	})
	t.Run("the session cannot be stored", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t, func(o *oidcSetup) {
			o.wrapTokens = func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, createErr: errors.New("token table locked")}
			}
		})

		_, w := s.signIn(t, oidcClaims())

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, bodyInternal, w.Body.String())
		assert.Nil(t, cookieNamed(w, hostRefreshCookie))
	})
	t.Run("the session was revoked before the answer", func(t *testing.T) {
		t.Parallel()
		s := newOIDCHarness(t, func(o *oidcSetup) {
			o.wrapTokens = func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
				return &faultyTokens{RefreshTokenRepository: r, zeroActive: true}
			}
		})

		_, w := s.signIn(t, oidcClaims())

		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, bodyNotPermitted, w.Body.String())
		assert.Nil(t, cookieNamed(w, hostRefreshCookie), "no cookie for a revoked session")
	})
}

// ---------------------------------------------------------------------------
// The redirect and the logs.

func TestOIDCCallbackRedirectIsFixed(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/auth/callback", "/welcome/home"} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			s := newOIDCHarness(t, func(o *oidcSetup) { o.oidc = func(c *config.OIDCConfig) { c.FrontendPath = path } })
			b := newBrowser()
			authURL := s.start(t, b)
			cb, err := url.Parse(s.idp.Authorize(t, authURL, oidcClaims()))
			require.NoError(t, err)

			// A hostile link adds redirect parameters of every usual name.
			q := cb.Query()
			for _, k := range []string{"redirect", "redirect_uri", "next", "return_to", "returnUrl", "url", "target", "continue"} {
				q.Set(k, "https://evil.example.com/steal")
			}
			w := s.get(b, cb.Path+"?"+q.Encode())

			require.Equal(t, http.StatusFound, w.Code)
			assert.Equal(t, path, w.Header().Get("Location"))
			assert.NotContains(t, w.Header().Get("Location"), "evil")
		})
	}
}

func TestOIDCNoTokensInTheRedirectOrTheLogs(t *testing.T) {
	t.Parallel()
	s := newOIDCHarness(t)
	b := newBrowser()
	authURL := s.start(t, b)
	loc, err := url.Parse(authURL)
	require.NoError(t, err)
	state := loc.Query().Get("state")
	binding := b.cookies[hostBindingCookie].Value
	cbURL := s.idp.Authorize(t, authURL, oidcClaims())
	cb, err := url.Parse(cbURL)
	require.NoError(t, err)
	code := cb.Query().Get("code")

	w := s.get(b, cb.RequestURI())

	require.Equal(t, http.StatusFound, w.Code)
	refresh := b.cookies[hostRefreshCookie]
	require.NotNil(t, refresh)
	assert.Equal(t, "/auth/callback", w.Header().Get("Location"))
	for _, h := range w.Result().Header.Values("Location") {
		assert.NotContains(t, h, "token")
		assert.NotContains(t, h, refresh.Value)
	}
	logs := s.logs.String()
	assert.Contains(t, logs, "OIDC login succeeded")
	for _, secret := range []string{state, binding, code, refresh.Value, s.idp.ClientSecret, loc.Query().Get("nonce"), "Bearer", "access_token"} {
		assert.NotContains(t, logs, secret)
	}
	var tokens []models.RefreshToken
	require.NoError(t, s.db.Find(&tokens).Error)
	require.Len(t, tokens, 1)
	assert.NotEqual(t, refresh.Value, tokens[0].TokenHash, "only the hash of the refresh token is stored")
}

func TestWellFormedOIDCState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{in: strings.Repeat("A", 43), want: true},
		{in: "a-b_C9", want: true},
		{in: strings.Repeat("A", 128), want: true},
		{in: strings.Repeat("A", 129)},
		{in: ""},
		{in: "a b"},
		{in: "a=b"},
		{in: "a/b"},
		{in: "a+b"},
		{in: "\x00"},
		{in: "é"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, wellFormedOIDCState(tt.in))
		})
	}
}

func TestSafeLogToken(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "access_denied", safeLogToken("access_denied"))
	assert.Equal(t, "invalid_request.x-1", safeLogToken("invalid_request.x-1"))
	for _, bad := range []string{"", "a b", "<script>", "a\nb", strings.Repeat("a", 65), "é"} {
		assert.Equal(t, "invalid", safeLogToken(bad), bad)
	}
}

func TestGenerateUsername(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		name, err := generateUsername()
		require.NoError(t, err)
		assert.Regexp(t, `^oidc_[0-9a-f]{16}$`, name)
		assert.False(t, seen[name])
		seen[name] = true
	}
}

func TestOIDCRedirectHasNoBody(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	redirect(c, "/somewhere?a=b")

	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "/somewhere?a=b", w.Header().Get("Location"))
	assert.Empty(t, w.Body.String())
}

func TestNewOIDCHandler(t *testing.T) {
	t.Parallel()
	cfg := &config.OIDCConfig{Enabled: true, StateTTL: time.Minute, FrontendPath: "/x"}
	authH := NewAuthHandler(nil, testAuthConfig(), cfg)

	h := NewOIDCHandler(nil, authH, nil, nil, cfg)

	require.NotNil(t, h)
	assert.Equal(t, oidcExchangeTimeout, h.exchangeTimeout)
	assert.True(t, h.enabled())
	var nilCfg *OIDCHandler
	assert.False(t, nilCfg.enabled(), "a nil handler is disabled")
}

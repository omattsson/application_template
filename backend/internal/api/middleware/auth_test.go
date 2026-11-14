package middleware

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "unit-test-secret-0123456789"

// signClaims signs claims with the given method and key.
func signClaims(t *testing.T, method jwt.SigningMethod, key interface{}, claims jwt.Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	require.NoError(t, err)
	return s
}

// validClaims returns claims that ValidateJWT accepts.
func validClaims() Claims {
	now := time.Now()
	return Claims{
		UserID:   "u1",
		Username: "alice",
		Role:     "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "jti-1",
			Subject:   "u1",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
}

func TestValidateJWT(t *testing.T) {
	t.Parallel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	mutate := func(f func(*Claims)) Claims {
		c := validClaims()
		f(&c)
		return c
	}
	sign := func(c Claims) string { return signClaims(t, jwt.SigningMethodHS256, []byte(testSecret), c) }

	tests := []struct {
		name      string
		token     string
		secret    string
		wantValid bool
	}{
		{name: "valid", token: sign(validClaims()), secret: testSecret, wantValid: true},
		{name: "wrong secret", token: signClaims(t, jwt.SigningMethodHS256, []byte("another-secret-0123456789"), validClaims()), secret: testSecret},
		{name: "expired", token: sign(mutate(func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) })), secret: testSecret},
		{name: "missing exp", token: sign(mutate(func(c *Claims) { c.ExpiresAt = nil })), secret: testSecret},
		{name: "not yet valid", token: sign(mutate(func(c *Claims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour)) })), secret: testSecret},
		{name: "empty subject", token: sign(mutate(func(c *Claims) { c.Subject = "" })), secret: testSecret},
		{name: "empty user_id", token: sign(mutate(func(c *Claims) { c.UserID = "" })), secret: testSecret},
		{name: "empty claims with exp only", token: signClaims(t, jwt.SigningMethodHS256, []byte(testSecret), jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}), secret: testSecret},
		{name: "empty claims", token: signClaims(t, jwt.SigningMethodHS256, []byte(testSecret), jwt.MapClaims{}), secret: testSecret},
		{name: "alg none", token: signClaims(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, validClaims()), secret: testSecret},
		{name: "alg none with empty secret", token: signClaims(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, validClaims()), secret: ""},
		{name: "HS384 is not accepted", token: signClaims(t, jwt.SigningMethodHS384, []byte(testSecret), validClaims()), secret: testSecret},
		{name: "HS512 is not accepted", token: signClaims(t, jwt.SigningMethodHS512, []byte(testSecret), validClaims()), secret: testSecret},
		{name: "RS256 is not accepted", token: signClaims(t, jwt.SigningMethodRS256, rsaKey, validClaims()), secret: testSecret},
		{name: "RS256 with the public key as secret", token: signClaims(t, jwt.SigningMethodRS256, rsaKey, validClaims()), secret: string(pubPEM)},
		{name: "HS256 signed with a public key used as HMAC secret", token: signClaims(t, jwt.SigningMethodHS256, pubPEM, validClaims()), secret: testSecret},
		{name: "empty secret never validates", token: signClaims(t, jwt.SigningMethodHS256, []byte(""), validClaims()), secret: ""},
		{name: "empty secret with a valid token", token: sign(validClaims()), secret: ""},
		{name: "empty token", token: "", secret: testSecret},
		{name: "malformed", token: "not.a.jwt", secret: testSecret},
		{name: "two segments", token: "abc.def", secret: testSecret},
		{name: "garbage", token: "!!!", secret: testSecret},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			claims, err := ValidateJWT(tt.token, tt.secret)
			if tt.wantValid {
				require.NoError(t, err)
				require.NotNil(t, claims)
				assert.Equal(t, "u1", claims.UserID)
				assert.Equal(t, "alice", claims.Username)
				assert.Equal(t, "user", claims.Role)
				return
			}
			require.Error(t, err)
			assert.Nil(t, claims)
			if tt.secret != "" {
				assert.NotContains(t, err.Error(), tt.secret, "the error must not contain the secret")
			}
		})
	}
}

func TestClaimsIssueTime(t *testing.T) {
	t.Parallel()
	ms := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	iat := time.Date(2026, 1, 2, 3, 4, 5, 900_000_000, time.UTC)

	var nilClaims *Claims
	assert.True(t, nilClaims.IssueTime().IsZero())
	assert.True(t, (&Claims{}).IssueTime().IsZero())
	assert.True(t, ms.Equal((&Claims{IssuedAtMs: ms.UnixMilli()}).IssueTime()))
	c := &Claims{RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(iat)}}
	assert.True(t, iat.Truncate(time.Second).Equal(c.IssueTime()), "iat counts in whole seconds")
	c.IssuedAtMs = ms.UnixMilli()
	assert.True(t, ms.Equal(c.IssueTime()), "iat_ms wins over iat")
}

func TestGenerateToken(t *testing.T) {
	t.Parallel()

	tok, err := GenerateToken("u1", "alice", "devops", testSecret, time.Hour)
	require.NoError(t, err)
	claims, err := ValidateJWT(tok, testSecret)
	require.NoError(t, err)
	assert.Equal(t, "u1", claims.UserID)
	assert.Equal(t, "u1", claims.Subject)
	assert.Equal(t, "alice", claims.Username)
	assert.Equal(t, "devops", claims.Role)
	assert.NotEmpty(t, claims.ID)
	assert.NotZero(t, claims.IssuedAtMs)
	assert.WithinDuration(t, time.Now().Add(time.Hour), claims.ExpiresAt.Time, 5*time.Second)

	parsed, _, err := jwt.NewParser().ParseUnverified(tok, &Claims{})
	require.NoError(t, err)
	assert.Equal(t, "HS256", parsed.Header["alg"])

	tok2, err := GenerateToken("u1", "alice", "devops", testSecret, time.Hour)
	require.NoError(t, err)
	claims2, err := ValidateJWT(tok2, testSecret)
	require.NoError(t, err)
	assert.NotEqual(t, claims.ID, claims2.ID, "each token has its own jti")

	t.Run("options carry optional claims", func(t *testing.T) {
		t.Parallel()
		tok, err := GenerateTokenWithOpts(GenerateTokenOptions{
			UserID: "u2", Username: "bob", DisplayName: "Bob", Role: "admin", Secret: testSecret,
			Expiration: time.Minute, AuthProvider: "oidc", Email: "bob@example.com", SessionID: "sid-1",
		})
		require.NoError(t, err)
		c, err := ValidateJWT(tok, testSecret)
		require.NoError(t, err)
		assert.Equal(t, "Bob", c.DisplayName)
		assert.Equal(t, "oidc", c.AuthProvider)
		assert.Equal(t, "bob@example.com", c.Email)
		assert.Equal(t, "sid-1", c.SessionID)
	})
}

// jwtTestRouter returns a router with the JWT middleware on GET /protected.
// The handler echoes the context values.
func jwtTestRouter(opts JWTAuthOptions) *gin.Engine {
	r := gin.New()
	r.GET("/protected", AuthRequiredWithOptions(opts), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"user_id":  GetUserIDFromContext(c),
			"username": GetUsernameFromContext(c),
			"role":     GetRoleFromContext(c),
			"jti":      GetJTIFromContext(c),
			"sid":      GetSessionIDFromContext(c),
		})
	})
	return r
}

func doGet(r http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	return doGetCtx(context.Background(), r, path, headers)
}

func doGetCtx(ctx context.Context, r http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body["error"]
}

func bearer(t *testing.T, c Claims) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "Bearer " + signClaims(t, jwt.SigningMethodHS256, []byte(testSecret), c)}
}

func TestAuthRequiredHeaderParsing(t *testing.T) {
	t.Parallel()
	r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret})
	good, err := GenerateToken("u1", "alice", "user", testSecret, time.Hour)
	require.NoError(t, err)

	tests := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{name: "valid bearer", path: "/protected", headers: map[string]string{"Authorization": "Bearer " + good}, want: http.StatusOK},
		{name: "scheme is case-insensitive", path: "/protected", headers: map[string]string{"Authorization": "bearer " + good}, want: http.StatusOK},
		{name: "no header", path: "/protected", want: http.StatusUnauthorized},
		{name: "empty header value", path: "/protected", headers: map[string]string{"Authorization": ""}, want: http.StatusUnauthorized},
		{name: "scheme only", path: "/protected", headers: map[string]string{"Authorization": "Bearer"}, want: http.StatusUnauthorized},
		{name: "scheme and empty token", path: "/protected", headers: map[string]string{"Authorization": "Bearer "}, want: http.StatusUnauthorized},
		{name: "token without scheme", path: "/protected", headers: map[string]string{"Authorization": good}, want: http.StatusUnauthorized},
		{name: "basic scheme", path: "/protected", headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, want: http.StatusUnauthorized},
		{name: "api key style scheme", path: "/protected", headers: map[string]string{"Authorization": "ApiKey sk_0123456789abcdef0123"}, want: http.StatusUnauthorized},
		{name: "two spaces before the token", path: "/protected", headers: map[string]string{"Authorization": "Bearer  " + good}, want: http.StatusUnauthorized},
		{name: "token with trailing text", path: "/protected", headers: map[string]string{"Authorization": "Bearer " + good + " extra"}, want: http.StatusUnauthorized},
		{name: "token in the query string is ignored", path: "/protected?token=" + good, want: http.StatusUnauthorized},
		{name: "access_token in the query string is ignored", path: "/protected?access_token=" + good, want: http.StatusUnauthorized},
		{name: "token in the X-API-Key header is ignored", path: "/protected", headers: map[string]string{"X-API-Key": good}, want: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := doGet(r, tt.path, tt.headers)
			assert.Equal(t, tt.want, w.Code)
			if tt.want == http.StatusUnauthorized {
				assert.NotEmpty(t, errorBody(t, w))
				assert.NotContains(t, w.Body.String(), testSecret)
			}
		})
	}
}

func TestAuthRequiredToken(t *testing.T) {
	t.Parallel()
	r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret})

	t.Run("sets the context values", func(t *testing.T) {
		t.Parallel()
		c := validClaims()
		c.SessionID = "sid-9"
		w := doGet(r, "/protected", bearer(t, c))
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, map[string]string{"user_id": "u1", "username": "alice", "role": "user", "jti": "jti-1", "sid": "sid-9"}, body)
	})

	t.Run("token signed with another secret", func(t *testing.T) {
		t.Parallel()
		h := map[string]string{"Authorization": "Bearer " + signClaims(t, jwt.SigningMethodHS256, []byte("another-secret-0123456789"), validClaims())}
		w := doGet(r, "/protected", h)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "Invalid or expired token", errorBody(t, w))
	})

	t.Run("expired token", func(t *testing.T) {
		t.Parallel()
		c := validClaims()
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		w := doGet(r, "/protected", bearer(t, c))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "Invalid or expired token", errorBody(t, w))
	})

	t.Run("alg none", func(t *testing.T) {
		t.Parallel()
		tok := signClaims(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, validClaims())
		w := doGet(r, "/protected", map[string]string{"Authorization": "Bearer " + tok})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("empty secret configured rejects every token", func(t *testing.T) {
		t.Parallel()
		empty := jwtTestRouter(JWTAuthOptions{JWTSecret: ""})
		tok := signClaims(t, jwt.SigningMethodHS256, []byte(""), validClaims())
		w := doGet(empty, "/protected", map[string]string{"Authorization": "Bearer " + tok})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// failingStore wraps a SessionStore and fails the lookups with err.
type failingStore struct {
	sessionstore.SessionStore
	err error
}

func (f failingStore) IsTokenBlocked(context.Context, string) (bool, error) { return false, f.err }
func (f failingStore) IsSessionBlocked(context.Context, string) (bool, error) {
	return false, f.err
}
func (f failingStore) IsUserBlocked(context.Context, string, time.Time) (bool, error) {
	return false, f.err
}

func TestAuthRequiredRevocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	until := time.Now().Add(time.Hour)

	t.Run("blocked jti gives 401 with the revocation message", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		require.NoError(t, store.BlockToken(ctx, "jti-1", until))
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store})
		w := doGet(r, "/protected", bearer(t, validClaims()))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, RevocationMessage(ErrTokenRevoked), errorBody(t, w))
		assert.Equal(t, "Token has been revoked", errorBody(t, w))
	})

	t.Run("another jti is not affected", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		require.NoError(t, store.BlockToken(ctx, "other-jti", until))
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store})
		w := doGet(r, "/protected", bearer(t, validClaims()))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("user block covers tokens issued before it only", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store})

		oldClaims := validClaims()
		oldClaims.IssuedAtMs = time.Now().Add(-time.Minute).UnixMilli()
		require.NoError(t, store.BlockUser(ctx, "u1", until))

		w := doGet(r, "/protected", bearer(t, oldClaims))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, RevocationMessage(ErrSessionRevoked), errorBody(t, w))
		assert.Equal(t, "Session revoked", errorBody(t, w))

		newClaims := validClaims()
		newClaims.IssuedAtMs = time.Now().Add(time.Minute).UnixMilli()
		assert.Equal(t, http.StatusOK, doGet(r, "/protected", bearer(t, newClaims)).Code)
	})

	t.Run("a token without iat and iat_ms is refused once a user block exists", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store})
		noIAT := validClaims()
		noIAT.IssuedAt = nil
		noIAT.IssuedAtMs = 0
		require.True(t, noIAT.IssueTime().IsZero(), "the token has no issue time")

		assert.Equal(t, http.StatusOK, doGet(r, "/protected", bearer(t, noIAT)).Code, "without a block it passes")

		require.NoError(t, store.BlockUser(ctx, "u1", until))
		w := doGet(r, "/protected", bearer(t, noIAT))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "no issue time cannot prove the token is newer than the block")
		assert.Equal(t, "Session revoked", errorBody(t, w))

		other := noIAT
		other.UserID = "u2"
		assert.Equal(t, http.StatusOK, doGet(r, "/protected", bearer(t, other)).Code, "another user's block does not apply")
	})

	t.Run("blocked session gives 401", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		require.NoError(t, store.BlockSession(ctx, "sid-1", until))
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store})

		c := validClaims()
		c.SessionID = "sid-1"
		w := doGet(r, "/protected", bearer(t, c))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "Session revoked", errorBody(t, w))

		c.SessionID = "sid-2"
		assert.Equal(t, http.StatusOK, doGet(r, "/protected", bearer(t, c)).Code)
	})

	t.Run("a token without jti is not looked up in the token blocklist", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store})
		c := validClaims()
		c.ID = ""
		assert.Equal(t, http.StatusOK, doGet(r, "/protected", bearer(t, c)).Code)
	})
}

func TestAuthRequiredStoreError(t *testing.T) {
	t.Parallel()
	dbDown := errors.New("connection refused to db.internal:3306")
	memory := sessionstore.NewMemory()
	t.Cleanup(memory.Stop)

	tests := []struct {
		name      string
		store     sessionstore.SessionStore
		strict    bool
		wantCode  int
		wantError string
	}{
		{name: "default fails open on a store error", store: failingStore{memory, dbDown}, wantCode: http.StatusOK},
		{name: "strict mode fails closed with 500", store: failingStore{memory, dbDown}, strict: true, wantCode: http.StatusInternalServerError, wantError: "Internal server error"},
		{name: "strict mode and a deadline stays 500", store: failingStore{memory, context.DeadlineExceeded}, strict: true, wantCode: http.StatusInternalServerError, wantError: "Internal server error"},
		{name: "a cancelled error gives 499", store: failingStore{memory, context.Canceled}, wantCode: 499, wantError: "Client closed request"},
		{name: "a wrapped cancelled error gives 499 in strict mode", store: failingStore{memory, errors.Join(errors.New("x"), context.Canceled)}, strict: true, wantCode: 499, wantError: "Client closed request"},
		{name: "default and a deadline fails open", store: failingStore{memory, context.DeadlineExceeded}, wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: tt.store, FailClosedOnStoreError: tt.strict})
			w := doGet(r, "/protected", bearer(t, validClaims()))
			assert.Equal(t, tt.wantCode, w.Code)
			if tt.wantError != "" {
				assert.Equal(t, tt.wantError, errorBody(t, w))
			}
			assert.NotContains(t, w.Body.String(), "db.internal", "the store error must not reach the client")
		})
	}

	t.Run("strict mode never runs the handler after a failed lookup", func(t *testing.T) {
		t.Parallel()
		for _, storeErr := range []error{dbDown, context.DeadlineExceeded} {
			ran := false
			r := gin.New()
			r.GET("/protected", AuthRequiredWithOptions(JWTAuthOptions{
				JWTSecret: testSecret, SessionStore: failingStore{memory, storeErr}, FailClosedOnStoreError: true,
			}), func(c *gin.Context) { ran = true })
			w := doGet(r, "/protected", bearer(t, validClaims()))
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.False(t, ran, "the request must not be admitted")
		}
		// A cancelled lookup is not admitted either.
		ran := false
		r := gin.New()
		r.GET("/protected", AuthRequiredWithOptions(JWTAuthOptions{
			JWTSecret: testSecret, SessionStore: failingStore{memory, context.Canceled}, FailClosedOnStoreError: true,
		}), func(c *gin.Context) { ran = true })
		assert.Equal(t, 499, doGet(r, "/protected", bearer(t, validClaims())).Code)
		assert.False(t, ran)
	})

	t.Run("a revocation wins over a failing lookup", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		require.NoError(t, store.BlockToken(context.Background(), "jti-1", time.Now().Add(time.Hour)))
		// The token check finds the block; the failing session store is never needed.
		mixed := mixedStore{SessionStore: store, sessionErr: dbDown}
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: mixed, FailClosedOnStoreError: true})
		c := validClaims()
		c.SessionID = "sid-1"
		w := doGet(r, "/protected", bearer(t, c))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "Token has been revoked", errorBody(t, w))
	})

	t.Run("one failing check does not hide a revocation found by another", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		require.NoError(t, store.BlockSession(context.Background(), "sid-1", time.Now().Add(time.Hour)))
		mixed := mixedStore{SessionStore: store, tokenErr: dbDown}
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: mixed})
		c := validClaims()
		c.SessionID = "sid-1"
		assert.Equal(t, http.StatusUnauthorized, doGet(r, "/protected", bearer(t, c)).Code)
	})

	t.Run("a request context cancelled before the store lookup gives 499", func(t *testing.T) {
		t.Parallel()
		store := sessionstore.NewMemory()
		defer store.Stop()
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := doGetCtx(ctx, r, "/protected", bearer(t, validClaims()))
		assert.Equal(t, 499, w.Code)
	})
}

// mixedStore fails only some lookups.
type mixedStore struct {
	sessionstore.SessionStore
	tokenErr, sessionErr error
}

func (m mixedStore) IsTokenBlocked(ctx context.Context, jti string) (bool, error) {
	if m.tokenErr != nil {
		return false, m.tokenErr
	}
	return m.SessionStore.IsTokenBlocked(ctx, jti)
}

func (m mixedStore) IsSessionBlocked(ctx context.Context, sid string) (bool, error) {
	if m.sessionErr != nil {
		return false, m.sessionErr
	}
	return m.SessionStore.IsSessionBlocked(ctx, sid)
}

func TestRevocationStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	claims := &Claims{UserID: "u1", SessionID: "s1", RegisteredClaims: jwt.RegisteredClaims{ID: "j1"}}

	revoked, lookupErr := RevocationStatus(ctx, nil, claims)
	assert.NoError(t, revoked, "nil store passes")
	assert.NoError(t, lookupErr)
	store := sessionstore.NewMemory()
	defer store.Stop()
	revoked, lookupErr = RevocationStatus(ctx, store, nil)
	assert.NoError(t, revoked, "nil claims pass")
	assert.NoError(t, lookupErr)
	revoked, lookupErr = RevocationStatus(ctx, store, claims)
	assert.NoError(t, revoked)
	assert.NoError(t, lookupErr)

	revoked, lookupErr = RevocationStatus(ctx, failingStore{store, errors.New("boom")}, claims)
	assert.NoError(t, revoked)
	require.Error(t, lookupErr)
	for _, part := range []string{"token blocklist", "session blocklist", "user blocklist"} {
		assert.Contains(t, lookupErr.Error(), part)
	}

	assert.Equal(t, "Token has been revoked", RevocationMessage(ErrTokenRevoked))
	assert.Equal(t, "Session revoked", RevocationMessage(ErrSessionRevoked))
	assert.Equal(t, "Session revoked", RevocationMessage(errors.New("anything else")))
}

func TestContextGettersZeroValues(t *testing.T) {
	t.Parallel()
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		return c
	}

	c := newCtx()
	assert.Empty(t, GetUserIDFromContext(c))
	assert.Empty(t, GetUsernameFromContext(c))
	assert.Empty(t, GetRoleFromContext(c))
	assert.Empty(t, GetJTIFromContext(c))
	assert.Empty(t, GetSessionIDFromContext(c))
	_, ok := GetTokenExpiryFromContext(c)
	assert.False(t, ok)

	// Values of the wrong type must not panic.
	c = newCtx()
	for _, k := range []string{contextKeyUserID, contextKeyUsername, contextKeyRole, contextKeyJTI, contextKeySID, contextKeyExpiry} {
		c.Set(k, 42)
	}
	assert.Empty(t, GetUserIDFromContext(c))
	assert.Empty(t, GetUsernameFromContext(c))
	assert.Empty(t, GetRoleFromContext(c))
	assert.Empty(t, GetJTIFromContext(c))
	assert.Empty(t, GetSessionIDFromContext(c))
	_, ok = GetTokenExpiryFromContext(c)
	assert.False(t, ok)

	// Correct types come back.
	c = newCtx()
	exp := time.Now()
	c.Set(contextKeyUserID, "u")
	c.Set(contextKeyExpiry, exp)
	assert.Equal(t, "u", GetUserIDFromContext(c))
	got, ok := GetTokenExpiryFromContext(c)
	assert.True(t, ok)
	assert.True(t, exp.Equal(got))
}

func TestAuthRequiredSetsExpiry(t *testing.T) {
	t.Parallel()
	r := gin.New()
	var gotOK bool
	r.GET("/x", AuthRequiredWithOptions(JWTAuthOptions{JWTSecret: testSecret}), func(c *gin.Context) {
		_, gotOK = GetTokenExpiryFromContext(c)
		c.Status(http.StatusNoContent)
	})
	w := doGet(r, "/x", bearer(t, validClaims()))
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, gotOK)
}

// The identity keys carry the "auth." prefix: a plain "userID" or "role" that
// other code puts on the context is not an identity, and SetTestIdentity sets
// exactly what the getters read.
func TestIdentityContextKeys(t *testing.T) {
	t.Parallel()
	keys := []string{contextKeyUserID, contextKeyUsername, contextKeyRole, contextKeyJTI, contextKeyExpiry,
		contextKeySID, contextKeyIssuedAt, contextKeyAuthMethod, contextKeyAPIKeyID}
	for _, k := range keys {
		assert.True(t, strings.HasPrefix(k, "auth."), k)
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("userID", "u-forged")
	c.Set("role", "admin")
	c.Set("authMethod", AuthMethodJWT)
	assert.Empty(t, GetUserIDFromContext(c))
	assert.Empty(t, GetRoleFromContext(c))
	assert.Empty(t, GetAuthMethodFromContext(c))

	SetTestIdentity(c, "u1", "devops", AuthMethodAPIKey)
	assert.Equal(t, "u1", GetUserIDFromContext(c))
	assert.Equal(t, "devops", GetRoleFromContext(c))
	assert.Equal(t, AuthMethodAPIKey, GetAuthMethodFromContext(c))

	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetTestIdentity(c2, "u2", "user", "")
	assert.Empty(t, GetAuthMethodFromContext(c2), "no method when none is given")
}

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// explodingAPIKeyRepo and explodingUserRepo panic on any call. They prove
// that this task's CombinedAuth never reaches the API-key repositories.
type explodingAPIKeyRepo struct{ models.APIKeyRepository }
type explodingUserRepo struct{ models.UserRepository }

func combinedRouter(deps APIKeyAuthDeps) *gin.Engine {
	r := gin.New()
	r.GET("/protected", CombinedAuth(deps), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": GetUserIDFromContext(c), "role": GetRoleFromContext(c)})
	})
	return r
}

func TestCombinedAuth(t *testing.T) {
	t.Parallel()

	store := sessionstore.NewMemory()
	t.Cleanup(store.Stop)
	good, err := GenerateToken("u1", "alice", "user", testSecret, time.Hour)
	require.NoError(t, err)
	revoked, err := GenerateToken("u2", "bob", "user", testSecret, time.Hour)
	require.NoError(t, err)
	revokedClaims, err := ValidateJWT(revoked, testSecret)
	require.NoError(t, err)
	require.NoError(t, store.BlockToken(t.Context(), revokedClaims.ID, time.Now().Add(time.Hour)))

	jwtOnly := APIKeyAuthDeps{JWTSecret: testSecret, SessionStore: store}
	withRepos := APIKeyAuthDeps{
		JWTSecret: testSecret, SessionStore: store,
		APIKeyRepo: explodingAPIKeyRepo{}, UserRepo: explodingUserRepo{},
	}
	const rawKey = "sk_0123456789abcdef0123456789abcdef"

	tests := []struct {
		name    string
		deps    APIKeyAuthDeps
		headers map[string]string
		want    int
	}{
		{name: "JWT only: valid bearer", deps: jwtOnly, headers: map[string]string{"Authorization": "Bearer " + good}, want: http.StatusOK},
		{name: "JWT only: no credentials", deps: jwtOnly, want: http.StatusUnauthorized},
		{name: "JWT only: garbage bearer", deps: jwtOnly, headers: map[string]string{"Authorization": "Bearer garbage"}, want: http.StatusUnauthorized},
		{name: "JWT only: revoked bearer", deps: jwtOnly, headers: map[string]string{"Authorization": "Bearer " + revoked}, want: http.StatusUnauthorized},
		{name: "JWT only: ApiKey scheme does not authenticate", deps: jwtOnly, headers: map[string]string{"Authorization": "ApiKey " + rawKey}, want: http.StatusUnauthorized},
		{name: "JWT only: X-API-Key does not authenticate", deps: jwtOnly, headers: map[string]string{"X-API-Key": rawKey}, want: http.StatusUnauthorized},
		{name: "JWT only: Bearer with a key does not authenticate", deps: jwtOnly, headers: map[string]string{"Authorization": "Bearer " + rawKey}, want: http.StatusUnauthorized},

		{name: "repos set: valid bearer", deps: withRepos, headers: map[string]string{"Authorization": "Bearer " + good}, want: http.StatusOK},
		{name: "repos set: revoked bearer", deps: withRepos, headers: map[string]string{"Authorization": "Bearer " + revoked}, want: http.StatusUnauthorized},
		{name: "repos set: garbage bearer does not fall through to the key", deps: withRepos, headers: map[string]string{"Authorization": "Bearer garbage", "X-API-Key": rawKey}, want: http.StatusUnauthorized},
		{name: "repos set: no credentials", deps: withRepos, want: http.StatusUnauthorized},
		{name: "repos set: ApiKey scheme does not authenticate", deps: withRepos, headers: map[string]string{"Authorization": "ApiKey " + rawKey}, want: http.StatusUnauthorized},
		{name: "repos set: X-API-Key is refused until key verification exists", deps: withRepos, headers: map[string]string{"X-API-Key": rawKey}, want: http.StatusUnauthorized},
		{name: "repos set: only one repo set behaves as JWT only", deps: APIKeyAuthDeps{JWTSecret: testSecret, APIKeyRepo: explodingAPIKeyRepo{}}, headers: map[string]string{"X-API-Key": rawKey}, want: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := doGet(combinedRouter(tt.deps), "/protected", tt.headers)
			assert.Equal(t, tt.want, w.Code)
			if tt.want == http.StatusUnauthorized {
				assert.NotEmpty(t, errorBody(t, w))
				assert.NotContains(t, w.Body.String(), rawKey)
			}
		})
	}
}

func TestCombinedAuthSetsContext(t *testing.T) {
	t.Parallel()
	tok, err := GenerateToken("u1", "alice", "devops", testSecret, time.Hour)
	require.NoError(t, err)
	r := gin.New()
	r.GET("/x", CombinedAuth(APIKeyAuthDeps{JWTSecret: testSecret}), func(c *gin.Context) {
		assert.Equal(t, "u1", GetUserIDFromContext(c))
		assert.Equal(t, "alice", GetUsernameFromContext(c))
		assert.Equal(t, "devops", GetRoleFromContext(c))
		assert.Equal(t, AuthMethodJWT, GetAuthMethodFromContext(c))
		assert.NotEmpty(t, GetJTIFromContext(c))
		c.Status(http.StatusNoContent)
	})
	assert.Equal(t, http.StatusNoContent, doGet(r, "/x", map[string]string{"Authorization": "Bearer " + tok}).Code)
}

func TestCombinedAuthPassesStoreSettings(t *testing.T) {
	t.Parallel()
	failing := failingStore{SessionStore: sessionstore.NewMemory(), err: assert.AnError}
	tok, err := GenerateToken("u1", "alice", "user", testSecret, time.Hour)
	require.NoError(t, err)
	h := map[string]string{"Authorization": "Bearer " + tok}

	open := combinedRouter(APIKeyAuthDeps{JWTSecret: testSecret, SessionStore: failing})
	assert.Equal(t, http.StatusOK, doGet(open, "/protected", h).Code)

	strict := combinedRouter(APIKeyAuthDeps{JWTSecret: testSecret, SessionStore: failing, FailClosedOnStoreError: true})
	assert.Equal(t, http.StatusInternalServerError, doGet(strict, "/protected", h).Code)
}

func TestAuthMethodGetters(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.Empty(t, GetAuthMethodFromContext(c))
	c.Set(contextKeyAuthMethod, 7)
	assert.Empty(t, GetAuthMethodFromContext(c), "a value of the wrong type reads as empty")
}

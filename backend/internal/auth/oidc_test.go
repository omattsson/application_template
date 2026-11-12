package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/auth"
	"backend/internal/auth/authtest"
	"backend/internal/config"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testNonce = "nonce-value-for-the-test"

// newConfig returns a valid OIDC config for the fake provider.
func newConfig(idp *authtest.IdP) *config.OIDCConfig {
	return &config.OIDCConfig{
		Enabled:      true,
		Issuer:       idp.Issuer,
		ClientID:     idp.ClientID,
		ClientSecret: idp.ClientSecret,
		RedirectURL:  idp.RedirectURL,
		Scopes:       []string{"openid", "profile", "email"},
		RoleClaim:    "roles",
		AdminRoles:   []string{"platform-admins"},
		DevOpsRoles:  []string{"builders"},
		StateTTL:     5 * time.Minute,
		FrontendPath: "/auth/callback",
	}
}

func testOptions() auth.Options {
	return auth.Options{AllowInsecureLoopbackForTesting: true}
}

func newProvider(t *testing.T, idp *authtest.IdP, mutate ...func(*config.OIDCConfig)) *auth.Provider {
	t.Helper()
	cfg := newConfig(idp)
	for _, m := range mutate {
		m(cfg)
	}
	p, err := auth.NewProvider(cfg, testOptions())
	require.NoError(t, err)
	return p
}

func baseClaims() map[string]any {
	return map[string]any{
		"sub":                "user-1",
		"email":              "alice@example.com",
		"email_verified":     true,
		"name":               "Alice Example",
		"preferred_username": "alice",
		"roles":              []string{"staff"},
	}
}

func with(claims map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(claims)+len(kv)/2)
	for k, v := range claims {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// runLogin plays one login: the authorization request, the user at the
// provider, and the code exchange. It returns the verifier it used.
func runLogin(t *testing.T, p *auth.Provider, idp *authtest.IdP, claims map[string]any) (*auth.Identity, error) {
	t.Helper()
	ident, _, err := runLoginWithVerifier(t, p, idp, claims)
	return ident, err
}

func runLoginWithVerifier(t *testing.T, p *auth.Provider, idp *authtest.IdP, claims map[string]any) (*auth.Identity, string, error) {
	t.Helper()
	verifier, challenge, err := auth.NewPKCE()
	require.NoError(t, err)
	authURL, err := p.AuthCodeURL(context.Background(), "state-1", challenge, testNonce)
	require.NoError(t, err)
	cb := idp.Authorize(t, authURL, claims)
	u, err := url.Parse(cb)
	require.NoError(t, err)
	ident, err := p.Exchange(context.Background(), u.Query().Get("code"), verifier, testNonce)
	return ident, verifier, err
}

func TestNewProviderValidatesTheConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(c *config.OIDCConfig)
		opts    auth.Options
		wantErr string
	}{
		{name: "https issuer", mutate: func(c *config.OIDCConfig) { c.Issuer = "https://idp.example.com" }},
		{name: "empty issuer", mutate: func(c *config.OIDCConfig) { c.Issuer = "" }, wantErr: "issuer"},
		{name: "http issuer", mutate: func(c *config.OIDCConfig) { c.Issuer = "http://idp.example.com" }, wantErr: "issuer"},
		{name: "http loopback issuer without the test option", mutate: func(c *config.OIDCConfig) { c.Issuer = "http://127.0.0.1:9" }, wantErr: "issuer"},
		{name: "http localhost issuer without the test option", mutate: func(c *config.OIDCConfig) { c.Issuer = "http://localhost:9" }, wantErr: "issuer"},
		{name: "http loopback issuer with the test option", mutate: func(c *config.OIDCConfig) { c.Issuer = "http://127.0.0.1:9" }, opts: testOptions()},
		{name: "http IPv6 loopback issuer with the test option", mutate: func(c *config.OIDCConfig) { c.Issuer = "http://[::1]:9" }, opts: testOptions()},
		{name: "http public issuer even with the test option", mutate: func(c *config.OIDCConfig) { c.Issuer = "http://idp.example.com" }, opts: testOptions(), wantErr: "issuer"},
		{name: "issuer with credentials", mutate: func(c *config.OIDCConfig) { c.Issuer = "https://u:p@idp.example.com" }, wantErr: "issuer"},
		{name: "other scheme", mutate: func(c *config.OIDCConfig) { c.Issuer = "ftp://idp.example.com" }, wantErr: "issuer"},
		{name: "empty client id", mutate: func(c *config.OIDCConfig) { c.ClientID = "" }, wantErr: "client id"},
		{name: "empty client secret", mutate: func(c *config.OIDCConfig) { c.ClientSecret = "" }, wantErr: "client secret"},
		{name: "empty redirect url", mutate: func(c *config.OIDCConfig) { c.RedirectURL = "" }, wantErr: "redirect url"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.OIDCConfig{
				Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "very-secret-value",
				RedirectURL: "https://app.example.com/cb", Scopes: []string{"openid"}, RoleClaim: "roles",
			}
			tt.mutate(cfg)

			p, err := auth.NewProvider(cfg, tt.opts)

			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.NotNil(t, p)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), "very-secret-value")
			assert.NotContains(t, err.Error(), "u:p", "credentials of a URL are not echoed")
		})
	}
	t.Run("nil config", func(t *testing.T) {
		t.Parallel()
		_, err := auth.NewProvider(nil, auth.Options{})
		require.Error(t, err)
	})
}

func TestNewProviderDoesNotContactTheProvider(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")

	_ = newProvider(t, idp)

	assert.Zero(t, idp.Hits("discovery"), "discovery waits for the first login, so an IdP outage at start does not stop the app")
}

func TestRandomTokenAndPKCE(t *testing.T) {
	t.Parallel()
	t.Run("state, nonce and binding values are random and at least 128 bits", func(t *testing.T) {
		t.Parallel()
		seen := map[string]bool{}
		for i := 0; i < 2000; i++ {
			v, err := auth.RandomToken()
			require.NoError(t, err)
			raw, err := base64.RawURLEncoding.DecodeString(v)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, len(raw), 16)
			assert.False(t, seen[v], "no repeated value")
			seen[v] = true
		}
	})
	t.Run("the PKCE verifier is 43 to 128 unreserved characters and the challenge is its S256", func(t *testing.T) {
		t.Parallel()
		seen := map[string]bool{}
		for i := 0; i < 200; i++ {
			verifier, challenge, err := auth.NewPKCE()
			require.NoError(t, err)
			assert.GreaterOrEqual(t, len(verifier), 43)
			assert.LessOrEqual(t, len(verifier), 128)
			assert.Regexp(t, `^[A-Za-z0-9_-]+$`, verifier)
			sum := sha256.Sum256([]byte(verifier))
			assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), challenge)
			assert.NotEqual(t, verifier, challenge, "the challenge is never the plain verifier")
			assert.False(t, seen[verifier])
			seen[verifier] = true
		}
	})
}

func TestAuthCodeURL(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "/realms/app")
	p := newProvider(t, idp)
	verifier, challenge, err := auth.NewPKCE()
	require.NoError(t, err)

	raw, err := p.AuthCodeURL(context.Background(), "the-state", challenge, "the-nonce")
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, idp.Server.URL+"/realms/app/authorize", u.Scheme+"://"+u.Host+u.Path)
	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, idp.ClientID, q.Get("client_id"))
	assert.Equal(t, idp.RedirectURL, q.Get("redirect_uri"))
	assert.Equal(t, "the-state", q.Get("state"))
	assert.Equal(t, "the-nonce", q.Get("nonce"))
	assert.Equal(t, challenge, q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"), "only S256, never plain")
	assert.Equal(t, "openid profile email", q.Get("scope"))
	assert.NotContains(t, raw, verifier, "the verifier never leaves the server in the authorization request")
	assert.NotContains(t, raw, idp.ClientSecret)
	assert.Len(t, q["code_challenge_method"], 1)
}

func TestAuthCodeURLRefusesAnEmptyChallengeStateOrNonce(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	p := newProvider(t, idp)
	tests := []struct{ name, state, challenge, nonce string }{
		{name: "no state", challenge: "c", nonce: "n"},
		{name: "no challenge", state: "s", nonce: "n"},
		{name: "no nonce", state: "s", challenge: "c"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.AuthCodeURL(context.Background(), tt.state, tt.challenge, tt.nonce)
			require.Error(t, err)
		})
	}
}

func TestExchangeSuccess(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	p := newProvider(t, idp)

	ident, verifier, err := runLoginWithVerifier(t, p, idp, with(baseClaims(), "roles", []string{"platform-admins"}))

	require.NoError(t, err)
	assert.Equal(t, idp.Issuer, ident.Issuer)
	assert.Equal(t, "user-1", ident.Subject)
	assert.Equal(t, "alice@example.com", ident.Email)
	assert.Equal(t, "Alice Example", ident.Name)
	assert.Equal(t, "alice", ident.PreferredUsername)
	assert.Equal(t, "admin", ident.Role)

	reqs := idp.TokenRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, verifier, reqs[0].Form.Get("code_verifier"), "the PKCE verifier goes to the token endpoint")
	assert.Equal(t, "authorization_code", reqs[0].Form.Get("grant_type"))
	assert.Equal(t, idp.RedirectURL, reqs[0].Form.Get("redirect_uri"))
	assert.True(t, reqs[0].HasBasic, "client_secret_basic is the default client authentication")
	assert.Empty(t, reqs[0].Form.Get("client_secret"), "the secret is not also in the body")
}

func TestExchangeUsesClientSecretPostWhenOnlyThatIsAdvertised(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	idp.Configure(func(k *authtest.Knobs) {
		k.DiscoveryHook = func(doc map[string]any) {
			doc["token_endpoint_auth_methods_supported"] = []string{"client_secret_post"}
		}
	})
	p := newProvider(t, idp)

	_, err := runLogin(t, p, idp, baseClaims())

	require.NoError(t, err)
	reqs := idp.TokenRequests()
	require.Len(t, reqs, 1)
	assert.False(t, reqs[0].HasBasic)
	assert.Equal(t, idp.ClientSecret, reqs[0].Form.Get("client_secret"))
}

func TestExchangeSendsTheWrongVerifierToNoOne(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	p := newProvider(t, idp)
	_, challenge, err := auth.NewPKCE()
	require.NoError(t, err)
	authURL, err := p.AuthCodeURL(context.Background(), "s", challenge, testNonce)
	require.NoError(t, err)
	cb, err := url.Parse(idp.Authorize(t, authURL, baseClaims()))
	require.NoError(t, err)

	// A verifier that does not belong to the challenge: the provider refuses.
	_, err = p.Exchange(context.Background(), cb.Query().Get("code"), "a-different-verifier-value-0123456789abcdefghij", testNonce)

	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrLoginRejected)
}

func TestExchangeCodeWorksOnce(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	p := newProvider(t, idp)
	verifier, challenge, err := auth.NewPKCE()
	require.NoError(t, err)
	authURL, err := p.AuthCodeURL(context.Background(), "s", challenge, testNonce)
	require.NoError(t, err)
	cb, err := url.Parse(idp.Authorize(t, authURL, baseClaims()))
	require.NoError(t, err)
	code := cb.Query().Get("code")

	_, err = p.Exchange(context.Background(), code, verifier, testNonce)
	require.NoError(t, err)
	_, err = p.Exchange(context.Background(), code, verifier, testNonce)

	assert.ErrorIs(t, err, auth.ErrLoginRejected)
}

func TestExchangeRefusesAnEmptyCodeVerifierOrNonce(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	p := newProvider(t, idp)
	for _, tt := range []struct{ name, code, verifier, nonce string }{
		{name: "no code", verifier: "v", nonce: "n"},
		{name: "no verifier", code: "c", nonce: "n"},
		{name: "no nonce", code: "c", verifier: "v"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.Exchange(context.Background(), tt.code, tt.verifier, tt.nonce)
			assert.ErrorIs(t, err, auth.ErrLoginRejected)
		})
	}
	assert.Zero(t, idp.Hits("token"), "nothing is sent to the provider")
}

// Every ID token that fails a check is refused, and the refusal does not
// carry anything from the token.
func TestExchangeRefusesInvalidIDTokens(t *testing.T) {
	t.Parallel()
	soon := func() int64 { return time.Now().Add(time.Hour).Unix() }
	ago := func() int64 { return time.Now().Add(-time.Hour).Unix() }
	tests := []struct {
		name string
		knob func(k *authtest.Knobs)
	}{
		{name: "wrong issuer", knob: hook(func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" })},
		{name: "issuer with a trailing slash", knob: hook(func(c jwt.MapClaims) { c["iss"] = c["iss"].(string) + "/" })},
		{name: "wrong audience", knob: hook(func(c jwt.MapClaims) { c["aud"] = "another-client" })},
		{name: "audience list without the client", knob: hook(func(c jwt.MapClaims) { c["aud"] = []string{"a", "b"} })},
		{name: "several audiences without azp", knob: hook(func(c jwt.MapClaims) { c["aud"] = []string{"test-client", "other"} })},
		{name: "several audiences with a foreign azp", knob: hook(func(c jwt.MapClaims) { c["aud"] = []string{"test-client", "other"}; c["azp"] = "other" })},
		{name: "a foreign azp with one audience", knob: hook(func(c jwt.MapClaims) { c["azp"] = "other" })},
		{name: "expired", knob: hook(func(c jwt.MapClaims) { c["exp"] = ago() })},
		{name: "no exp", knob: hook(func(c jwt.MapClaims) { delete(c, "exp") })},
		{name: "not yet valid", knob: hook(func(c jwt.MapClaims) { c["nbf"] = soon() })},
		{name: "issued in the future", knob: hook(func(c jwt.MapClaims) { c["iat"] = soon() })},
		{name: "no iat", knob: hook(func(c jwt.MapClaims) { delete(c, "iat") })},
		{name: "no nonce", knob: hook(func(c jwt.MapClaims) { delete(c, "nonce") })},
		{name: "empty nonce", knob: hook(func(c jwt.MapClaims) { c["nonce"] = "" })},
		{name: "another nonce", knob: hook(func(c jwt.MapClaims) { c["nonce"] = "not-the-nonce-of-this-login" })},
		{name: "no sub", knob: hook(func(c jwt.MapClaims) { delete(c, "sub") })},
		{name: "empty sub", knob: hook(func(c jwt.MapClaims) { c["sub"] = "" })},
		{name: "sub longer than 255 characters", knob: hook(func(c jwt.MapClaims) { c["sub"] = strings.Repeat("s", 256) })},
		{name: "signed by an unknown key", knob: func(k *authtest.Knobs) { k.Signing = authtest.SignWrongKey }},
		{name: "alg none", knob: func(k *authtest.Knobs) { k.Signing = authtest.SignNone }},
		{name: "HS256 with the client secret as the key", knob: func(k *authtest.Knobs) { k.Signing = authtest.SignHS256 }},
		{name: "no id_token in the response", knob: func(k *authtest.Knobs) { k.OmitIDToken = true }},
		{name: "empty id_token in the response", knob: func(k *authtest.Knobs) { k.EmptyIDToken = true }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := authtest.New(t, "")
			idp.Configure(tt.knob)
			p := newProvider(t, idp)

			ident, err := runLogin(t, p, idp, baseClaims())

			require.Error(t, err)
			assert.Nil(t, ident)
			assert.ErrorIs(t, err, auth.ErrLoginRejected)
			assert.NotContains(t, err.Error(), idp.ClientSecret)
			assert.NotContains(t, err.Error(), "alice@example.com", "nothing from the token is in the error")
		})
	}
}

func hook(fn func(c jwt.MapClaims)) func(k *authtest.Knobs) {
	return func(k *authtest.Knobs) { k.ClaimsHook = fn }
}

func TestExchangeAcceptsAValidAudienceList(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	idp.Configure(hook(func(c jwt.MapClaims) {
		c["aud"] = []string{"other", "test-client"}
		c["azp"] = "test-client"
	}))
	p := newProvider(t, idp)

	ident, err := runLogin(t, p, idp, baseClaims())

	require.NoError(t, err)
	assert.Equal(t, "user-1", ident.Subject)
}

func TestExchangeAcceptsAnAzpThatIsTheClient(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	idp.Configure(hook(func(c jwt.MapClaims) { c["azp"] = "test-client" }))
	p := newProvider(t, idp)

	_, err := runLogin(t, p, idp, baseClaims())

	require.NoError(t, err)
}

// A token endpoint failure is reported without anything the provider sent:
// no description, no body, no client secret, no code.
func TestExchangeProviderFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		knob    func(k *authtest.Knobs)
		opts    func(o *auth.Options)
		wantErr error
	}{
		{name: "the provider rejects the code", knob: func(k *authtest.Knobs) { k.TokenStatus = http.StatusBadRequest }, wantErr: auth.ErrLoginRejected},
		{name: "the provider refuses the client credentials (401)", knob: func(k *authtest.Knobs) { k.TokenStatus = http.StatusUnauthorized }, wantErr: auth.ErrProviderUnavailable},
		{name: "the provider answers 403", knob: func(k *authtest.Knobs) { k.TokenStatus = http.StatusForbidden }, wantErr: auth.ErrLoginRejected},
		{name: "the provider answers 500", knob: func(k *authtest.Knobs) { k.TokenStatus = http.StatusInternalServerError }, wantErr: auth.ErrProviderUnavailable},
		{name: "the provider answers 503", knob: func(k *authtest.Knobs) { k.TokenStatus = http.StatusServiceUnavailable }, wantErr: auth.ErrProviderUnavailable},
		{name: "the provider is too slow", knob: func(k *authtest.Knobs) { k.TokenDelay = 3 * time.Second }, opts: func(o *auth.Options) { o.HTTPTimeout = 200 * time.Millisecond }, wantErr: auth.ErrProviderUnavailable},
		{name: "the token response is over the size limit", knob: func(k *authtest.Knobs) { k.OversizeTokenBody = true }, wantErr: auth.ErrProviderUnavailable},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := authtest.New(t, "")
			idp.Configure(tt.knob)
			opts := testOptions()
			if tt.opts != nil {
				tt.opts(&opts)
			}
			p, err := auth.NewProvider(newConfig(idp), opts)
			require.NoError(t, err)
			verifier, challenge, err := auth.NewPKCE()
			require.NoError(t, err)
			authURL, err := p.AuthCodeURL(context.Background(), "s", challenge, testNonce)
			require.NoError(t, err)
			cb, err := url.Parse(idp.Authorize(t, authURL, baseClaims()))
			require.NoError(t, err)
			code := cb.Query().Get("code")

			start := time.Now()
			_, err = p.Exchange(context.Background(), code, verifier, testNonce)

			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
			assert.Less(t, time.Since(start), 2*time.Second, "the exchange is bounded by the HTTP timeout")
			for _, secret := range []string{idp.ClientSecret, authtest.LeakMarker, code, verifier} {
				assert.NotContains(t, err.Error(), secret)
			}
		})
	}
}

func TestExchangeNeverFollowsARedirectOfTheTokenEndpoint(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(other.Close)
	idp := authtest.New(t, "")
	idp.Configure(func(k *authtest.Knobs) { k.TokenRedirectTo = other.URL + "/steal" })
	p := newProvider(t, idp)

	_, err := runLogin(t, p, idp, baseClaims())

	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrProviderUnavailable)
	assert.Zero(t, hits.Load(), "the other host got no request, so no code or secret")
}

func TestDiscoveryFailuresAreProviderErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		hook func(doc map[string]any)
	}{
		{name: "issuer differs from the configured one", hook: func(d map[string]any) { d["issuer"] = "https://evil.example.com" }},
		{name: "http token endpoint on a public host", hook: func(d map[string]any) { d["token_endpoint"] = "http://idp.example.com/token" }},
		{name: "http authorization endpoint on a public host", hook: func(d map[string]any) { d["authorization_endpoint"] = "http://idp.example.com/auth" }},
		{name: "http key set on a public host", hook: func(d map[string]any) { d["jwks_uri"] = "http://idp.example.com/jwks" }},
		{name: "missing token endpoint", hook: func(d map[string]any) { delete(d, "token_endpoint") }},
		{name: "missing authorization endpoint", hook: func(d map[string]any) { delete(d, "authorization_endpoint") }},
		{name: "missing key set", hook: func(d map[string]any) { delete(d, "jwks_uri") }},
		{name: "endpoint with credentials", hook: func(d map[string]any) { d["token_endpoint"] = "https://u:p@idp.example.com/token" }},
		{name: "oversize document", hook: func(d map[string]any) { d["pad"] = strings.Repeat("a", 2<<20) }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := authtest.New(t, "")
			idp.Configure(func(k *authtest.Knobs) { k.DiscoveryHook = tt.hook })
			p := newProvider(t, idp)

			_, err := p.AuthCodeURL(context.Background(), "s", "c", "n")

			require.Error(t, err)
			assert.ErrorIs(t, err, auth.ErrProviderUnavailable)
			assert.NotContains(t, err.Error(), "u:p")
		})
	}
}

func TestOversizeKeySetRefusesTheToken(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	idp.Configure(func(k *authtest.Knobs) { k.OversizeJWKS = true })
	p := newProvider(t, idp)

	_, err := runLogin(t, p, idp, baseClaims())

	require.Error(t, err)
}

func TestDiscoveryRequiresTLSVerification(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(tlsServer.Close)
	cfg := &config.OIDCConfig{
		Issuer: tlsServer.URL, ClientID: "c", ClientSecret: "s", RedirectURL: "https://app.example.com/cb",
		Scopes: []string{"openid"}, RoleClaim: "roles",
	}
	// No test option and no extra roots: a self-signed certificate is refused.
	p, err := auth.NewProvider(cfg, auth.Options{})
	require.NoError(t, err)

	_, err = p.AuthCodeURL(context.Background(), "s", "c", "n")

	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrProviderUnavailable)
	assert.Contains(t, err.Error(), "certificate", "the operator sees that the certificate was the problem")
	assert.Zero(t, hits.Load(), "the handshake failed before any request")
}

func TestDiscoveryHappensOnceForParallelLogins(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	p := newProvider(t, idp)

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.AuthCodeURL(context.Background(), "s", "c", "n")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, idp.Hits("discovery"))
}

func TestDiscoveryFailureIsRetriedAfterTheInterval(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	idp.Configure(func(k *authtest.Knobs) {
		k.DiscoveryHook = func(d map[string]any) { d["issuer"] = "https://evil.example.com" }
	})
	opts := testOptions()
	opts.DiscoveryRetryInterval = 150 * time.Millisecond
	p, err := auth.NewProvider(newConfig(idp), opts)
	require.NoError(t, err)

	_, err = p.AuthCodeURL(context.Background(), "s", "c", "n")
	require.ErrorIs(t, err, auth.ErrProviderUnavailable)
	_, err = p.AuthCodeURL(context.Background(), "s", "c", "n")
	require.ErrorIs(t, err, auth.ErrProviderUnavailable)
	assert.Equal(t, 1, idp.Hits("discovery"), "a failure is remembered for the interval, so a flood does not hit the provider")

	idp.Configure(func(k *authtest.Knobs) { k.DiscoveryHook = nil })
	assert.Eventually(t, func() bool {
		_, err := p.AuthCodeURL(context.Background(), "s", "c", "n")
		return err == nil
	}, 3*time.Second, 50*time.Millisecond, "the provider recovers once it answers again")
}

func TestDiscoveryUsesThePathOfTheIssuer(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "/realms/app")
	p := newProvider(t, idp)

	ident, err := runLogin(t, p, idp, baseClaims())

	require.NoError(t, err)
	assert.Equal(t, idp.Server.URL+"/realms/app", ident.Issuer)
}

func TestExchangeCancelledContext(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	p := newProvider(t, idp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Exchange(ctx, "code", "verifier", "nonce")

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "a cancelled request stays a cancel, not a provider error")
}

func TestIdentityEmail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		claims    map[string]any
		wantEmail string
	}{
		{name: "verified", claims: baseClaims(), wantEmail: "alice@example.com"},
		{name: "verified as the string true", claims: with(baseClaims(), "email_verified", "true"), wantEmail: "alice@example.com"},
		{name: "not verified", claims: with(baseClaims(), "email_verified", false)},
		{name: "verified flag missing", claims: func() map[string]any { c := baseClaims(); delete(c, "email_verified"); return c }()},
		{name: "verified as the string false", claims: with(baseClaims(), "email_verified", "false")},
		{name: "verified as a number", claims: with(baseClaims(), "email_verified", 1)},
		{name: "verified as the string yes", claims: with(baseClaims(), "email_verified", "yes")},
		{name: "not an email address", claims: with(baseClaims(), "email", "not-an-email")},
		{name: "email longer than the column", claims: with(baseClaims(), "email", strings.Repeat("a", 250)+"@example.com")},
		{name: "email is not a string", claims: with(baseClaims(), "email", 42)},
		{name: "no email", claims: func() map[string]any { c := baseClaims(); delete(c, "email"); return c }()},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := authtest.New(t, "")
			p := newProvider(t, idp)

			ident, err := runLogin(t, p, idp, tt.claims)

			require.NoError(t, err, "an unverified email is dropped, it does not fail the login")
			assert.Equal(t, tt.wantEmail, ident.Email)
		})
	}
}

func TestIdentityName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{name: "the name claim", claims: baseClaims(), want: "Alice Example"},
		{name: "given and family name", claims: with(baseClaims(), "name", "", "given_name", "Alice", "family_name", "Example"), want: "Alice Example"},
		{name: "preferred username", claims: with(baseClaims(), "name", ""), want: "alice"},
		{name: "nothing", claims: with(baseClaims(), "name", "", "preferred_username", ""), want: ""},
		{name: "control characters are removed", claims: with(baseClaims(), "name", "Al\x00ice\n\x1b[31m Ex"), want: "Alice[31m Ex"},
		{name: "long names are cut at 255 characters", claims: with(baseClaims(), "name", strings.Repeat("é", 300)), want: strings.Repeat("é", 255)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := authtest.New(t, "")
			p := newProvider(t, idp)

			ident, err := runLogin(t, p, idp, tt.claims)

			require.NoError(t, err)
			assert.Equal(t, tt.want, ident.Name)
		})
	}
}

func TestIdentityRoleMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		claims map[string]any
		mutate func(c *config.OIDCConfig)
		want   string
	}{
		{name: "no roles", claims: with(baseClaims(), "roles", []string{}), want: "user"},
		{name: "unrelated role", claims: baseClaims(), want: "user"},
		{name: "admin role", claims: with(baseClaims(), "roles", []string{"x", "platform-admins"}), want: "admin"},
		{name: "admin role in another case is not an admin", claims: with(baseClaims(), "roles", []string{"Platform-Admins"}), want: "user"},
		{name: "devops role", claims: with(baseClaims(), "roles", []string{"builders"}), want: "devops"},
		{name: "devops role in another case", claims: with(baseClaims(), "roles", []string{"BUILDERS"}), want: "user"},
		{name: "admin wins over devops", claims: with(baseClaims(), "roles", []string{"builders", "platform-admins"}), want: "admin"},
		{name: "a single string claim", claims: with(baseClaims(), "roles", "platform-admins"), want: "admin"},
		{name: "a string claim is not split", claims: with(baseClaims(), "roles", "platform-admins builders"), want: "user"},
		{name: "a prefix of an admin role", claims: with(baseClaims(), "roles", []string{"platform-admin"}), want: "user"},
		{name: "the claim is missing", claims: func() map[string]any { c := baseClaims(); delete(c, "roles"); return c }(), want: "user"},
		{name: "a claim of numbers", claims: with(baseClaims(), "roles", []int{1, 2}), want: "user"},
		{name: "numbers among the roles are ignored", claims: with(baseClaims(), "roles", []any{1, "platform-admins"}), want: "admin"},
		{name: "an object claim", claims: with(baseClaims(), "roles", map[string]any{"platform-admins": true}), want: "user"},
		{name: "empty admin roles make nobody an admin", claims: with(baseClaims(), "roles", []string{"admin", "", "platform-admins"}), mutate: func(c *config.OIDCConfig) { c.AdminRoles = nil }, want: "user"},
		{name: "an empty role name never matches an empty admin entry", claims: with(baseClaims(), "roles", []string{""}), mutate: func(c *config.OIDCConfig) { c.AdminRoles = []string{""} }, want: "user"},
		{name: "another claim name", claims: with(baseClaims(), "groups", []string{"platform-admins"}), mutate: func(c *config.OIDCConfig) { c.RoleClaim = "groups" }, want: "admin"},
		{name: "the default claim is ignored when another is configured", claims: with(baseClaims(), "roles", []string{"platform-admins"}), mutate: func(c *config.OIDCConfig) { c.RoleClaim = "groups" }, want: "user"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := authtest.New(t, "")
			var mutate []func(*config.OIDCConfig)
			if tt.mutate != nil {
				mutate = append(mutate, tt.mutate)
			}
			p := newProvider(t, idp, mutate...)

			ident, err := runLogin(t, p, idp, tt.claims)

			require.NoError(t, err)
			assert.Equal(t, tt.want, ident.Role)
		})
	}
}

func TestExternalID(t *testing.T) {
	t.Parallel()
	t.Run("is the SHA-256 of issuer, a newline and sub", func(t *testing.T) {
		t.Parallel()
		sum := sha256.Sum256([]byte("https://idp.example.com\nuser-1"))
		assert.Equal(t, hex.EncodeToString(sum[:]), auth.ExternalID("https://idp.example.com", "user-1"))
	})
	t.Run("fits the column and is stable", func(t *testing.T) {
		t.Parallel()
		long := auth.ExternalID(strings.Repeat("h", 2000), strings.Repeat("s", 255))
		assert.Len(t, long, 64)
		assert.Equal(t, long, auth.ExternalID(strings.Repeat("h", 2000), strings.Repeat("s", 255)))
	})
	t.Run("does not collide across issuers or by moving the boundary", func(t *testing.T) {
		t.Parallel()
		ids := map[string]string{}
		for _, c := range [][2]string{
			{"https://a.example.com", "x"}, {"https://b.example.com", "x"},
			{"https://a.example.com", "x"}, {"https://a.example.comx", ""},
			{"https://a.example.com\nx", ""}, {"https://a.example.com", "\nx"},
		} {
			id := auth.ExternalID(c[0], c[1])
			if prev, dup := ids[id]; dup {
				assert.Equal(t, prev, c[0]+"|"+c[1], "only the same pair may repeat")
			}
			ids[id] = c[0] + "|" + c[1]
		}
		assert.NotEqual(t, auth.ExternalID("https://a.example.com", "x"), auth.ExternalID("https://b.example.com", "x"))
	})
	t.Run("the identity of a login uses the issuer and sub", func(t *testing.T) {
		t.Parallel()
		idp := authtest.New(t, "")
		p := newProvider(t, idp)
		ident, err := runLogin(t, p, idp, baseClaims())
		require.NoError(t, err)
		assert.Equal(t, auth.ExternalID(idp.Issuer, "user-1"), ident.ExternalID())
	})
}

func TestBindingHash(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("abc"))
	assert.Equal(t, hex.EncodeToString(sum[:]), auth.BindingHash("abc"))
	assert.NotEqual(t, auth.BindingHash("abc"), auth.BindingHash("abd"))
}

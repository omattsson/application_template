package authtest_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"backend/internal/auth/authtest"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func getJSON(t *testing.T, target string) map[string]any {
	t.Helper()
	resp, err := http.Get(target) // #nosec G107 G704 -- a httptest server of this test
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func authURL(idp *authtest.IdP, challenge string) string {
	q := url.Values{
		"response_type": {"code"}, "client_id": {idp.ClientID}, "redirect_uri": {idp.RedirectURL},
		"scope": {"openid"}, "state": {"s1"}, "nonce": {"n1"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	return idp.Server.URL + "/realm/authorize?" + q.Encode()
}

func postToken(t *testing.T, idp *authtest.IdP, form url.Values, basic bool) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, idp.Server.URL+"/realm/token", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		req.SetBasicAuth(url.QueryEscape(idp.ClientID), url.QueryEscape(idp.ClientSecret))
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- a httptest server of this test
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestIdPServesDiscoveryAndKeys(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "/realm")

	doc := getJSON(t, idp.Issuer+"/.well-known/openid-configuration")
	assert.Equal(t, idp.Issuer, doc["issuer"])
	assert.Equal(t, idp.Issuer+"/token", doc["token_endpoint"])
	keys := getJSON(t, idp.Issuer+"/jwks")["keys"].([]any)
	require.Len(t, keys, 1)
	assert.Equal(t, "RSA", keys[0].(map[string]any)["kty"])
	assert.Equal(t, 1, idp.Hits("discovery"))
	assert.Equal(t, 1, idp.Hits("jwks"))
}

func TestIdPTokenEndpointChecksWhatARealProviderChecks(t *testing.T) {
	t.Parallel()
	const verifier = "a-verifier-of-at-least-forty-three-characters-0123456789"
	good := func(code string) url.Values {
		return url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://app.example.test/api/v1/auth/oidc/callback"},
			"code_verifier": {verifier},
		}
	}
	callbackCode := func(t *testing.T, idp *authtest.IdP) string {
		cb, err := url.Parse(idp.Authorize(t, authURL(idp, challengeOf(verifier)), map[string]any{"sub": "u1", "email": "a@example.com"}))
		require.NoError(t, err)
		assert.Equal(t, "s1", cb.Query().Get("state"))
		return cb.Query().Get("code")
	}

	t.Run("a good request gets a signed ID token with the nonce", func(t *testing.T) {
		t.Parallel()
		idp := authtest.New(t, "/realm")
		status, body := postToken(t, idp, good(callbackCode(t, idp)), true)
		require.Equal(t, http.StatusOK, status)
		claims := jwt.MapClaims{}
		_, _, err := jwt.NewParser().ParseUnverified(body["id_token"].(string), claims)
		require.NoError(t, err)
		assert.Equal(t, "n1", claims["nonce"])
		assert.Equal(t, "u1", claims["sub"])
		assert.Equal(t, idp.Issuer, claims["iss"])
		require.Len(t, idp.TokenRequests(), 1)
		assert.True(t, idp.TokenRequests()[0].HasBasic)
	})
	t.Run("client secret in the body works too", func(t *testing.T) {
		t.Parallel()
		idp := authtest.New(t, "/realm")
		form := good(callbackCode(t, idp))
		form.Set("client_id", idp.ClientID)
		form.Set("client_secret", idp.ClientSecret)
		status, _ := postToken(t, idp, form, false)
		assert.Equal(t, http.StatusOK, status)
	})
	t.Run("a wrong secret, verifier or redirect URI is refused", func(t *testing.T) {
		t.Parallel()
		idp := authtest.New(t, "/realm")

		form := good(callbackCode(t, idp))
		form.Set("client_id", idp.ClientID)
		form.Set("client_secret", "wrong")
		status, _ := postToken(t, idp, form, false)
		assert.Equal(t, http.StatusUnauthorized, status)

		form = good(callbackCode(t, idp))
		form.Set("code_verifier", "another-verifier-0123456789-0123456789-0123456789")
		status, _ = postToken(t, idp, form, true)
		assert.Equal(t, http.StatusBadRequest, status)

		form = good(callbackCode(t, idp))
		form.Set("redirect_uri", "https://evil.example.test/cb")
		status, _ = postToken(t, idp, form, true)
		assert.Equal(t, http.StatusBadRequest, status)
	})
	t.Run("a code works once and an unknown code never", func(t *testing.T) {
		t.Parallel()
		idp := authtest.New(t, "/realm")
		code := callbackCode(t, idp)
		status, _ := postToken(t, idp, good(code), true)
		assert.Equal(t, http.StatusOK, status)
		status, _ = postToken(t, idp, good(code), true)
		assert.Equal(t, http.StatusBadRequest, status)
		status, _ = postToken(t, idp, good("unknown"), true)
		assert.Equal(t, http.StatusBadRequest, status)
		form := good(code)
		form.Set("grant_type", "password")
		status, _ = postToken(t, idp, form, true)
		assert.Equal(t, http.StatusBadRequest, status)
	})
}

func TestIdPKnobs(t *testing.T) {
	t.Parallel()
	const verifier = "a-verifier-of-at-least-forty-three-characters-0123456789"
	run := func(t *testing.T, knob func(k *authtest.Knobs)) (int, map[string]any, string) {
		t.Helper()
		idp := authtest.New(t, "")
		idp.Configure(knob)
		q := url.Values{
			"response_type": {"code"}, "client_id": {idp.ClientID}, "redirect_uri": {idp.RedirectURL},
			"scope": {"openid"}, "state": {"s"}, "nonce": {"n"},
			"code_challenge": {challengeOf(verifier)}, "code_challenge_method": {"S256"},
		}
		cb, err := url.Parse(idp.Authorize(t, idp.Server.URL+"/authorize?"+q.Encode(), map[string]any{"sub": "u"}))
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, idp.Server.URL+"/token", strings.NewReader(url.Values{
			"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")}, "redirect_uri": {idp.RedirectURL}, "code_verifier": {verifier},
		}.Encode()))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(idp.ClientID, idp.ClientSecret)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req) // #nosec G704 -- a httptest server of this test
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out, string(raw)
	}

	t.Run("no id_token and an empty one", func(t *testing.T) {
		t.Parallel()
		_, body, _ := run(t, func(k *authtest.Knobs) { k.OmitIDToken = true })
		assert.NotContains(t, body, "id_token")
		_, body, _ = run(t, func(k *authtest.Knobs) { k.EmptyIDToken = true })
		assert.Equal(t, "", body["id_token"])
	})
	t.Run("forced status carries the marker", func(t *testing.T) {
		t.Parallel()
		status, _, raw := run(t, func(k *authtest.Knobs) { k.TokenStatus = http.StatusServiceUnavailable })
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.Contains(t, raw, authtest.LeakMarker)
	})
	t.Run("redirect and oversize", func(t *testing.T) {
		t.Parallel()
		status, _, _ := run(t, func(k *authtest.Knobs) { k.TokenRedirectTo = "http://127.0.0.1:1/" })
		assert.Equal(t, http.StatusFound, status)
		_, _, raw := run(t, func(k *authtest.Knobs) { k.OversizeTokenBody = true })
		assert.Greater(t, len(raw), 1<<20)
	})
	t.Run("signing choices", func(t *testing.T) {
		t.Parallel()
		for _, signing := range []string{authtest.SignHS256, authtest.SignNone, authtest.SignWrongKey, authtest.SignRS256} {
			_, body, _ := run(t, func(k *authtest.Knobs) { k.Signing = signing })
			token, _ := body["id_token"].(string)
			assert.Equal(t, 3, len(strings.Split(token, ".")), signing)
		}
	})
	t.Run("a delay", func(t *testing.T) {
		t.Parallel()
		status, _, _ := run(t, func(k *authtest.Knobs) { k.TokenDelay = 10 })
		assert.Equal(t, http.StatusOK, status)
	})
}

func TestIdPAuthorizeJWKSOversize(t *testing.T) {
	t.Parallel()
	idp := authtest.New(t, "")
	idp.Configure(func(k *authtest.Knobs) { k.OversizeJWKS = true })
	resp, err := http.Get(idp.Issuer + "/jwks") // #nosec G107 G704 -- a httptest server of this test
	require.NoError(t, err)
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	assert.Greater(t, n, int64(1<<20))
}

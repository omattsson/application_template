// Package authtest is a fake OpenID Connect identity provider for tests. It
// serves the discovery document, the key set (JWKS), the authorization
// endpoint (as a helper the test calls in place of the browser) and the token
// endpoint, on a httptest server. It implements only what the login needs and
// checks what a real provider checks (client authentication, redirect URI,
// the PKCE verifier), so a test that passes here fails when the code under
// test sends a wrong request.
//
// It is test support, not production code: nothing outside tests imports it.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Signing choices of the ID token (Knobs.Signing).
const (
	SignRS256      = ""          // the key of the key set (default)
	SignWrongKey   = "wrong-key" // RS256 with a key that is not in the key set
	SignHS256      = "hs256"     // HS256 with the client secret as the key
	SignNone       = "none"      // alg none, no signature
	keyID          = "test-key-1"
	codeRandBytes  = 16
	defaultTTL     = 5 * time.Minute
	maxFormBytes   = 1 << 16
	oversizeFiller = 2 << 20
)

// Knobs are the behaviours a test can change. Change them with IdP.Configure.
type Knobs struct {
	// ClaimsHook edits the claims of the next ID tokens before they are signed.
	ClaimsHook func(claims jwt.MapClaims)
	// DiscoveryHook edits the discovery document before it is served.
	DiscoveryHook func(doc map[string]any)
	// Signing is one of the Sign* constants.
	Signing string
	// TokenStatus, when not 0, makes the token endpoint answer with this
	// status and an OAuth error body (with a description that contains
	// LeakMarker, so a test can check it is never reflected).
	TokenStatus int
	// TokenRedirectTo, when set, makes the token endpoint answer 302 to it.
	TokenRedirectTo string
	// TokenDelay delays the token endpoint.
	TokenDelay time.Duration
	// OmitIDToken leaves id_token out of the token response; EmptyIDToken
	// sends an empty string.
	OmitIDToken  bool
	EmptyIDToken bool
	// OversizeTokenBody makes the token response larger than any limit.
	OversizeTokenBody bool
	// OversizeJWKS makes the key set response larger than any limit.
	OversizeJWKS bool
}

// LeakMarker is a marker that the fake provider puts in its error
// descriptions. Code under test must not reflect it to a browser.
const LeakMarker = "idp-internal-detail-7f3a"

// IdP is a running fake identity provider.
type IdP struct {
	Server       *httptest.Server
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// Issuer is the issuer URL: the server URL plus the path given to New.
	Issuer string

	key      *rsa.PrivateKey
	path     string
	mu       sync.Mutex
	knobs    Knobs
	codes    map[string]*grant
	requests []TokenRequest
	hits     map[string]int
}

// grant is an issued authorization code.
type grant struct {
	claims    jwt.MapClaims
	challenge string
	nonce     string
	redirect  string
}

// TokenRequest is a request that reached the token endpoint.
type TokenRequest struct {
	Form      url.Values
	BasicUser string
	BasicPass string
	HasBasic  bool
}

var (
	sharedKey      = sync.OnceValue(mustKey)
	sharedWrongKey = sync.OnceValue(mustKey)
)

func mustKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

// New starts a fake provider whose issuer is the server URL plus path ("" or
// for example "/realms/app"). It closes with the test.
func New(t *testing.T, path string) *IdP {
	t.Helper()
	idp := &IdP{
		ClientID:     "test-client",
		ClientSecret: "test-client-secret-9d2f",
		RedirectURL:  "https://app.example.test/api/v1/auth/oidc/callback",
		key:          sharedKey(),
		path:         path,
		codes:        map[string]*grant{},
		hits:         map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path+"/.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc(path+"/jwks", idp.jwks)
	mux.HandleFunc(path+"/token", idp.token)
	idp.Server = httptest.NewServer(idp.counting(mux))
	t.Cleanup(idp.Server.Close)
	idp.Issuer = idp.Server.URL + path
	return idp
}

// Configure changes the knobs under the lock of the server.
func (i *IdP) Configure(fn func(k *Knobs)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	fn(&i.knobs)
}

// Hits returns the number of requests the endpoint got ("discovery", "jwks",
// "token").
func (i *IdP) Hits(endpoint string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.hits[endpoint]
}

// TokenRequests returns the requests that reached the token endpoint.
func (i *IdP) TokenRequests() []TokenRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]TokenRequest(nil), i.requests...)
}

func (i *IdP) counting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := ""
		switch strings.TrimPrefix(r.URL.Path, i.path) {
		case "/.well-known/openid-configuration":
			name = "discovery"
		case "/jwks":
			name = "jwks"
		case "/token":
			name = "token"
		}
		i.mu.Lock()
		i.hits[name]++
		i.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (i *IdP) snapshot() Knobs {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.knobs
}

func (i *IdP) discovery(w http.ResponseWriter, _ *http.Request) {
	k := i.snapshot()
	doc := map[string]any{
		"issuer":                                i.Issuer,
		"authorization_endpoint":                i.Server.URL + i.path + "/authorize",
		"token_endpoint":                        i.Server.URL + i.path + "/token",
		"jwks_uri":                              i.Server.URL + i.path + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	if k.DiscoveryHook != nil {
		k.DiscoveryHook(doc)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (i *IdP) jwks(w http.ResponseWriter, _ *http.Request) {
	k := i.snapshot()
	w.Header().Set("Content-Type", "application/json")
	if k.OversizeJWKS {
		_, _ = w.Write([]byte(`{"keys":[],"pad":"` + strings.Repeat("a", oversizeFiller) + `"}`))
		return
	}
	pub := &i.key.PublicKey
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": keyID,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// Authorize plays the browser and the user: it checks the authorization
// request that authURL is and returns the callback URL that the provider
// redirects to, carrying a one-time code and the state of the request. claims
// are added to the ID token (sub is required). The test fails on a request
// that a real provider would refuse.
func (i *IdP) Authorize(t *testing.T, authURL string, claims map[string]any) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorization URL: %v", err)
	}
	if !strings.HasPrefix(authURL, i.Server.URL+i.path+"/authorize") {
		t.Fatalf("authorization URL does not point at the provider: %s", authURL)
	}
	q := u.Query()
	check := func(key, want string) {
		if got := q.Get(key); got != want {
			t.Fatalf("authorization request %s = %q, want %q", key, got, want)
		}
	}
	check("response_type", "code")
	check("client_id", i.ClientID)
	check("redirect_uri", i.RedirectURL)
	check("code_challenge_method", "S256")
	if q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("authorization request lacks code_challenge, state or nonce: %v", q)
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		t.Fatalf("authorization request scope lacks openid: %q", q.Get("scope"))
	}
	if _, ok := claims["sub"]; !ok {
		t.Fatalf("claims need a sub")
	}

	code := randomCode()
	c := jwt.MapClaims{}
	for k, v := range claims {
		c[k] = v
	}
	i.mu.Lock()
	i.codes[code] = &grant{claims: c, challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri")}
	i.mu.Unlock()

	cb, err := url.Parse(i.RedirectURL)
	if err != nil {
		t.Fatalf("redirect URL: %v", err)
	}
	cq := cb.Query()
	cq.Set("code", code)
	cq.Set("state", q.Get("state"))
	cb.RawQuery = cq.Encode()
	return cb.String()
}

func randomCode() string {
	b := make([]byte, codeRandBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (i *IdP) token(w http.ResponseWriter, r *http.Request) {
	k := i.snapshot()
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	user, pass, hasBasic := r.BasicAuth()
	i.mu.Lock()
	i.requests = append(i.requests, TokenRequest{Form: cloneValues(r.PostForm), BasicUser: user, BasicPass: pass, HasBasic: hasBasic})
	i.mu.Unlock()

	if k.TokenDelay > 0 {
		select {
		case <-time.After(k.TokenDelay):
		case <-r.Context().Done():
			return
		}
	}
	if k.TokenRedirectTo != "" {
		http.Redirect(w, r, k.TokenRedirectTo, http.StatusFound)
		return
	}
	if k.TokenStatus != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(k.TokenStatus)
		_, _ = fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"%s"}`, LeakMarker)
		return
	}

	if !i.clientAuthenticated(r, user, pass, hasBasic) {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	i.mu.Lock()
	g, ok := i.codes[r.PostForm.Get("code")]
	delete(i.codes, r.PostForm.Get("code")) // a code works once
	i.mu.Unlock()
	if !ok || g.redirect != r.PostForm.Get("redirect_uri") {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}

	resp := map[string]any{"access_token": "opaque-access-token", "token_type": "Bearer", "expires_in": 300}
	switch {
	case k.OmitIDToken:
	case k.EmptyIDToken:
		resp["id_token"] = ""
	default:
		idt, err := i.signIDToken(k, g)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error")
			return
		}
		resp["id_token"] = idt
	}
	if k.OversizeTokenBody {
		resp["pad"] = strings.Repeat("a", oversizeFiller)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func (i *IdP) clientAuthenticated(r *http.Request, user, pass string, hasBasic bool) bool {
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if hasBasic {
		id, secret = user, pass
		if u, err := url.QueryUnescape(user); err == nil {
			id = u
		}
		if p, err := url.QueryUnescape(pass); err == nil {
			secret = p
		}
	}
	return id == i.ClientID && subtle.ConstantTimeCompare([]byte(secret), []byte(i.ClientSecret)) == 1
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
}

func (i *IdP) signIDToken(k Knobs, g *grant) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   i.Issuer,
		"aud":   i.ClientID,
		"exp":   now.Add(defaultTTL).Unix(),
		"iat":   now.Unix(),
		"nonce": g.nonce,
	}
	for key, v := range g.claims {
		claims[key] = v
	}
	if k.ClaimsHook != nil {
		k.ClaimsHook(claims)
	}
	switch k.Signing {
	case SignHS256:
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		tok.Header["kid"] = keyID
		return tok.SignedString([]byte(i.ClientSecret))
	case SignNone:
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
		return tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	case SignWrongKey:
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = keyID
		return tok.SignedString(sharedWrongKey())
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = keyID
	return tok.SignedString(i.key)
}

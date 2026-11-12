// Package auth is the OpenID Connect (OIDC) client of the login: discovery,
// the authorization URL with PKCE, the code exchange and the validation of the
// ID token. It does not touch users, sessions or HTTP handlers; the OIDC
// handler uses it and owns those.
//
// What the package guarantees:
//   - Every request to the identity provider goes through one HTTP client with
//     certificate verification, a timeout, a response size limit and no
//     redirects. The issuer, and every endpoint of its discovery document,
//     must be https; plain http is accepted only for a loopback host and only
//     when a test sets Options.AllowInsecureLoopbackForTesting (there is no
//     setting for it in the configuration).
//   - PKCE uses S256 only. State, nonce and verifier come from crypto/rand.
//   - An ID token is accepted only when its signature verifies with a key of
//     the provider's key set (RS*, PS*, ES* and EdDSA; never none, never HS*),
//     its issuer is exactly the configured issuer, its audience contains the
//     client ID (and azp is the client ID when it is present or when there
//     are several audiences), exp and iat are valid, and its nonce is the
//     nonce of the login.
//   - The identity of an account is the issuer plus sub (Identity.ExternalID).
//     An email address is used only when email_verified is true.
//   - Errors never contain the client secret, the code, the verifier, a
//     response body or a claim; they have a kind (ErrLoginRejected or
//     ErrProviderUnavailable) and a short fixed description.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"backend/internal/config"
	"backend/internal/ctxutil"
	"backend/internal/models"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Errors of the provider. Every error that comes out of Provider wraps one of
// these (a cancelled context comes out as the context error instead).
var (
	// ErrLoginRejected: the provider or the ID token refused this login (the
	// code was refused, or a check of the ID token failed). The user can try
	// again; nothing is wrong with the provider itself.
	ErrLoginRejected = errors.New("oidc login rejected")
	// ErrProviderUnavailable: the provider could not be used (it did not
	// answer, answered with a server error or something unreadable, or its
	// discovery document is not acceptable).
	ErrProviderUnavailable = errors.New("oidc provider unavailable")
)

// Error is an error of the provider with a fixed, safe description. Detail
// never contains a secret, a code, a response body or a claim.
type Error struct {
	Kind   error
	Detail string
	// Misconfigured marks an ErrProviderUnavailable that is a problem of this
	// server's configuration (the provider refuses the client credentials),
	// not an outage of the provider: an operator must act, so the handler logs
	// it at Error level.
	Misconfigured bool
}

func (e *Error) Error() string { return e.Kind.Error() + ": " + e.Detail }
func (e *Error) Unwrap() error { return e.Kind }

func rejected(detail string) error    { return &Error{Kind: ErrLoginRejected, Detail: detail} }
func unavailable(detail string) error { return &Error{Kind: ErrProviderUnavailable, Detail: detail} }

// misconfigured is an unavailable provider whose cause is the configuration of
// this server (the client credentials).
func misconfigured(detail string) error {
	return &Error{Kind: ErrProviderUnavailable, Detail: detail, Misconfigured: true}
}

// Limits and defaults.
const (
	defaultHTTPTimeout            = 10 * time.Second
	defaultDiscoveryRetryInterval = 5 * time.Second
	// iatLeeway is the clock difference allowed between the provider and this
	// server for an ID token that is issued "in the future".
	iatLeeway = time.Minute
	// maxSubjectLen is the limit of OIDC Core for sub (255 ASCII characters).
	maxSubjectLen = 255
	// maxTextLen limits the display name and preferred username (the size of
	// the users columns).
	maxTextLen = 255
	// maxEmailLen is the size of users.email.
	maxEmailLen = 255
	// maxRoleValues limits the roles read from the claim.
	maxRoleValues = 200
	// randomTokenBytes is the size of state, nonce, PKCE verifier and browser
	// binding values: 256 bits.
	randomTokenBytes = 32
)

// allowedSigningAlgs is the allow-list of ID token signature algorithms. HS*
// (a key derived from the client secret) and none are never allowed.
var allowedSigningAlgs = []string{
	oidc.RS256, oidc.RS384, oidc.RS512,
	oidc.PS256, oidc.PS384, oidc.PS512,
	oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.EdDSA,
}

// Options of NewProvider. The zero value is the production setting.
type Options struct {
	// AllowInsecureLoopbackForTesting allows an http issuer and http endpoints
	// when their host is a loopback address or localhost. It exists for tests
	// with httptest servers. Production code must not set it, and there is no
	// environment variable for it.
	AllowInsecureLoopbackForTesting bool
	// HTTPTimeout is the total time of one request to the provider (default
	// 10s).
	HTTPTimeout time.Duration
	// DiscoveryRetryInterval is how long a failed discovery is remembered, so
	// a flood of logins does not become a flood of requests to a provider that
	// is down (default 5s).
	DiscoveryRetryInterval time.Duration
}

// Identity is what a successful login proves. Issuer and Subject identify the
// account; the other fields are data about it.
type Identity struct {
	// Issuer is the issuer of the ID token (the configured issuer).
	Issuer string
	// Subject is the sub claim: stable for the user at this issuer.
	Subject string
	// Email is the email address of the user when the provider says it is
	// verified (email_verified true) and it is well formed; otherwise empty.
	// It identifies nothing: never use it to find or link an account.
	Email string
	// Name is the display name: name, else given and family name, else the
	// preferred username. Control characters are removed.
	Name string
	// PreferredUsername is the preferred_username claim, cleaned.
	PreferredUsername string
	// Role is the role of the user in this application, from the role claim
	// and the configured admin and devops role names.
	Role string
}

// ExternalID returns the value that identifies the account in
// users.external_id.
func (i *Identity) ExternalID() string { return ExternalID(i.Issuer, i.Subject) }

// ExternalID returns the stable, collision-free key of an identity: the
// SHA-256 (hex, 64 characters) of the issuer, a newline and the subject. The
// issuer is part of it, so the same sub at two issuers gives two accounts; a
// digest fits the 255 character column whatever the length of the issuer and
// sub, and a newline cannot be part of an issuer URL, so the pair is
// unambiguous.
func ExternalID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\n" + subject))
	return hex.EncodeToString(sum[:])
}

// BindingHash returns the SHA-256 (hex) of a browser binding value, the form
// in which the login state stores it.
func BindingHash(binding string) string {
	sum := sha256.Sum256([]byte(binding))
	return hex.EncodeToString(sum[:])
}

// RandomToken returns 256 random bits from crypto/rand as base64url without
// padding (43 characters), for a state, a nonce or a browser binding value.
func RandomToken() (string, error) {
	buf := make([]byte, randomTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewPKCE returns a PKCE code verifier (43 characters, 256 random bits) and
// its S256 challenge: base64url(SHA-256(verifier)). The plain method is not
// supported.
func NewPKCE() (verifier, challenge string, err error) {
	verifier, err = RandomToken()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// Provider is the OIDC client for one identity provider. It is safe for
// concurrent use. NewProvider does not contact the provider: discovery runs on
// the first login, so a provider that is down at start does not stop the
// application.
type Provider struct {
	cfg      config.OIDCConfig
	client   *http.Client
	lastFail time.Time
	lastErr  error
	disc     *discovered
	opts     Options
	mu       sync.Mutex
}

// discovered is the result of a successful discovery.
type discovered struct {
	oauth2   *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewProvider checks cfg and returns a Provider. The issuer must be https
// (see Options). The error never contains the client secret or the value of a
// URL.
func NewProvider(cfg *config.OIDCConfig, opts Options) (*Provider, error) {
	if cfg == nil {
		return nil, errors.New("oidc: no configuration")
	}
	if !validEndpointURL(cfg.Issuer, opts.AllowInsecureLoopbackForTesting, false) {
		return nil, errors.New("oidc: issuer must be an https URL with a host, without credentials, query or fragment")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("oidc: client id is required")
	}
	if cfg.ClientSecret == "" {
		return nil, errors.New("oidc: client secret is required")
	}
	if strings.TrimSpace(cfg.RedirectURL) == "" {
		return nil, errors.New("oidc: redirect url is required")
	}
	if opts.HTTPTimeout <= 0 {
		opts.HTTPTimeout = defaultHTTPTimeout
	}
	if opts.DiscoveryRetryInterval <= 0 {
		opts.DiscoveryRetryInterval = defaultDiscoveryRetryInterval
	}
	return &Provider{cfg: *cfg, opts: opts, client: newHTTPClient(opts.HTTPTimeout)}, nil
}

// validEndpointURL reports whether raw is an https URL with a host and no
// credentials or fragment (and no query unless allowQuery). With loopbackHTTP,
// an http URL on a loopback host or localhost is also accepted.
func validEndpointURL(raw string, loopbackHTTP, allowQuery bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if !allowQuery && (u.RawQuery != "" || u.ForceQuery) {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return loopbackHTTP && isLoopbackHost(u.Hostname())
	}
	return false
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// discover returns the discovery result, running discovery when there is none
// yet. One discovery runs at a time (parallel logins wait for it). A failure
// is remembered for DiscoveryRetryInterval. The discovery is not tied to the
// request that triggered it: a client that leaves does not make it fail for the
// others.
func (p *Provider) discover(ctx context.Context) (*discovered, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disc != nil {
		return p.disc, nil
	}
	if p.lastErr != nil && time.Since(p.lastFail) < p.opts.DiscoveryRetryInterval {
		return nil, p.lastErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dctx, cancel := ctxutil.Detach(ctx, p.opts.HTTPTimeout)
	defer cancel()
	d, err := p.runDiscovery(oidc.ClientContext(dctx, p.client))
	if err != nil {
		p.lastErr, p.lastFail = err, time.Now()
		return nil, err
	}
	p.disc, p.lastErr = d, nil
	return d, nil
}

func (p *Provider) runDiscovery(ctx context.Context) (*discovered, error) {
	prov, err := oidc.NewProvider(ctx, p.cfg.Issuer)
	if err != nil {
		return nil, unavailable("discovery failed: " + transportReason(err))
	}
	var meta struct {
		JWKSURI     string   `json:"jwks_uri"`
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	if err := prov.Claims(&meta); err != nil {
		return nil, unavailable("discovery document is not readable")
	}
	endpoint := prov.Endpoint()
	loopback := p.opts.AllowInsecureLoopbackForTesting
	for name, raw := range map[string]string{
		"authorization_endpoint": endpoint.AuthURL,
		"token_endpoint":         endpoint.TokenURL,
		"jwks_uri":               meta.JWKSURI,
	} {
		if !validEndpointURL(raw, loopback, true) {
			return nil, unavailable("discovery document has no acceptable " + name)
		}
	}

	endpoint.AuthStyle = authStyle(meta.AuthMethods)
	return &discovered{
		oauth2: &oauth2.Config{
			ClientID:     p.cfg.ClientID,
			ClientSecret: p.cfg.ClientSecret,
			RedirectURL:  p.cfg.RedirectURL,
			Endpoint:     endpoint,
			Scopes:       p.cfg.Scopes,
		},
		verifier: prov.Verifier(&oidc.Config{ClientID: p.cfg.ClientID, SupportedSigningAlgs: allowedSigningAlgs}),
	}, nil
}

// authStyle picks how the client authenticates at the token endpoint:
// client_secret_basic (the default of the specification) unless the provider
// supports only client_secret_post.
func authStyle(methods []string) oauth2.AuthStyle {
	if len(methods) > 0 && !slices.Contains(methods, "client_secret_basic") && slices.Contains(methods, "client_secret_post") {
		return oauth2.AuthStyleInParams
	}
	return oauth2.AuthStyleInHeader
}

// AuthCodeURL returns the authorization URL to redirect the browser to: the
// authorization code flow with PKCE (S256 challenge only), the state and the
// nonce. The state, the challenge and the nonce are required.
func (p *Provider) AuthCodeURL(ctx context.Context, state, challenge, nonce string) (string, error) {
	if state == "" || challenge == "" || nonce == "" {
		return "", errors.New("oidc: state, code challenge and nonce are required")
	}
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	return d.oauth2.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("nonce", nonce),
	), nil
}

// Exchange trades the authorization code for tokens (sending the PKCE
// verifier), validates the ID token against the nonce of the login and returns
// the identity. See the package documentation for the checks. A cancelled ctx
// returns its error.
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (*Identity, error) {
	if code == "" || verifier == "" || nonce == "" {
		return nil, rejected("code, verifier and nonce are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}

	octx := oidc.ClientContext(ctx, p.client)
	token, err := d.oauth2.Exchange(octx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, exchangeError(err)
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		return nil, rejected("the token response has no id_token")
	}
	idToken, err := d.verifier.Verify(octx, raw)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, rejected("id_token verification failed")
	}
	return p.identityOf(idToken, nonce)
}

// identityOf runs the checks that the library does not, and builds the
// identity from the claims of a verified ID token.
func (p *Provider) identityOf(idToken *oidc.IDToken, nonce string) (*Identity, error) {
	if idToken.Subject == "" || utf8.RuneCountInString(idToken.Subject) > maxSubjectLen {
		return nil, rejected("id_token has no usable sub")
	}
	if idToken.IssuedAt.IsZero() || idToken.IssuedAt.After(time.Now().Add(iatLeeway)) {
		return nil, rejected("id_token has no valid iat")
	}
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return nil, rejected("id_token nonce does not match")
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, rejected("id_token claims are not readable")
	}
	// The verifier checked that the audience contains the client ID. With
	// several audiences, or an azp claim, the authorized party must be this
	// client too (OIDC Core, 3.1.3.7 rules 4 and 5).
	if azp, present := claims["azp"]; present {
		if s, _ := azp.(string); s != p.cfg.ClientID {
			return nil, rejected("id_token azp is not the client")
		}
	} else if len(idToken.Audience) > 1 {
		return nil, rejected("id_token has several audiences and no azp")
	}

	ident := &Identity{
		Issuer:            idToken.Issuer,
		Subject:           idToken.Subject,
		PreferredUsername: cleanText(claimString(claims, "preferred_username"), maxTextLen),
		Role:              mapRole(p.cfg.AdminRoles, p.cfg.DevOpsRoles, claimStrings(claims, p.cfg.RoleClaim)),
	}
	if emailVerified(claims) {
		ident.Email = validEmail(claimString(claims, "email"))
	}
	ident.Name = cleanText(claimString(claims, "name"), maxTextLen)
	if ident.Name == "" {
		given := cleanText(claimString(claims, "given_name"), maxTextLen)
		family := cleanText(claimString(claims, "family_name"), maxTextLen)
		ident.Name = cleanText(given+" "+family, maxTextLen)
	}
	if ident.Name == "" {
		ident.Name = ident.PreferredUsername
	}
	return ident, nil
}

// exchangeError classifies an error of the token request. It uses only the
// HTTP status and the OAuth error code (a short token), never the text of the
// error: that can contain the response body, with the description of the
// provider.
func exchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.Response != nil {
		status := re.Response.StatusCode
		// The provider refuses this server's client credentials (a wrong or
		// rotated secret, a client that may not use the code flow): every user
		// fails the same way until an operator fixes the configuration, so it
		// is not a rejected login.
		if status == http.StatusUnauthorized || re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client" {
			return misconfigured(fmt.Sprintf("the token endpoint refused the client credentials (status %d, error %s)", status, safeCode(re.ErrorCode)))
		}
		if status >= 400 && status < 500 {
			return rejected(fmt.Sprintf("the token endpoint refused the request (status %d, error %s)", status, safeCode(re.ErrorCode)))
		}
		return unavailable(fmt.Sprintf("the token endpoint answered status %d", status))
	}
	return unavailable("the token request failed: " + transportReason(err))
}

// safeCode returns an OAuth error code when it is a short token of letters,
// digits and underscores, and "unknown" otherwise.
func safeCode(code string) string {
	if code == "" || len(code) > 64 {
		return "unknown"
	}
	for _, r := range code {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return "unknown"
		}
	}
	return code
}

// transportReason names the kind of a failed request without the URL or the
// text of the error.
func transportReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		netErr           net.Error
	)
	switch {
	case errors.Is(err, errResponseTooLarge):
		return "response too large"
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid):
		return "the certificate of the provider is not valid"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "connection or response error"
}

// mapRole returns the application role for the roles of the claim: admin when
// one of them is exactly (case-sensitive) an admin role name, devops when one
// is exactly a devops role name, else user. An empty list of names makes
// nobody an admin (or devops).
func mapRole(adminRoles, devopsRoles, roles []string) string {
	has := func(names []string) bool {
		for _, r := range roles {
			if r != "" && slices.Contains(names, r) {
				return true
			}
		}
		return false
	}
	switch {
	case has(adminRoles):
		return models.RoleAdmin
	case has(devopsRoles):
		return models.RoleDevOps
	}
	return models.RoleUser
}

// claimStrings returns the string values of a claim that is a string or a list
// of strings; other values are ignored.
func claimStrings(claims map[string]any, name string) []string {
	switch v := claims[name].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, min(len(v), maxRoleValues))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
				if len(out) == maxRoleValues {
					break
				}
			}
		}
		return out
	}
	return nil
}

func claimString(claims map[string]any, name string) string {
	s, _ := claims[name].(string)
	return s
}

// emailVerified reports whether the email_verified claim says true. It is the
// boolean true, or the string "true" (some providers send it that way);
// nothing else counts.
func emailVerified(claims map[string]any) bool {
	switch v := claims["email_verified"].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// validEmail returns email when it is well formed and fits the column.
func validEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" || utf8.RuneCountInString(email) > maxEmailLen || !models.IsValidEmail(email) {
		return ""
	}
	return email
}

// cleanText removes control characters, trims spaces and cuts s to max
// characters.
func cleanText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit])
	}
	return s
}

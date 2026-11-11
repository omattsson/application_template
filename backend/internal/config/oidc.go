package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds of the OIDC settings.
const (
	// MinOIDCStateTTL and MaxOIDCStateTTL bound OIDC_STATE_TTL: the time a
	// user has to finish the login at the identity provider. A short window
	// limits the use of a leaked authorization URL; a very short one fails a
	// slow login (a password manager, a second factor).
	MinOIDCStateTTL = 30 * time.Second
	MaxOIDCStateTTL = time.Hour

	defaultOIDCStateTTL     = 5 * time.Minute
	defaultOIDCRoleClaim    = "roles"
	defaultOIDCFrontendPath = "/auth/callback"
	defaultOIDCScopes       = "openid,profile,email"

	// maxOIDCRoleClaimLen and maxOIDCRoleNameLen are generous limits for a
	// claim name and a role name; they keep a typo from becoming a huge value.
	maxOIDCRoleClaimLen = 100
	maxOIDCRoleNameLen  = 200
)

// oidcFrontendPathPattern is a path that is safe to put in a Location header:
// it starts with one slash and has only URL path characters.
var oidcFrontendPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@/%-]*$`)

// OIDCConfig holds the OpenID Connect settings. OIDC is off by default
// (OIDC_ENABLED=false). It also needs AUTH_ENABLED=true: without
// authentication the OIDC routes answer 404 and these settings are not
// validated.
//
//nolint:govet // Field order follows the documentation of the variables
type OIDCConfig struct {
	// StateTTL is OIDC_STATE_TTL (default 5m, MinOIDCStateTTL to
	// MaxOIDCStateTTL): how long a started login stays valid.
	StateTTL time.Duration
	// Issuer is OIDC_ISSUER: the issuer URL of the identity provider. It must
	// be an https URL and is compared exactly with the issuer of the discovery
	// document and of every ID token.
	Issuer string
	// ClientID is OIDC_CLIENT_ID.
	ClientID string
	// ClientSecret is OIDC_CLIENT_SECRET. It has no default and is never
	// logged or put in an error.
	ClientSecret string
	// RedirectURL is OIDC_REDIRECT_URL: the callback URL registered at the
	// identity provider, for example
	// https://app.example.com/api/v1/auth/oidc/callback.
	RedirectURL string
	// RoleClaim is OIDC_ROLE_CLAIM (default "roles"): the ID token claim with
	// the roles or groups of the user (a string or a list of strings).
	RoleClaim string
	// FrontendPath is OIDC_FRONTEND_PATH (default /auth/callback): the fixed
	// path of the web UI that the callback redirects to after a successful
	// login. It is never taken from a request.
	FrontendPath string
	// Scopes is OIDC_SCOPES (default openid,profile,email); it must include
	// openid.
	Scopes []string
	// AdminRoles is OIDC_ADMIN_ROLES: role names (exact, case-sensitive) that
	// make an OIDC user an admin. Empty: nobody is an admin through OIDC.
	AdminRoles []string
	// DevOpsRoles is OIDC_DEVOPS_ROLES: role names (exact, case-sensitive)
	// that make an OIDC user a devops user. An admin role wins.
	DevOpsRoles []string
	// Enabled turns on the OIDC login (OIDC_ENABLED).
	Enabled bool
	// LocalAuth is OIDC_LOCAL_AUTH (default false): keep the password login
	// open to every local user while OIDC is enabled. With false only service
	// accounts (break-glass) may sign in with a password when Enabled is true.
	LocalAuth bool
}

// loadOIDCConfig reads the OIDC variables. With authentication on, the
// booleans and the duration are parsed strictly: a typo in OIDC_ENABLED or
// OIDC_LOCAL_AUTH must not silently change who can sign in how. With auth off
// nothing uses them and they are read leniently.
func loadOIDCConfig(authEnabled bool) (OIDCConfig, error) {
	cfg := OIDCConfig{
		Issuer:       strings.TrimSpace(getEnv("OIDC_ISSUER", "")),
		ClientID:     strings.TrimSpace(getEnv("OIDC_CLIENT_ID", "")),
		ClientSecret: getEnv("OIDC_CLIENT_SECRET", ""),
		RedirectURL:  strings.TrimSpace(getEnv("OIDC_REDIRECT_URL", "")),
		RoleClaim:    strings.TrimSpace(getEnv("OIDC_ROLE_CLAIM", defaultOIDCRoleClaim)),
		FrontendPath: strings.TrimSpace(getEnv("OIDC_FRONTEND_PATH", defaultOIDCFrontendPath)),
		Scopes:       splitList(getEnv("OIDC_SCOPES", defaultOIDCScopes)),
		AdminRoles:   splitList(getEnv("OIDC_ADMIN_ROLES", "")),
		DevOpsRoles:  splitList(getEnv("OIDC_DEVOPS_ROLES", "")),
	}

	if !authEnabled {
		cfg.Enabled = getEnvBool("OIDC_ENABLED", false)
		cfg.LocalAuth = getEnvBool("OIDC_LOCAL_AUTH", false)
		cfg.StateTTL = getEnvDuration("OIDC_STATE_TTL", defaultOIDCStateTTL)

		return cfg, nil
	}

	var err error
	if cfg.Enabled, err = parseEnvBoolStrict("OIDC_ENABLED", false); err != nil {
		return OIDCConfig{}, err
	}

	if cfg.LocalAuth, err = parseEnvBoolStrict("OIDC_LOCAL_AUTH", false); err != nil {
		return OIDCConfig{}, err
	}

	if cfg.StateTTL, err = parseEnvDurationStrict("OIDC_STATE_TTL", defaultOIDCStateTTL); err != nil {
		return OIDCConfig{}, err
	}

	return cfg, nil
}

// splitList splits a comma or whitespace separated list, trimming every
// element and dropping empty ones. The case of an element is kept.
func splitList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(parts) == 0 {
		return nil
	}

	return parts
}

// Validate checks the OIDC settings. Config.Validate calls it only when both
// AUTH_ENABLED and OIDC_ENABLED are true. The messages name the variable and
// never repeat a value, so the client secret and any credentials inside a URL
// stay out of the logs.
func (c *OIDCConfig) Validate() error {
	if c.Issuer == "" {
		return errors.New("OIDC_ISSUER is required when OIDC is enabled")
	}

	if !isHTTPSURLWithoutExtras(c.Issuer) {
		return errors.New("OIDC_ISSUER must be an https URL with a host, without credentials, query or fragment")
	}

	if strings.TrimSpace(c.ClientID) == "" {
		return errors.New("OIDC_CLIENT_ID is required when OIDC is enabled")
	}

	if c.ClientSecret == "" {
		return errors.New("OIDC_CLIENT_SECRET is required when OIDC is enabled")
	}

	if c.RedirectURL == "" {
		return errors.New("OIDC_REDIRECT_URL is required when OIDC is enabled")
	}

	if !isValidRedirectURL(c.RedirectURL) {
		return errors.New("OIDC_REDIRECT_URL must be an absolute https URL (http only for localhost) without credentials or fragment")
	}

	if !containsString(c.Scopes, "openid") {
		return errors.New("OIDC_SCOPES must include openid")
	}

	if n := utf8.RuneCountInString(c.RoleClaim); n < 1 || n > maxOIDCRoleClaimLen {
		return fmt.Errorf("OIDC_ROLE_CLAIM must be 1 to %d characters", maxOIDCRoleClaimLen)
	}

	if c.StateTTL < MinOIDCStateTTL || c.StateTTL > MaxOIDCStateTTL {
		return fmt.Errorf("OIDC_STATE_TTL must be between %s and %s", MinOIDCStateTTL, MaxOIDCStateTTL)
	}

	if !IsSafeRedirectPath(c.FrontendPath) {
		return errors.New("OIDC_FRONTEND_PATH must be a path that starts with one slash, for example /auth/callback")
	}

	if err := validateRoleNames("OIDC_ADMIN_ROLES", c.AdminRoles); err != nil {
		return err
	}

	return validateRoleNames("OIDC_DEVOPS_ROLES", c.DevOpsRoles)
}

func validateRoleNames(name string, roles []string) error {
	for _, r := range roles {
		if utf8.RuneCountInString(r) > maxOIDCRoleNameLen {
			return fmt.Errorf("%s has a role name longer than %d characters", name, maxOIDCRoleNameLen)
		}
	}

	return nil
}

// IsSafeRedirectPath reports whether p is a same-origin path that a redirect
// can use without leaving the site: one leading slash (not "//"), only URL
// path characters (no backslash, no control character, no scheme).
func IsSafeRedirectPath(p string) bool {
	return !strings.HasPrefix(p, "//") && oidcFrontendPathPattern.MatchString(p)
}

// isHTTPSURLWithoutExtras reports whether raw is an https URL with a host and
// no credentials, query or fragment.
func isHTTPSURLWithoutExtras(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return false
	}

	return u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

// isValidRedirectURL reports whether raw is an absolute https URL, or an http
// URL on a loopback host (local development), without credentials or
// fragment. The browser, not the server, calls it, so http on localhost sends
// nothing over a network.
func isValidRedirectURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}

	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}

		ip := net.ParseIP(host)

		return ip != nil && ip.IsLoopback()
	}

	return false
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}

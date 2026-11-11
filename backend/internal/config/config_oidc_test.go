package config_test

import (
	"strings"
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oidcEnvKeys are the variables that LoadConfig reads for OIDCConfig.
var oidcEnvKeys = []string{
	"OIDC_ENABLED", "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
	"OIDC_REDIRECT_URL", "OIDC_SCOPES", "OIDC_ROLE_CLAIM", "OIDC_ADMIN_ROLES",
	"OIDC_DEVOPS_ROLES", "OIDC_LOCAL_AUTH", "OIDC_STATE_TTL", "OIDC_FRONTEND_PATH",
}

// clearOIDCEnv unsets the auth and OIDC variables for the test.
func clearOIDCEnv(t *testing.T) {
	t.Helper()
	clearAuthEnv(t)
	for _, k := range oidcEnvKeys {
		t.Setenv(k, "")
	}
}

const oidcTestSecret = "s3cr3t-client-secret-value"

// validOIDCEnv is a complete OIDC setup behind AUTH_ENABLED=true.
func validOIDCEnv() map[string]string {
	return map[string]string{
		"AUTH_ENABLED":       "true",
		"JWT_SECRET":         "1234567890123456",
		"OIDC_ENABLED":       "true",
		"OIDC_ISSUER":        "https://idp.example.com/realms/app",
		"OIDC_CLIENT_ID":     "app-client",
		"OIDC_CLIENT_SECRET": oidcTestSecret,
		"OIDC_REDIRECT_URL":  "https://app.example.com/api/v1/auth/oidc/callback",
	}
}

func TestLoadConfigOIDCDefaults(t *testing.T) {
	// Not parallel: t.Setenv modifies the process environment.
	clearOIDCEnv(t)

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.False(t, cfg.OIDC.Enabled, "OIDC is off by default")
	assert.False(t, cfg.OIDC.LocalAuth)
	assert.Empty(t, cfg.OIDC.Issuer)
	assert.Empty(t, cfg.OIDC.ClientSecret, "there is no default client secret")
	assert.Equal(t, []string{"openid", "profile", "email"}, cfg.OIDC.Scopes)
	assert.Equal(t, "roles", cfg.OIDC.RoleClaim)
	assert.Empty(t, cfg.OIDC.AdminRoles, "no role is an admin role by default")
	assert.Empty(t, cfg.OIDC.DevOpsRoles)
	assert.Equal(t, 5*time.Minute, cfg.OIDC.StateTTL)
	assert.Equal(t, "/auth/callback", cfg.OIDC.FrontendPath)
}

func TestLoadConfigOIDCFromEnv(t *testing.T) {
	clearOIDCEnv(t)
	for k, v := range validOIDCEnv() {
		t.Setenv(k, v)
	}
	t.Setenv("OIDC_SCOPES", "openid, groups ,email")
	t.Setenv("OIDC_ROLE_CLAIM", "groups")
	t.Setenv("OIDC_ADMIN_ROLES", "Admins, ops ,,")
	t.Setenv("OIDC_DEVOPS_ROLES", "builders")
	t.Setenv("OIDC_LOCAL_AUTH", "true")
	t.Setenv("OIDC_STATE_TTL", "2m")
	t.Setenv("OIDC_FRONTEND_PATH", "/welcome")

	cfg, err := config.LoadConfig()
	require.NoError(t, err)

	assert.True(t, cfg.OIDC.Enabled)
	assert.True(t, cfg.OIDC.LocalAuth)
	assert.Equal(t, "https://idp.example.com/realms/app", cfg.OIDC.Issuer)
	assert.Equal(t, "app-client", cfg.OIDC.ClientID)
	assert.Equal(t, oidcTestSecret, cfg.OIDC.ClientSecret)
	assert.Equal(t, "https://app.example.com/api/v1/auth/oidc/callback", cfg.OIDC.RedirectURL)
	assert.Equal(t, []string{"openid", "groups", "email"}, cfg.OIDC.Scopes)
	assert.Equal(t, "groups", cfg.OIDC.RoleClaim)
	assert.Equal(t, []string{"Admins", "ops"}, cfg.OIDC.AdminRoles, "names are kept as written (case matters), empties dropped")
	assert.Equal(t, []string{"builders"}, cfg.OIDC.DevOpsRoles)
	assert.Equal(t, 2*time.Minute, cfg.OIDC.StateTTL)
	assert.Equal(t, "/welcome", cfg.OIDC.FrontendPath)
}

// With AUTH_ENABLED=true and OIDC_ENABLED=true, startup fails with a clear
// error for a missing or malformed setting. The error names the variable and
// never contains the client secret.
func TestLoadConfigOIDCEnabledValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(env map[string]string)
		wantErr string
	}{
		{name: "complete setup passes", mutate: func(map[string]string) {}},
		{name: "missing issuer", mutate: func(e map[string]string) { delete(e, "OIDC_ISSUER") }, wantErr: "OIDC_ISSUER is required"},
		{name: "missing client id", mutate: func(e map[string]string) { delete(e, "OIDC_CLIENT_ID") }, wantErr: "OIDC_CLIENT_ID is required"},
		{name: "blank client id", mutate: func(e map[string]string) { e["OIDC_CLIENT_ID"] = "   " }, wantErr: "OIDC_CLIENT_ID is required"},
		{name: "missing client secret", mutate: func(e map[string]string) { delete(e, "OIDC_CLIENT_SECRET") }, wantErr: "OIDC_CLIENT_SECRET is required"},
		{name: "missing redirect url", mutate: func(e map[string]string) { delete(e, "OIDC_REDIRECT_URL") }, wantErr: "OIDC_REDIRECT_URL is required"},
		{name: "http issuer", mutate: func(e map[string]string) { e["OIDC_ISSUER"] = "http://idp.example.com" }, wantErr: "OIDC_ISSUER must be an https URL"},
		{name: "http loopback issuer is still refused", mutate: func(e map[string]string) { e["OIDC_ISSUER"] = "http://127.0.0.1:8080" }, wantErr: "OIDC_ISSUER must be an https URL"},
		{name: "issuer without host", mutate: func(e map[string]string) { e["OIDC_ISSUER"] = "https:///realm" }, wantErr: "OIDC_ISSUER must be an https URL"},
		{name: "issuer is not a url", mutate: func(e map[string]string) { e["OIDC_ISSUER"] = "idp.example.com" }, wantErr: "OIDC_ISSUER must be an https URL"},
		{name: "issuer with credentials", mutate: func(e map[string]string) { e["OIDC_ISSUER"] = "https://user:pw@idp.example.com" }, wantErr: "OIDC_ISSUER must be an https URL"},
		{name: "issuer with query", mutate: func(e map[string]string) { e["OIDC_ISSUER"] = "https://idp.example.com/?a=b" }, wantErr: "OIDC_ISSUER must be an https URL"},
		{name: "issuer with fragment", mutate: func(e map[string]string) { e["OIDC_ISSUER"] = "https://idp.example.com/#x" }, wantErr: "OIDC_ISSUER must be an https URL"},
		{name: "relative redirect url", mutate: func(e map[string]string) { e["OIDC_REDIRECT_URL"] = "/api/v1/auth/oidc/callback" }, wantErr: "OIDC_REDIRECT_URL must be an absolute"},
		{name: "http redirect url on a public host", mutate: func(e map[string]string) { e["OIDC_REDIRECT_URL"] = "http://app.example.com/cb" }, wantErr: "OIDC_REDIRECT_URL must be an absolute"},
		{name: "http redirect url on localhost passes", mutate: func(e map[string]string) { e["OIDC_REDIRECT_URL"] = "http://localhost:8081/api/v1/auth/oidc/callback" }},
		{name: "redirect url with fragment", mutate: func(e map[string]string) { e["OIDC_REDIRECT_URL"] = "https://app.example.com/cb#x" }, wantErr: "OIDC_REDIRECT_URL must be an absolute"},
		{name: "redirect url with credentials", mutate: func(e map[string]string) { e["OIDC_REDIRECT_URL"] = "https://u:p@app.example.com/cb" }, wantErr: "OIDC_REDIRECT_URL must be an absolute"},
		{name: "scopes without openid", mutate: func(e map[string]string) { e["OIDC_SCOPES"] = "profile,email" }, wantErr: "OIDC_SCOPES must include openid"},
		{name: "state ttl too long", mutate: func(e map[string]string) { e["OIDC_STATE_TTL"] = "2h" }, wantErr: "OIDC_STATE_TTL must be between"},
		{name: "state ttl zero", mutate: func(e map[string]string) { e["OIDC_STATE_TTL"] = "0s" }, wantErr: "OIDC_STATE_TTL must be between"},
		{name: "state ttl not a duration", mutate: func(e map[string]string) { e["OIDC_STATE_TTL"] = "soon" }, wantErr: "OIDC_STATE_TTL must be a duration"},
		{name: "frontend path with a host", mutate: func(e map[string]string) { e["OIDC_FRONTEND_PATH"] = "//evil.example.com" }, wantErr: "OIDC_FRONTEND_PATH must be a path"},
		{name: "frontend path absolute url", mutate: func(e map[string]string) { e["OIDC_FRONTEND_PATH"] = "https://evil.example.com/" }, wantErr: "OIDC_FRONTEND_PATH must be a path"},
		{name: "frontend path with backslash", mutate: func(e map[string]string) { e["OIDC_FRONTEND_PATH"] = "/\\evil.example.com" }, wantErr: "OIDC_FRONTEND_PATH must be a path"},
		{name: "frontend path with a newline", mutate: func(e map[string]string) { e["OIDC_FRONTEND_PATH"] = "/a\nLocation: x" }, wantErr: "OIDC_FRONTEND_PATH must be a path"},
		{name: "role claim too long", mutate: func(e map[string]string) { e["OIDC_ROLE_CLAIM"] = strings.Repeat("c", 101) }, wantErr: "OIDC_ROLE_CLAIM must be"},
		{name: "local auth typo", mutate: func(e map[string]string) { e["OIDC_LOCAL_AUTH"] = "treu" }, wantErr: "OIDC_LOCAL_AUTH must be a boolean"},
		{name: "enabled typo", mutate: func(e map[string]string) { e["OIDC_ENABLED"] = "yes please" }, wantErr: "OIDC_ENABLED must be a boolean"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			clearOIDCEnv(t)
			env := validOIDCEnv()
			tt.mutate(env)
			for k, v := range env {
				t.Setenv(k, v)
			}

			cfg, err := config.LoadConfig()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.NotContains(t, err.Error(), oidcTestSecret, "the error must not contain the client secret")
				assert.NotContains(t, err.Error(), "pw@", "the error must not echo credentials of a URL")
				return
			}
			require.NoError(t, err)
			assert.True(t, cfg.OIDC.Enabled)
		})
	}
}

// OIDC is inert without authentication: its settings are not validated and
// the handlers answer 404.
func TestLoadConfigOIDCWithAuthDisabledIsNotValidated(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("AUTH_ENABLED", "false")
	t.Setenv("OIDC_ENABLED", "true")

	cfg, err := config.LoadConfig()

	require.NoError(t, err)
	assert.True(t, cfg.OIDC.Enabled)
	assert.False(t, cfg.Auth.Enabled)
}

func TestLoadConfigOIDCDisabledIgnoresIncompleteSettings(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("AUTH_ENABLED", "true")
	t.Setenv("JWT_SECRET", "1234567890123456")
	t.Setenv("OIDC_ENABLED", "false")
	t.Setenv("OIDC_ISSUER", "not a url")

	_, err := config.LoadConfig()

	require.NoError(t, err)
}

func TestOIDCConfigValidate(t *testing.T) {
	t.Parallel()
	valid := func() config.OIDCConfig {
		return config.OIDCConfig{
			Enabled: true, Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "s",
			RedirectURL: "https://app.example.com/cb", Scopes: []string{"openid"}, RoleClaim: "roles",
			StateTTL: time.Minute, FrontendPath: "/auth/callback",
		}
	}
	tests := []struct {
		name    string
		mutate  func(c *config.OIDCConfig)
		wantErr string
	}{
		{name: "valid", mutate: func(*config.OIDCConfig) {}},
		{name: "empty role claim", mutate: func(c *config.OIDCConfig) { c.RoleClaim = "" }, wantErr: "OIDC_ROLE_CLAIM"},
		{name: "empty frontend path", mutate: func(c *config.OIDCConfig) { c.FrontendPath = "" }, wantErr: "OIDC_FRONTEND_PATH"},
		{name: "state ttl at the lower bound", mutate: func(c *config.OIDCConfig) { c.StateTTL = 30 * time.Second }},
		{name: "state ttl below the lower bound", mutate: func(c *config.OIDCConfig) { c.StateTTL = 29 * time.Second }, wantErr: "OIDC_STATE_TTL"},
		{name: "state ttl at the upper bound", mutate: func(c *config.OIDCConfig) { c.StateTTL = time.Hour }},
		{name: "state ttl above the upper bound", mutate: func(c *config.OIDCConfig) { c.StateTTL = time.Hour + time.Second }, wantErr: "OIDC_STATE_TTL"},
		{name: "a role name over the limit", mutate: func(c *config.OIDCConfig) { c.AdminRoles = []string{strings.Repeat("r", 201)} }, wantErr: "OIDC_ADMIN_ROLES"},
		{name: "a devops role name over the limit", mutate: func(c *config.OIDCConfig) { c.DevOpsRoles = []string{strings.Repeat("r", 201)} }, wantErr: "OIDC_DEVOPS_ROLES"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tt.mutate(&c)

			err := c.Validate()

			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

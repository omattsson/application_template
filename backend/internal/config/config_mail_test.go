package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mailOn is the smallest valid mail setup.
func mailOn(extra map[string]string) map[string]string {
	env := map[string]string{
		"MAIL_ENABLED": "true",
		"SMTP_HOST":    "smtp.example.com",
		"MAIL_FROM":    "App <noreply@example.com>",
	}
	for k, v := range extra {
		env[k] = v
	}

	return env
}

// resetOn is the smallest valid password reset setup.
func resetOn(extra map[string]string) map[string]string {
	env := mailOn(map[string]string{
		"AUTH_ENABLED":           "true",
		"JWT_SECRET":             "0123456789abcdef0123",
		"PASSWORD_RESET_ENABLED": "true",
		"PASSWORD_RESET_URL":     "https://app.example.com",
	})
	for k, v := range extra {
		env[k] = v
	}

	return env
}

func loadWith(t *testing.T, env map[string]string) (*config.Config, error) {
	t.Helper()
	t.Setenv("ENV_FILE", "/nonexistent/.env")
	for k, v := range env {
		t.Setenv(k, v)
	}

	return config.LoadConfig()
}

func TestLoadConfigMailAndResetDefaults(t *testing.T) {
	cfg, err := loadWith(t, nil)
	require.NoError(t, err)

	assert.False(t, cfg.Mail.Enabled, "the mailer is off by default")
	assert.False(t, cfg.PasswordReset.Enabled, "the flow is off by default")
	assert.Equal(t, 587, cfg.Mail.Port)
	assert.Equal(t, "starttls", cfg.Mail.TLS)
	assert.Equal(t, 10*time.Second, cfg.Mail.Timeout)
	assert.Equal(t, time.Hour, cfg.PasswordReset.TokenTTL)
	assert.Equal(t, time.Minute, cfg.PasswordReset.MinInterval)
}

func TestLoadConfigMail(t *testing.T) {
	tests := []struct {
		env     map[string]string
		name    string
		wantErr string
	}{
		{name: "valid with defaults", env: mailOn(nil)},
		{name: "valid with a bare from address", env: mailOn(map[string]string{"MAIL_FROM": "noreply@example.com"})},
		{name: "valid implicit tls and auth", env: mailOn(map[string]string{"SMTP_TLS": "TLS", "SMTP_PORT": "465", "SMTP_USERNAME": "u", "SMTP_PASSWORD": "p"})},
		{name: "valid no tls without credentials", env: mailOn(map[string]string{"SMTP_TLS": "none", "SMTP_PORT": "1025"})},
		{name: "ipv6 host", env: mailOn(map[string]string{"SMTP_HOST": "::1"})},
		{name: "typo in the switch", env: map[string]string{"MAIL_ENABLED": "yes"}, wantErr: "MAIL_ENABLED"},
		{name: "host is required", env: mailOn(map[string]string{"SMTP_HOST": ""}), wantErr: "SMTP_HOST"},
		{name: "host with a scheme", env: mailOn(map[string]string{"SMTP_HOST": "smtp://mail.example.com"}), wantErr: "SMTP_HOST"},
		{name: "host with a port", env: mailOn(map[string]string{"SMTP_HOST": "mail.example.com:25"}), wantErr: "SMTP_HOST"},
		{name: "host with a space", env: mailOn(map[string]string{"SMTP_HOST": "mail example.com"}), wantErr: "SMTP_HOST"},
		{name: "port that is not a number", env: mailOn(map[string]string{"SMTP_PORT": "smtp"}), wantErr: "SMTP_PORT"},
		{name: "port zero", env: mailOn(map[string]string{"SMTP_PORT": "0"}), wantErr: "SMTP_PORT"},
		{name: "port too high", env: mailOn(map[string]string{"SMTP_PORT": "70000"}), wantErr: "SMTP_PORT"},
		{name: "unknown tls mode", env: mailOn(map[string]string{"SMTP_TLS": "ssl"}), wantErr: "SMTP_TLS"},
		{name: "no tls with a username", env: mailOn(map[string]string{"SMTP_TLS": "none", "SMTP_USERNAME": "u", "SMTP_PASSWORD": "p"}), wantErr: "SMTP_TLS=none"},
		{name: "username without password", env: mailOn(map[string]string{"SMTP_USERNAME": "u"}), wantErr: "SMTP_PASSWORD"},
		{name: "from is required", env: mailOn(map[string]string{"MAIL_FROM": ""}), wantErr: "MAIL_FROM"},
		{name: "from is not an address", env: mailOn(map[string]string{"MAIL_FROM": "not an address"}), wantErr: "MAIL_FROM"},
		{name: "from with a header injection", env: mailOn(map[string]string{"MAIL_FROM": "a@example.com\r\nBcc: x@example.com"}), wantErr: "MAIL_FROM"},
		{name: "from with two addresses", env: mailOn(map[string]string{"MAIL_FROM": "a@example.com, b@example.com"}), wantErr: "MAIL_FROM"},
		{name: "timeout that does not parse", env: mailOn(map[string]string{"SMTP_TIMEOUT": "soon"}), wantErr: "SMTP_TIMEOUT"},
		{name: "timeout zero", env: mailOn(map[string]string{"SMTP_TIMEOUT": "0s"}), wantErr: "SMTP_TIMEOUT"},
		{name: "timeout too long", env: mailOn(map[string]string{"SMTP_TIMEOUT": "10m"}), wantErr: "SMTP_TIMEOUT"},
		{name: "off reads bad values leniently", env: map[string]string{"SMTP_PORT": "smtp", "SMTP_TIMEOUT": "soon", "SMTP_TLS": "ssl", "MAIL_FROM": "bad"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadWith(t, tt.env)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.env["MAIL_ENABLED"] == "true", cfg.Mail.Enabled)
		})
	}
}

func TestLoadConfigMailValues(t *testing.T) {
	cfg, err := loadWith(t, mailOn(map[string]string{
		"SMTP_PORT": "465", "SMTP_TLS": " TLS ", "SMTP_USERNAME": "user", "SMTP_PASSWORD": " pass word ", "SMTP_TIMEOUT": "3s",
	}))
	require.NoError(t, err)

	assert.Equal(t, "smtp.example.com", cfg.Mail.Host)
	assert.Equal(t, 465, cfg.Mail.Port)
	assert.Equal(t, config.SMTPTLSImplicit, cfg.Mail.TLS)
	assert.Equal(t, "user", cfg.Mail.Username)
	assert.Equal(t, " pass word ", cfg.Mail.Password, "the password is kept as it is")
	assert.Equal(t, "App <noreply@example.com>", cfg.Mail.From)
	assert.Equal(t, 3*time.Second, cfg.Mail.Timeout)
}

func TestMailConfigNeverLogsThePassword(t *testing.T) {
	cfg, err := loadWith(t, mailOn(map[string]string{"SMTP_USERNAME": "user", "SMTP_PASSWORD": "s3cret-value"}))
	require.NoError(t, err)

	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("config", "mail", cfg.Mail)
	assert.NotContains(t, b.String(), "s3cret-value")
	assert.NotContains(t, cfg.Mail.LogValue().String(), "s3cret-value")
}

func TestLoadConfigPasswordReset(t *testing.T) {
	tests := []struct {
		env     map[string]string
		name    string
		wantErr string
	}{
		{name: "valid", env: resetOn(nil)},
		{name: "valid with a path and a trailing slash", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "https://app.example.com/ui/"})},
		{name: "http on localhost", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "http://localhost:3000"})},
		{name: "http on a loopback address", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "http://127.0.0.1:3000"})},
		{name: "http on ipv6 loopback", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "http://[::1]:3000"})},
		{name: "ttl at the lower bound", env: resetOn(map[string]string{"PASSWORD_RESET_TOKEN_TTL": "5m"})},
		{name: "ttl at the upper bound", env: resetOn(map[string]string{"PASSWORD_RESET_TOKEN_TTL": "24h"})},
		{name: "typo in the switch", env: map[string]string{"PASSWORD_RESET_ENABLED": "yes"}, wantErr: "PASSWORD_RESET_ENABLED"},
		{name: "needs auth", env: resetOn(map[string]string{"AUTH_ENABLED": "false"}), wantErr: "AUTH_ENABLED"},
		{name: "needs the mailer", env: resetOn(map[string]string{"MAIL_ENABLED": "false"}), wantErr: "MAIL_ENABLED"},
		{
			name: "needs the local password form with oidc",
			env: resetOn(map[string]string{
				"OIDC_ENABLED": "true", "OIDC_ISSUER": "https://idp.example.com", "OIDC_CLIENT_ID": "id", "OIDC_CLIENT_SECRET": "s",
				"OIDC_REDIRECT_URL": "https://app.example.com/cb", "OIDC_LOCAL_AUTH": "false",
			}),
			wantErr: "OIDC_LOCAL_AUTH",
		},
		{
			name: "oidc with local auth is fine",
			env: resetOn(map[string]string{
				"OIDC_ENABLED": "true", "OIDC_ISSUER": "https://idp.example.com", "OIDC_CLIENT_ID": "id", "OIDC_CLIENT_SECRET": "s",
				"OIDC_REDIRECT_URL": "https://app.example.com/cb", "OIDC_LOCAL_AUTH": "true",
			}),
		},
		{name: "url is required", env: resetOn(map[string]string{"PASSWORD_RESET_URL": ""}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url is not absolute", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "app.example.com"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "plain http on a public host", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "http://app.example.com"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url with credentials", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "https://user:pw@app.example.com"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url with a query", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "https://app.example.com/?a=b"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url with an empty query", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "https://app.example.com/?"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url with a fragment", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "https://app.example.com/#x"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url that is a javascript scheme", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "javascript://app.example.com/%0aalert(1)"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url with a control character", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "https://app.example.com/\r\nBcc: x"}), wantErr: "PASSWORD_RESET_URL"},
		{name: "url too long", env: resetOn(map[string]string{"PASSWORD_RESET_URL": "https://app.example.com/" + strings.Repeat("a", 600)}), wantErr: "PASSWORD_RESET_URL"},
		{name: "ttl that does not parse", env: resetOn(map[string]string{"PASSWORD_RESET_TOKEN_TTL": "1 hour"}), wantErr: "PASSWORD_RESET_TOKEN_TTL"},
		{name: "ttl below 5m", env: resetOn(map[string]string{"PASSWORD_RESET_TOKEN_TTL": "4m59s"}), wantErr: "PASSWORD_RESET_TOKEN_TTL"},
		{name: "ttl above 24h", env: resetOn(map[string]string{"PASSWORD_RESET_TOKEN_TTL": "25h"}), wantErr: "PASSWORD_RESET_TOKEN_TTL"},
		{name: "interval that does not parse", env: resetOn(map[string]string{"PASSWORD_RESET_MIN_INTERVAL": "often"}), wantErr: "PASSWORD_RESET_MIN_INTERVAL"},
		{name: "interval zero", env: resetOn(map[string]string{"PASSWORD_RESET_MIN_INTERVAL": "0s"}), wantErr: "PASSWORD_RESET_MIN_INTERVAL"},
		{name: "interval above 24h", env: resetOn(map[string]string{"PASSWORD_RESET_MIN_INTERVAL": "25h"}), wantErr: "PASSWORD_RESET_MIN_INTERVAL"},
		{
			name: "off reads bad values leniently",
			env:  map[string]string{"PASSWORD_RESET_TOKEN_TTL": "soon", "PASSWORD_RESET_MIN_INTERVAL": "-1s", "PASSWORD_RESET_URL": "nonsense"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadWith(t, tt.env)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.env["PASSWORD_RESET_ENABLED"] == "true", cfg.PasswordReset.Enabled)
		})
	}
}

func TestLoadConfigPasswordResetValues(t *testing.T) {
	cfg, err := loadWith(t, resetOn(map[string]string{
		"PASSWORD_RESET_URL": " https://app.example.com/ui/ ", "PASSWORD_RESET_TOKEN_TTL": "30m", "PASSWORD_RESET_MIN_INTERVAL": "2m",
	}))
	require.NoError(t, err)

	assert.True(t, cfg.PasswordReset.Enabled)
	assert.Equal(t, "https://app.example.com/ui", cfg.PasswordReset.URL, "the trailing slash is dropped")
	assert.Equal(t, 30*time.Minute, cfg.PasswordReset.TokenTTL)
	assert.Equal(t, 2*time.Minute, cfg.PasswordReset.MinInterval)
}

// The mailer alone (MAIL_ENABLED) does not need auth or the flow.
func TestLoadConfigMailWithoutTheFlow(t *testing.T) {
	cfg, err := loadWith(t, mailOn(nil))
	require.NoError(t, err)

	assert.True(t, cfg.Mail.Enabled)
	assert.False(t, cfg.PasswordReset.Enabled)
}

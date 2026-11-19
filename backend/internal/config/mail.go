package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// Values of SMTP_TLS (MailConfig.TLS).
const (
	// SMTPTLSStartTLS connects in clear text and upgrades with STARTTLS, which
	// is required (default; port 587).
	SMTPTLSStartTLS = "starttls"
	// SMTPTLSImplicit wraps the connection in TLS from the first byte (port 465).
	SMTPTLSImplicit = "tls"
	// SMTPTLSNone sends in clear text. It is for a local mail catcher and is
	// refused together with SMTP_USERNAME.
	SMTPTLSNone = "none"
)

// Defaults and limits of the mail and password reset modules.
const (
	defaultSMTPPort    = 587
	defaultSMTPTimeout = 10 * time.Second
	// maxSMTPTimeout bounds a send: the flow waits for it in tracked work that
	// shutdown has to wait for.
	maxSMTPTimeout = 5 * time.Minute

	defaultPasswordResetTokenTTL = time.Hour
	// minPasswordResetTokenTTL and maxPasswordResetTokenTTL bound the life of a
	// reset token: long enough to read a mail, short enough to limit a stolen
	// mailbox.
	minPasswordResetTokenTTL        = 5 * time.Minute
	maxPasswordResetTokenTTL        = 24 * time.Hour
	defaultPasswordResetMinInterval = time.Minute
	maxPasswordResetMinInterval     = 24 * time.Hour
	// maxPasswordResetURLLen bounds the base URL of the reset link.
	maxPasswordResetURLLen = 512
)

// MailConfig holds the settings of the optional mail module (the SMTP sender
// that the password reset uses). With Enabled false nothing is built and the
// other fields are not used.
type MailConfig struct {
	// Timeout is SMTP_TIMEOUT (default 10s): connect, TLS and the whole send.
	Timeout time.Duration
	// Host is SMTP_HOST: a host name or an IP address, without scheme or port.
	Host string
	// TLS is SMTP_TLS: SMTPTLSStartTLS (default), SMTPTLSImplicit or SMTPTLSNone.
	TLS string
	// Username is SMTP_USERNAME. Empty: no authentication.
	Username string
	// Password is SMTP_PASSWORD. It is never logged (see LogValue).
	Password string
	// From is MAIL_FROM: an address, optionally with a display name.
	From string
	// Port is SMTP_PORT (default 587).
	Port int
	// Enabled is MAIL_ENABLED (default false, strict boolean).
	Enabled bool
}

// LogValue implements slog.LogValuer: the password is never part of a log
// record, whatever the caller logs.
func (c MailConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("enabled", c.Enabled),
		slog.String("host", c.Host),
		slog.Int("port", c.Port),
		slog.String("tls", c.TLS),
		slog.Bool("auth", c.Username != ""),
	)
}

// String implements fmt.Stringer so that a %v or %+v of the config cannot
// print the password either.
func (c MailConfig) String() string {
	return fmt.Sprintf("{enabled:%t host:%s port:%d tls:%s auth:%t}", c.Enabled, c.Host, c.Port, c.TLS, c.Username != "")
}

// GoString implements fmt.GoStringer (%#v) for the same reason.
func (c MailConfig) GoString() string { return c.String() }

// PasswordResetConfig holds the settings of the optional password reset
// module (a single-use token that is mailed to the account).
type PasswordResetConfig struct {
	// TokenTTL is PASSWORD_RESET_TOKEN_TTL (default 1h, 5m to 24h).
	TokenTTL time.Duration
	// MinInterval is PASSWORD_RESET_MIN_INTERVAL (default 60s): at most one mail
	// per user in that time.
	MinInterval time.Duration
	// URL is PASSWORD_RESET_URL: the public base URL of the web UI, without a
	// trailing slash. The link in the mail is URL + "/reset-password#token=...".
	URL string
	// Enabled is PASSWORD_RESET_ENABLED (default false, strict boolean). With
	// false the routes do not exist.
	Enabled bool
}

// loadMailConfig reads the mail variables. The switch is parsed strictly (a
// typo must not silently drop the mailer). The other values are read leniently
// while the module is off, because nothing uses them, and strictly while it is
// on.
func loadMailConfig() (MailConfig, error) {
	enabled, err := parseEnvBoolStrict("MAIL_ENABLED", false)
	if err != nil {
		return MailConfig{}, err
	}

	c := MailConfig{
		Enabled:  enabled,
		Host:     strings.TrimSpace(getEnv("SMTP_HOST", "")),
		TLS:      strings.ToLower(strings.TrimSpace(getEnv("SMTP_TLS", SMTPTLSStartTLS))),
		Username: getEnv("SMTP_USERNAME", ""),
		Password: getEnv("SMTP_PASSWORD", ""),
		From:     strings.TrimSpace(getEnv("MAIL_FROM", "")),
	}

	if !enabled {
		c.Port = int(getEnvInt32("SMTP_PORT", defaultSMTPPort))
		c.Timeout = getEnvDuration("SMTP_TIMEOUT", defaultSMTPTimeout)

		return c, nil
	}

	port, err := parseEnvInt32Strict("SMTP_PORT", defaultSMTPPort)
	if err != nil {
		return MailConfig{}, err
	}

	c.Port = int(port)

	if c.Timeout, err = parseEnvDurationStrict("SMTP_TIMEOUT", defaultSMTPTimeout); err != nil {
		return MailConfig{}, err
	}

	return c, nil
}

// Validate checks the settings that the module uses. It is called only with
// the module on. No message repeats a value (the password least of all).
func (c *MailConfig) Validate() error {
	if err := validateSMTPHost(c.Host); err != nil {
		return err
	}

	if c.Port < 1 || c.Port > 65535 {
		return errors.New("SMTP_PORT must be between 1 and 65535")
	}

	switch c.TLS {
	case SMTPTLSStartTLS, SMTPTLSImplicit, SMTPTLSNone:
	default:
		return errors.New("SMTP_TLS must be starttls, tls or none")
	}

	if c.TLS == SMTPTLSNone && c.Username != "" {
		return errors.New("SMTP_TLS=none cannot be used with SMTP_USERNAME: the credentials would travel in clear text")
	}

	if c.Username != "" && c.Password == "" {
		return errors.New("SMTP_PASSWORD is required when SMTP_USERNAME is set")
	}

	if err := validateMailFrom(c.From); err != nil {
		return err
	}

	if c.Timeout <= 0 || c.Timeout > maxSMTPTimeout {
		return errors.New("SMTP_TIMEOUT must be positive and at most 5m")
	}

	return nil
}

// validateSMTPHost accepts a host name or an IP address: no scheme, port,
// path, space or control character.
func validateSMTPHost(host string) error {
	const msg = "SMTP_HOST must be a host name or an IP address, without scheme or port"

	if host == "" {
		return errors.New("SMTP_HOST is required with MAIL_ENABLED=true")
	}

	if net.ParseIP(host) != nil {
		return nil
	}

	if len(host) > 253 {
		return errors.New(msg)
	}

	for _, r := range host {
		ok := r == '.' || r == '-' || r == '_' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return errors.New(msg)
		}
	}

	return nil
}

// validateMailFrom accepts one address with an optional display name and
// refuses a line break or other control character (header injection).
func validateMailFrom(from string) error {
	const msg = "MAIL_FROM must be one address, for example noreply@example.com or App <noreply@example.com>"

	if from == "" {
		return errors.New("MAIL_FROM is required with MAIL_ENABLED=true")
	}

	for _, r := range from {
		if r < 0x20 || r == 0x7f {
			return errors.New(msg)
		}
	}

	if _, err := mail.ParseAddress(from); err != nil {
		return errors.New(msg)
	}

	return nil
}

// loadPasswordResetConfig reads the password reset variables. The switch is
// parsed strictly; the other values are read leniently while the flow is off
// and strictly while it is on.
func loadPasswordResetConfig() (PasswordResetConfig, error) {
	enabled, err := parseEnvBoolStrict("PASSWORD_RESET_ENABLED", false)
	if err != nil {
		return PasswordResetConfig{}, err
	}

	c := PasswordResetConfig{
		Enabled: enabled,
		URL:     strings.TrimRight(strings.TrimSpace(getEnv("PASSWORD_RESET_URL", "")), "/"),
	}

	if !enabled {
		c.TokenTTL = getEnvDuration("PASSWORD_RESET_TOKEN_TTL", defaultPasswordResetTokenTTL)
		c.MinInterval = getEnvDuration("PASSWORD_RESET_MIN_INTERVAL", defaultPasswordResetMinInterval)

		return c, nil
	}

	if c.TokenTTL, err = parseEnvDurationStrict("PASSWORD_RESET_TOKEN_TTL", defaultPasswordResetTokenTTL); err != nil {
		return PasswordResetConfig{}, err
	}

	if c.MinInterval, err = parseEnvDurationStrict("PASSWORD_RESET_MIN_INTERVAL", defaultPasswordResetMinInterval); err != nil {
		return PasswordResetConfig{}, err
	}

	return c, nil
}

// Validate checks the settings that the flow uses. It is called only with the
// flow on; the dependencies on other modules (auth, local login, mail) are
// checked by Config.Validate.
func (c *PasswordResetConfig) Validate() error {
	if c.TokenTTL < minPasswordResetTokenTTL || c.TokenTTL > maxPasswordResetTokenTTL {
		return errors.New("PASSWORD_RESET_TOKEN_TTL must be between 5m and 24h")
	}

	if c.MinInterval <= 0 || c.MinInterval > maxPasswordResetMinInterval {
		return errors.New("PASSWORD_RESET_MIN_INTERVAL must be positive and at most 24h")
	}

	return validatePasswordResetURL(c.URL)
}

// validatePasswordResetURL checks the base URL of the reset link: an absolute
// https URL, or an http URL on a loopback host (local development), without
// credentials, query or fragment, of a bounded length. The link carries the
// token in the fragment that the module adds, so the base must not have one.
func validatePasswordResetURL(raw string) error {
	const msg = "PASSWORD_RESET_URL must be the public base URL of the web UI: https (http only on a loopback host), without credentials, query or fragment"

	if raw == "" {
		return errors.New("PASSWORD_RESET_URL is required with PASSWORD_RESET_ENABLED=true")
	}

	if len(raw) > maxPasswordResetURLLen {
		return errors.New(msg)
	}

	u, err := url.Parse(raw)
	if err != nil || !isValidRedirectURL(raw) || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return errors.New(msg)
	}

	return nil
}

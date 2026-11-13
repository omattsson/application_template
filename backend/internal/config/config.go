// Package config provides configuration loading and validation for the application.
// It handles environment variables, default values, and config validation for all components.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

const (
	defaultMaxOpenConns    int32 = 25
	defaultMaxIdleConns    int32 = 5
	defaultConnMaxLifetime       = 5 * time.Minute
	defaultReadTimeout           = 10 * time.Second
	// defaultWriteTimeout is 0 (disabled at the server level) because per-write
	// deadlines are enforced in the WebSocket write pump (websocket/client.go).
	// A non-zero server-level write timeout would prematurely terminate idle
	// WebSocket connections.
	defaultWriteTimeout    = time.Duration(0)
	defaultIdleTimeout     = 30 * time.Second
	defaultShutdownTimeout = 30 * time.Second
)

// Supported values for DB_TYPE.
const (
	DBTypeMySQL  = "mysql"
	DBTypeSQLite = "sqlite"
)

// CORSConfig holds CORS configuration
type CORSConfig struct {
	AllowedOrigins string
}

// Config holds all configuration for the application
//
//nolint:govet // Struct field alignment has been optimized for better memory usage
type Config struct {
	// Group larger structs with time.Duration fields first
	Database DatabaseConfig
	Server   ServerConfig
	// Then string and simple field structs
	App     AppConfig
	Auth    AuthConfig
	OIDC    OIDCConfig
	CORS    CORSConfig
	Logging LogConfig
	// Otel holds the telemetry settings (OTEL_*, METRICS_ENABLED).
	Otel OtelConfig
	// Replica holds the multi-replica switches (LEADER_ELECTION_ENABLED,
	// REPLICA_HEARTBEAT_ENABLED).
	Replica ReplicaConfig
	// LogLevel is LOG_LEVEL parsed to a slog level; main applies it to the
	// default slog logger. An invalid value makes LoadConfig fail.
	LogLevel slog.Level
}

// AppConfig holds application-wide configuration
type AppConfig struct {
	Name        string
	Environment string
	// Title is the product name shown by the web UI (APP_TITLE). LoadConfig
	// applies DefaultAppTitle when it is unset.
	Title string
	// Debug is APP_DEBUG (default true). It drives the Gin mode
	// (Config.GinMode) and whether the GORM logger shows SQL values.
	Debug bool
}

// DefaultAppTitle is the Title used when APP_TITLE is unset.
const DefaultAppTitle = "Application Template"

// MinJWTSecretLength is the minimum length of JWT_SECRET in characters.
const MinJWTSecretLength = 16

// Defaults of the auth durations. There is no default for JWT_SECRET or
// ADMIN_PASSWORD.
const (
	defaultJWTExpiration         = 24 * time.Hour
	defaultAccessTokenExpiration = 15 * time.Minute
	defaultSessionIdleTimeout    = 30 * time.Minute
	defaultSessionMaxLifetime    = 12 * time.Hour
	// defaultRefreshTokenExpiration and defaultRefreshReuseGrace are the
	// defaults of the source project. The session deadline
	// (SESSION_MAX_LIFETIME) caps every refresh token, so with the defaults a
	// refresh token lives at most 12h.
	defaultRefreshTokenExpiration = 168 * time.Hour
	defaultRefreshReuseGrace      = 30 * time.Second
	defaultAdminUsername          = "admin"
)

// Values of SESSION_STORE (AuthConfig.SessionStore).
const (
	// SessionStoreMySQL keeps the token blocklist and the OIDC states in the
	// shared database (the default): every replica sees every revocation.
	SessionStoreMySQL = "mysql"
	// SessionStoreMemory keeps them in the process: for one replica and for
	// development only (a restart forgets every revocation).
	SessionStoreMemory = "memory"
)

// MaxRefreshReuseGrace caps REFRESH_REUSE_GRACE. The grace window only has to
// cover concurrent refresh requests (two browser tabs); a long window weakens
// replay detection.
const MaxRefreshReuseGrace = 5 * time.Minute

// AuthConfig holds authentication configuration.
//
//nolint:govet // Struct field alignment has been optimized for time.Duration and string fields
type AuthConfig struct {
	// JWTExpiration is JWT_EXPIRATION (default 24h): the lifetime of a token
	// issued outside a refresh session.
	JWTExpiration time.Duration
	// AccessTokenExpiration is ACCESS_TOKEN_EXPIRATION (default 15m): the
	// lifetime of the access token of a login session.
	AccessTokenExpiration time.Duration
	// SessionIdleTimeout is SESSION_IDLE_TIMEOUT (default 30m): a session with
	// no activity for this long ends.
	SessionIdleTimeout time.Duration
	// SessionMaxLifetime is SESSION_MAX_LIFETIME (default 12h): the absolute
	// lifetime of a session. Refreshing never extends a session beyond it.
	SessionMaxLifetime time.Duration
	// RefreshTokenExpiration is REFRESH_TOKEN_EXPIRATION (default 168h): the
	// lifetime of one refresh token. It is capped by the session deadline
	// (SessionMaxLifetime after the login), so rotation never extends a
	// session.
	RefreshTokenExpiration time.Duration
	// RefreshReuseGrace is REFRESH_REUSE_GRACE (default 30s, 0 to
	// MaxRefreshReuseGrace, 0 turns it off): a refresh token that a rotation
	// consumed less than this long ago gets one more access token (two tabs
	// that refresh at the same time). Any other reuse revokes the session.
	RefreshReuseGrace time.Duration
	// JWTSecret is JWT_SECRET: the HMAC key of the access tokens. It has no
	// default. Generate it (for example `openssl rand -base64 48`); it is
	// required, at least MinJWTSecretLength characters, when Enabled is true.
	// It is never logged or put in an error.
	JWTSecret string
	// SessionStore is SESSION_STORE (default "mysql"): where the token
	// blocklist and the OIDC states live (SessionStoreMySQL or
	// SessionStoreMemory). Another value fails Validate.
	SessionStore string
	// AdminUsername is ADMIN_USERNAME (default "admin").
	AdminUsername string
	// AdminPassword is ADMIN_PASSWORD. It has no default: when it is empty no
	// admin user is created at start.
	AdminPassword string
	// Enabled is AUTH_ENABLED (default false). With false the API is open and
	// the other fields are not validated.
	Enabled bool
	// SelfRegistration is SELF_REGISTRATION (default false): anyone can create
	// a local account.
	SelfRegistration bool
	// SecureCookies is SECURE_COOKIES (default true): the refresh token
	// and OIDC binding cookies have the Secure attribute and the __Host-
	// name prefix, so the browser sends them over HTTPS only. Set it to false only for plain-HTTP local development on a host
	// other than localhost (browsers accept Secure cookies on
	// http://localhost).
	SecureCookies bool
}

// Validate checks the auth settings. LoadConfig calls it only when Enabled is
// true. Error messages never contain the secret.
func (c *AuthConfig) Validate() error {
	// Surrounding whitespace (a stray newline or padding from a secret file)
	// does not count towards the length: a secret of spaces is no secret.
	secret := strings.TrimSpace(c.JWTSecret)
	if secret == "" {
		return errors.New("jwt_secret is required when AUTH_ENABLED is true")
	}

	// Count characters, not bytes: a multi-byte character is one character,
	// so the check is never weaker than the documented minimum.
	if utf8.RuneCountInString(secret) < MinJWTSecretLength {
		return fmt.Errorf("jwt_secret must be at least %d characters", MinJWTSecretLength)
	}

	if c.JWTExpiration <= 0 {
		return errors.New("jwt_expiration must be positive")
	}

	if c.AccessTokenExpiration <= 0 {
		return errors.New("access_token_expiration must be positive")
	}

	if c.SessionIdleTimeout <= 0 {
		return errors.New("session_idle_timeout must be positive")
	}

	if c.SessionMaxLifetime <= 0 {
		return errors.New("session_max_lifetime must be positive")
	}

	if c.SessionIdleTimeout > c.SessionMaxLifetime {
		return errors.New("session_idle_timeout must not exceed session_max_lifetime")
	}

	// An access token that lives as long as the idle timeout would let a
	// session that never refreshes stay usable for the whole idle window.
	if c.AccessTokenExpiration >= c.SessionIdleTimeout {
		return errors.New("access_token_expiration must be shorter than session_idle_timeout")
	}

	if c.RefreshTokenExpiration <= 0 {
		return errors.New("refresh_token_expiration must be positive")
	}

	if c.RefreshReuseGrace < 0 || c.RefreshReuseGrace > MaxRefreshReuseGrace {
		return fmt.Errorf("refresh_reuse_grace must be between 0s and %s", MaxRefreshReuseGrace)
	}

	switch c.SessionStore {
	case "", SessionStoreMySQL, SessionStoreMemory: // empty means the default (mysql)
	default:
		return fmt.Errorf("SESSION_STORE must be %s or %s", SessionStoreMySQL, SessionStoreMemory)
	}

	return nil
}

// DatabaseConfig holds database-specific configuration
//
//nolint:govet // Struct field alignment has been optimized for time.Duration and string fields
type DatabaseConfig struct {
	// 8-byte aligned fields first
	ConnMaxLifetime time.Duration
	// String fields (8-byte on 64-bit systems)
	// Type is the database backend: "mysql" (default) or "sqlite" (unit tests).
	Type     string
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
	// 4-byte aligned fields
	MaxOpenConns int32
	MaxIdleConns int32
	// Add padding field to maintain 8-byte alignment
	_ [4]byte
}

// ServerConfig holds HTTP server configuration
//
//nolint:govet // Struct field alignment has been optimized for time.Duration fields
type ServerConfig struct {
	// 8-byte aligned fields first
	ReadTimeout time.Duration
	// WriteTimeout is 0 (disabled) at the server level. Per-write deadlines are
	// enforced inside the WebSocket write pump (websocket/client.go), so a
	// server-level write timeout must be disabled to prevent premature
	// termination of idle WebSocket connections. Standard REST handlers complete
	// quickly in practice, but setting WriteTimeout to 0 does mean slow or
	// stalled clients can hold a connection indefinitely. Operators who want
	// protection against slow clients should set SERVER_WRITE_TIMEOUT to a
	// positive value (e.g. 30s); per-handler context deadlines can also be applied.
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// String fields (8-byte on 64-bit systems)
	Host string
	Port string
	// TrustedProxies are the IPs and CIDR ranges that may set the client IP
	// with X-Forwarded-For or X-Real-IP (TRUSTED_PROXIES, see
	// ParseTrustedProxies); nil = no proxy is trusted.
	TrustedProxies []string
	// PprofAddr is PPROF_ADDR (default 127.0.0.1:6060): where the pprof server
	// listens. LoadConfig refuses a non-loopback address unless
	// PprofAllowNonLoopback is true. Read only with PprofEnabled.
	PprofAddr string
	// PprofMutexFraction is PPROF_MUTEX_FRACTION (default 0, off):
	// runtime.SetMutexProfileFraction. Read only with PprofEnabled.
	PprofMutexFraction int
	// PprofBlockRate is PPROF_BLOCK_RATE in nanoseconds (default 0, off):
	// runtime.SetBlockProfileRate. Read only with PprofEnabled.
	PprofBlockRate int
	// 4-byte fields
	// RateLimit is RATE_LIMIT: requests per minute per client IP (default 100).
	RateLimit int32
	// LoginRateLimit is LOGIN_RATE_LIMIT: login attempts per minute per client
	// IP (default 10).
	LoginRateLimit int32
	// 1-byte fields
	// EnableSwagger is ENABLE_SWAGGER (default false).
	EnableSwagger bool
	// PprofEnabled is PPROF_ENABLED (default false): serve net/http/pprof on
	// its own port (PprofAddr), never on the API port.
	PprofEnabled bool
	// PprofAllowNonLoopback is PPROF_ALLOW_NON_LOOPBACK (default false): allow
	// a PprofAddr that is not loopback. The server then logs a warning.
	PprofAllowNonLoopback bool
}

// LogConfig holds logging configuration
type LogConfig struct {
	// Level is LOG_LEVEL: debug, info, warn (or warning) or error.
	Level string
	File  string
}

// SlogLevel parses Level (case-insensitive). An empty Level is info.
func (c LogConfig) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(c.Level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid LOG_LEVEL %q: use debug, info, warn or error", c.Level)
	}
}

// GinMode returns the Gin mode: GIN_MODE when it is set, else release mode
// unless APP_DEBUG is true. Debug mode prints every route at startup and
// warnings that are not wanted in production.
func (c *Config) GinMode() string {
	if m := os.Getenv("GIN_MODE"); m != "" {
		return m
	}

	if c.App.Debug {
		return gin.DebugMode
	}

	return gin.ReleaseMode
}

// OtelConfig holds the telemetry settings: OpenTelemetry traces, metrics and
// logs sent to an OTLP collector, and the Prometheus metrics handler. Both
// switches are off by default and nothing is created while they are off.
type OtelConfig struct {
	// Endpoint is OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317): the
	// gRPC address of the collector as host:port (LoadConfig reduces an
	// http:// URL to it and refuses https://). The connection is not
	// encrypted.
	Endpoint string
	// ServiceName is OTEL_SERVICE_NAME (default backend-api): the
	// service.name of every span, metric and log record.
	ServiceName string
	// SampleRate is OTEL_TRACE_SAMPLE_RATE (default 1): the share of new
	// traces that are recorded, 0 to 1. A request that arrives with a
	// sampled traceparent is always recorded.
	SampleRate float64
	// MetricsAddr is METRICS_ADDR (default :9090): where the Prometheus
	// /metrics server listens. It is separate from the API port, has no
	// authentication and must stay on a private network. Read only with
	// MetricsEnabled.
	MetricsAddr string
	// Enabled is OTEL_ENABLED (default false): traces, metrics and logs go
	// to the collector.
	Enabled bool
	// MetricsEnabled is METRICS_ENABLED (default false): request metrics are
	// recorded and a Prometheus handler is built, also without OTEL_ENABLED.
	MetricsEnabled bool
}

// Validate checks the telemetry settings. LoadConfig calls it only when
// Enabled is true: nothing reads the collector settings otherwise.
func (c *OtelConfig) Validate() error {
	if strings.TrimSpace(c.ServiceName) == "" {
		return errors.New("OTEL_SERVICE_NAME must not be empty")
	}

	if math.IsNaN(c.SampleRate) || c.SampleRate < 0 || c.SampleRate > 1 {
		return errors.New("OTEL_TRACE_SAMPLE_RATE must be a number from 0 to 1")
	}

	// The exporters take host:port. A URL ("http://collector:4317") is not
	// an address and would fail at the first export, long after the start.
	host, port, err := net.SplitHostPort(c.Endpoint)
	if err != nil || host == "" || port == "" || strings.Contains(c.Endpoint, "/") {
		return errors.New("OTEL_EXPORTER_OTLP_ENDPOINT must be host:port, without a scheme")
	}

	return nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if err := c.App.Validate(); err != nil {
		return fmt.Errorf("app config: %w", err)
	}

	if _, err := c.Logging.SlogLevel(); err != nil {
		return err
	}

	if err := c.Database.Validate(); err != nil {
		return fmt.Errorf("database config: %w", err)
	}

	if err := c.Server.Validate(); err != nil {
		return fmt.Errorf("server config: %w", err)
	}

	// With telemetry off the collector settings are not used.
	if c.Otel.Enabled {
		if err := c.Otel.Validate(); err != nil {
			return fmt.Errorf("telemetry config: %w", err)
		}
	}

	// With auth off the auth settings are not used, so a missing secret is fine.
	if c.Auth.Enabled {
		if err := c.Auth.Validate(); err != nil {
			return fmt.Errorf("auth config: %w", err)
		}

		// OIDC needs authentication: with auth off the OIDC routes answer 404
		// and its settings are not used.
		if c.OIDC.Enabled {
			if err := c.OIDC.Validate(); err != nil {
				return fmt.Errorf("oidc config: %w", err)
			}
		}
	}

	return nil
}

func (c *AppConfig) Validate() error {
	if c.Name == "" {
		return errors.New("name is required")
	}

	if c.Environment == "" {
		return errors.New("environment is required")
	}

	return nil
}

func (c *DatabaseConfig) Validate() error {
	switch c.Type {
	case "", DBTypeMySQL, DBTypeSQLite: // empty means the default (mysql)
	default:
		return fmt.Errorf("unsupported database type %q (supported: %s, %s)", c.Type, DBTypeMySQL, DBTypeSQLite)
	}

	if c.Host == "" {
		return errors.New("host is required")
	}

	if c.Port == "" {
		return errors.New("port is required")
	}

	if c.User == "" {
		return errors.New("user is required")
	}

	if c.DBName == "" {
		return errors.New("database name is required")
	}

	if c.MaxOpenConns <= 0 {
		return errors.New("max open connections must be positive")
	}

	if c.MaxIdleConns <= 0 {
		return errors.New("max idle connections must be positive")
	}

	if c.ConnMaxLifetime <= 0 {
		return errors.New("connection max lifetime must be positive")
	}

	return nil
}

func (c *ServerConfig) Validate() error {
	if c.Port == "" {
		return errors.New("port is required")
	}

	if c.ReadTimeout <= 0 {
		return errors.New("read timeout must be positive")
	}

	// WriteTimeout of 0 is valid — it disables the server-level write timeout
	// (per-write deadlines are handled in the WebSocket write pump instead).
	if c.WriteTimeout < 0 {
		return errors.New("write timeout must be non-negative (0 to disable)")
	}

	if c.IdleTimeout <= 0 {
		return errors.New("idle timeout must be positive")
	}

	if c.RateLimit <= 0 {
		return errors.New("RATE_LIMIT must be positive")
	}

	if c.LoginRateLimit <= 0 {
		return errors.New("LOGIN_RATE_LIMIT must be positive")
	}

	return nil
}

// MySQLSessionParams sets the session variables of every MySQL connection.
// The driver (go-sql-driver/mysql) runs `SET <name>=<value>` for each DSN
// parameter it does not know, on every new connection of the pool. A `SET`
// statement after connecting reached only one pooled connection.
//   - sql_mode: the MySQL 8.0/8.4 default, explicitly, so the mode does not
//     depend on the server configuration (a managed database can differ).
//   - time_zone: UTC, the same as loc=UTC on the client side.
//   - foreign_key_checks: on.
//
// Values are URL-encoded: %27 is ', %2C is , and %2B is + (a plain + would
// decode to a space).
//
// The value starts with "&": append it after an existing "?query" of the DSN
// (DSN appends it after "?charset=...").
const MySQLSessionParams = "&sql_mode=%27ONLY_FULL_GROUP_BY%2CSTRICT_TRANS_TABLES%2CNO_ZERO_IN_DATE%2CNO_ZERO_DATE%2CERROR_FOR_DIVISION_BY_ZERO%2CNO_ENGINE_SUBSTITUTION%27" +
	"&time_zone=%27%2B00%3A00%27" +
	"&foreign_key_checks=1"

// DSN returns the database connection string
func (c *DatabaseConfig) DSN() string {
	// Use a builder for better performance and readability
	var b strings.Builder

	b.WriteString(c.User)

	if c.Password != "" {
		b.WriteByte(':')
		b.WriteString(c.Password)
	}

	b.WriteString("@tcp(")
	b.WriteString(c.Host)
	b.WriteByte(':')
	b.WriteString(c.Port)
	b.WriteByte(')')
	b.WriteByte('/')
	b.WriteString(c.DBName)
	b.WriteString("?charset=utf8mb4&parseTime=True&loc=UTC")
	b.WriteString(MySQLSessionParams)

	if c.MaxOpenConns > 0 {
		b.WriteString("&maxAllowedPacket=0") // Let server control packet size
	}

	return b.String()
}

// LoadConfig loads configuration from environment variables
func LoadConfig() (*Config, error) {
	// Load .env file if it exists
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}

	// #nosec G703 -- envFile is the operator-set ENV_FILE path (process environment), not request input.
	if _, err := os.Stat(envFile); err == nil {
		if err := godotenv.Load(envFile); err != nil {
			return nil, fmt.Errorf("error loading %s: %w", envFile, err)
		}
	}

	cfg := &Config{
		App: AppConfig{
			Name:        getEnv("APP_NAME", "backend-api"),
			Environment: getEnv("GO_ENV", "development"),
			Title:       getEnv("APP_TITLE", DefaultAppTitle),
			Debug:       getEnvBool("APP_DEBUG", true),
		},
		Database: DatabaseConfig{
			Type:            strings.ToLower(strings.TrimSpace(getEnv("DB_TYPE", DBTypeMySQL))),
			Host:            getEnv("DB_HOST", "localhost"),
			Port:            getEnv("DB_PORT", "3306"),
			User:            getEnv("DB_USER", "root"),
			Password:        getEnv("DB_PASSWORD", ""),
			DBName:          getEnv("DB_NAME", "app"),
			MaxOpenConns:    getEnvInt32("DB_MAX_OPEN_CONNS", defaultMaxOpenConns),
			MaxIdleConns:    getEnvInt32("DB_MAX_IDLE_CONNS", defaultMaxIdleConns),
			ConnMaxLifetime: getEnvDuration("DB_CONN_MAX_LIFETIME", defaultConnMaxLifetime),
		},
		Server: ServerConfig{
			Host:            getEnv("SERVER_HOST", ""),
			Port:            getEnv("SERVER_PORT", "8081"),
			ReadTimeout:     getEnvDuration("SERVER_READ_TIMEOUT", defaultReadTimeout),
			WriteTimeout:    getEnvDuration("SERVER_WRITE_TIMEOUT", defaultWriteTimeout),
			IdleTimeout:     getEnvDuration("SERVER_IDLE_TIMEOUT", defaultIdleTimeout),
			ShutdownTimeout: getEnvDuration("SERVER_SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
			EnableSwagger:   getEnvBool("ENABLE_SWAGGER", false),
		},
		CORS: CORSConfig{
			AllowedOrigins: getEnv("CORS_ALLOWED_ORIGINS", "*"),
		},
		Logging: LogConfig{
			Level: getEnv("LOG_LEVEL", "info"),
			File:  getEnv("LOG_FILE", ""),
		},
	}

	var err error

	if cfg.Auth, err = loadAuthConfig(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	if cfg.OIDC, err = loadOIDCConfig(cfg.Auth.Enabled); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	if cfg.Otel, err = loadOtelConfig(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	if err = loadPprofConfig(&cfg.Server); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	if cfg.Replica, err = loadReplicaConfig(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	trustedProxies, err := ParseTrustedProxies(getEnv("TRUSTED_PROXIES", ""))
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	cfg.Server.TrustedProxies = trustedProxies

	// A rate limit that is set but not a number is an error: a silent
	// fallback to the default would hide a typo in a security setting.
	if cfg.Server.RateLimit, err = parseEnvInt32Strict("RATE_LIMIT", 100); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	if cfg.Server.LoginRateLimit, err = parseEnvInt32Strict("LOGIN_RATE_LIMIT", 10); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Validate the configuration (this also rejects an invalid LOG_LEVEL)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	cfg.LogLevel, _ = cfg.Logging.SlogLevel() // Validate checked the value

	return cfg, nil
}

// loadAuthConfig reads the auth variables. AUTH_ENABLED is parsed strictly: a
// value that is set but not a boolean is an error, because a silent fallback
// to false would turn authentication off on a typo. With AUTH_ENABLED=true a duration
// that is set but does not parse is an error (a silent fallback would hide a
// typo in a security setting); with auth off the durations are read leniently
// because nothing uses them.
func loadAuthConfig() (AuthConfig, error) {
	enabled, err := parseEnvBoolStrict("AUTH_ENABLED", false)
	if err != nil {
		return AuthConfig{}, err
	}

	cfg := AuthConfig{
		Enabled:          enabled,
		JWTSecret:        getEnv("JWT_SECRET", ""),
		AdminUsername:    getEnv("ADMIN_USERNAME", defaultAdminUsername),
		AdminPassword:    getEnv("ADMIN_PASSWORD", ""),
		SelfRegistration: getEnvBool("SELF_REGISTRATION", false),
		SessionStore:     strings.ToLower(strings.TrimSpace(getEnv("SESSION_STORE", SessionStoreMySQL))),
		SecureCookies:    true,
	}

	// SECURE_COOKIES is strict with auth on: a typo must not drop the Secure
	// attribute. With auth off it is unused and keeps the default.
	if enabled {
		if cfg.SecureCookies, err = parseEnvBoolStrict("SECURE_COOKIES", true); err != nil {
			return AuthConfig{}, err
		}
	}

	durations := []struct {
		dst      *time.Duration
		key      string
		fallback time.Duration
	}{
		{&cfg.JWTExpiration, "JWT_EXPIRATION", defaultJWTExpiration},
		{&cfg.AccessTokenExpiration, "ACCESS_TOKEN_EXPIRATION", defaultAccessTokenExpiration},
		{&cfg.SessionIdleTimeout, "SESSION_IDLE_TIMEOUT", defaultSessionIdleTimeout},
		{&cfg.SessionMaxLifetime, "SESSION_MAX_LIFETIME", defaultSessionMaxLifetime},
		{&cfg.RefreshTokenExpiration, "REFRESH_TOKEN_EXPIRATION", defaultRefreshTokenExpiration},
		{&cfg.RefreshReuseGrace, "REFRESH_REUSE_GRACE", defaultRefreshReuseGrace},
	}

	for _, d := range durations {
		if !enabled {
			*d.dst = getEnvDuration(d.key, d.fallback)
			continue
		}

		v, err := parseEnvDurationStrict(d.key, d.fallback)
		if err != nil {
			return AuthConfig{}, err
		}

		*d.dst = v
	}

	return cfg, nil
}

// loadOtelConfig reads the telemetry variables. The two switches are parsed
// strictly: a typo such as OTEL_ENABLED=ture would otherwise turn telemetry
// off without a word. The sample rate is strict only with OTEL_ENABLED=true;
// with it off nothing reads the value.
func loadOtelConfig() (OtelConfig, error) {
	enabled, err := parseEnvBoolStrict("OTEL_ENABLED", false)
	if err != nil {
		return OtelConfig{}, err
	}

	metrics, err := parseEnvBoolStrict("METRICS_ENABLED", false)
	if err != nil {
		return OtelConfig{}, err
	}

	cfg := OtelConfig{
		Enabled:        enabled,
		MetricsEnabled: metrics,
		Endpoint:       strings.TrimSpace(getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")),
		ServiceName:    getEnv("OTEL_SERVICE_NAME", "backend-api"),
		MetricsAddr:    strings.TrimSpace(getEnv("METRICS_ADDR", DefaultMetricsAddr)),
		SampleRate:     1,
	}

	if metrics {
		if err = validateListenAddr("METRICS_ADDR", cfg.MetricsAddr); err != nil {
			return OtelConfig{}, err
		}
	}

	if !enabled {
		cfg.SampleRate = getEnvFloat64("OTEL_TRACE_SAMPLE_RATE", 1)

		return cfg, nil
	}

	if cfg.SampleRate, err = parseEnvFloat64Strict("OTEL_TRACE_SAMPLE_RATE", 1); err != nil {
		return OtelConfig{}, err
	}

	if cfg.Endpoint, err = normalizeOTLPEndpoint(cfg.Endpoint); err != nil {
		return OtelConfig{}, err
	}

	return cfg, nil
}

// normalizeOTLPEndpoint reduces OTEL_EXPORTER_OTLP_ENDPOINT to the host:port
// that the exporters take. It accepts that form and the standard URL form of
// the variable with an http:// scheme (and at most a trailing slash). An
// https:// URL is refused: the exporters use plain gRPC, so accepting it would
// run unencrypted although the operator asked for TLS. A path, query,
// fragment or credentials are refused too: the exporters would ignore them.
func normalizeOTLPEndpoint(raw string) (string, error) {
	const name = "OTEL_EXPORTER_OTLP_ENDPOINT"

	if !strings.Contains(raw, "://") {
		return raw, nil
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s must be host:port or an http:// URL", name)
	}

	switch strings.ToLower(u.Scheme) {
	case "http":
	case "https":
		return "", fmt.Errorf("%s: https is not supported (the OTLP connection is not encrypted); use host:port or an http:// URL to a collector on a trusted network", name)
	default:
		return "", fmt.Errorf("%s must be host:port or an http:// URL", name)
	}

	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
		return "", fmt.Errorf("%s must be host:port or an http:// URL without path, query or credentials", name)
	}

	return u.Host, nil
}

// parseEnvBoolStrict reads a boolean from the environment. Surrounding
// whitespace is ignored. An unset or empty variable gives the fallback; a
// value that strconv.ParseBool does not accept is an error that names the
// variable and does not repeat the value.
func parseEnvBoolStrict(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}

	v, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (true or false)", key)
	}

	return v, nil
}

// parseEnvDurationStrict reads a duration from the environment. An unset or
// empty variable gives the fallback; a value that does not parse is an error.
func parseEnvDurationStrict(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}

	v, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 15m or 12h", key)
	}

	return v, nil
}

// Helper functions for environment variables
func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	return value
}

func getEnvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	v, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}

	return v
}

//nolint:wsl // Function layout has been made consistent with other helpers
func getEnvInt32(key string, fallback int32) int32 {
	if value := os.Getenv(key); value != "" {
		v, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return fallback
		}
		return int32(v)
	}
	return fallback
}

// parseEnvInt32Strict reads an int32 from the environment. An unset or empty
// variable gives the fallback; a value that is not a valid int32 is an error.
func parseEnvInt32Strict(key string, fallback int32) (int32, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}

	v, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, errors.Unwrap(err))
	}

	return int32(v), nil
}

func getEnvFloat64(key string, fallback float64) float64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fallback
	}

	return v
}

// parseEnvFloat64Strict reads a float from the environment. An unset or empty
// variable gives the fallback; a value that does not parse is an error that
// names the variable.
func parseEnvFloat64Strict(key string, fallback float64) (float64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}

	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number from 0 to 1", key)
	}

	return v, nil
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	v, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}

	return v
}

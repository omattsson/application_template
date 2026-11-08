// Package config provides configuration loading and validation for the application.
// It handles environment variables, default values, and config validation for all components.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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
	CORS    CORSConfig
	Logging LogConfig
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
	defaultAdminUsername         = "admin"
)

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
	// JWTSecret is JWT_SECRET: the HMAC key of the access tokens. It has no
	// default. Generate it (for example `openssl rand -base64 48`); it is
	// required, at least MinJWTSecretLength characters, when Enabled is true.
	// It is never logged or put in an error.
	JWTSecret string
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
}

// Validate checks the auth settings. LoadConfig calls it only when Enabled is
// true. Error messages never contain the secret.
func (c *AuthConfig) Validate() error {
	if c.JWTSecret == "" {
		return errors.New("jwt_secret is required when AUTH_ENABLED is true")
	}

	// Count characters, not bytes: a multi-byte character is one character,
	// so the check is never weaker than the documented minimum.
	if utf8.RuneCountInString(c.JWTSecret) < MinJWTSecretLength {
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

	if c.AccessTokenExpiration > c.SessionIdleTimeout {
		return errors.New("access_token_expiration must not exceed session_idle_timeout")
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
	// 4-byte fields
	// RateLimit is RATE_LIMIT: requests per minute per client IP (default 100).
	RateLimit int32
	// LoginRateLimit is LOGIN_RATE_LIMIT: login attempts per minute per client
	// IP (default 10).
	LoginRateLimit int32
	// 1-byte fields
	// EnableSwagger is ENABLE_SWAGGER (default false).
	EnableSwagger bool
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

	// With auth off the auth settings are not used, so a missing secret is fine.
	if c.Auth.Enabled {
		if err := c.Auth.Validate(); err != nil {
			return fmt.Errorf("auth config: %w", err)
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

// loadAuthConfig reads the auth variables. With AUTH_ENABLED=true a duration
// that is set but does not parse is an error (a silent fallback would hide a
// typo in a security setting); with auth off the durations are read leniently
// because nothing uses them.
func loadAuthConfig() (AuthConfig, error) {
	enabled := getEnvBool("AUTH_ENABLED", false)

	cfg := AuthConfig{
		Enabled:          enabled,
		JWTSecret:        getEnv("JWT_SECRET", ""),
		AdminUsername:    getEnv("ADMIN_USERNAME", defaultAdminUsername),
		AdminPassword:    getEnv("ADMIN_PASSWORD", ""),
		SelfRegistration: getEnvBool("SELF_REGISTRATION", false),
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

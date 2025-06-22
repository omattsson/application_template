package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all configuration for the application
type Config struct {
	App        AppConfig
	Database   DatabaseConfig
	AzureTable AzureTableConfig
	Server     ServerConfig
	Logging    LogConfig
}

// AppConfig holds application-wide configuration
type AppConfig struct {
	Name        string
	Environment string
	Debug       bool
}

// DatabaseConfig holds database-specific configuration
type DatabaseConfig struct {
	Host            string
	Port            string
	User            string
	Password        string
	DBName          string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// AzureTableConfig holds Azure Table Storage configuration
type AzureTableConfig struct {
	AccountName   string
	AccountKey    string
	Endpoint      string
	TableName     string
	UseAzureTable bool // true to use Azure Table as backend
	UseAzurite    bool // true to use local Azurite emulator
}

// ServerConfig holds HTTP server configuration
type ServerConfig struct {
	Host            string
	Port            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

// LogConfig holds logging configuration
type LogConfig struct {
	Level string
	File  string
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if err := c.App.Validate(); err != nil {
		return fmt.Errorf("app config: %w", err)
	}
	if err := c.Database.Validate(); err != nil {
		return fmt.Errorf("database config: %w", err)
	}
	if err := c.Server.Validate(); err != nil {
		return fmt.Errorf("server config: %w", err)
	}
	return nil
}

func (c *AppConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if c.Environment == "" {
		return fmt.Errorf("environment is required")
	}
	return nil
}

func (c *DatabaseConfig) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("host is required")
	}
	if c.Port == "" {
		return fmt.Errorf("port is required")
	}
	if c.User == "" {
		return fmt.Errorf("user is required")
	}
	if c.DBName == "" {
		return fmt.Errorf("database name is required")
	}
	if c.MaxOpenConns <= 0 {
		return fmt.Errorf("max open connections must be positive")
	}
	if c.MaxIdleConns <= 0 {
		return fmt.Errorf("max idle connections must be positive")
	}
	if c.ConnMaxLifetime <= 0 {
		return fmt.Errorf("connection max lifetime must be positive")
	}
	return nil
}

func (c *AzureTableConfig) Validate() error {
	if c.AccountName == "" {
		return fmt.Errorf("account name is required")
	}
	if c.AccountKey == "" {
		return fmt.Errorf("account key is required")
	}
	if c.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if c.TableName == "" {
		return fmt.Errorf("table name is required")
	}
	return nil
}

func (c *ServerConfig) Validate() error {
	if c.Port == "" {
		return fmt.Errorf("port is required")
	}
	if c.ReadTimeout <= 0 {
		return fmt.Errorf("read timeout must be positive")
	}
	if c.WriteTimeout <= 0 {
		return fmt.Errorf("write timeout must be positive")
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("idle timeout must be positive")
	}
	return nil
}

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
	b.WriteString("?charset=utf8mb4&parseTime=True&loc=Local")

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

	if _, err := os.Stat(envFile); err == nil {
		if err := godotenv.Load(envFile); err != nil {
			return nil, fmt.Errorf("error loading %s: %w", envFile, err)
		}
	}

	cfg := &Config{
		App: AppConfig{
			Name:        getEnv("APP_NAME", "backend-api"),
			Environment: getEnv("GO_ENV", "development"),
			Debug:       getEnvBool("APP_DEBUG", true),
		},
		Database: DatabaseConfig{
			Host:            getEnv("DB_HOST", "localhost"),
			Port:            getEnv("DB_PORT", "3306"),
			User:            getEnv("DB_USER", "root"),
			Password:        getEnv("DB_PASSWORD", ""),
			DBName:          getEnv("DB_NAME", "app"),
			MaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: getEnvDuration("DB_CONN_MAX_LIFETIME", 5*time.Minute),
		},
		AzureTable: AzureTableConfig{
			AccountName:   getEnv("AZURE_TABLE_ACCOUNT_NAME", ""),
			AccountKey:    getEnv("AZURE_TABLE_ACCOUNT_KEY", ""),
			Endpoint:      getEnv("AZURE_TABLE_ENDPOINT", ""),
			TableName:     getEnv("AZURE_TABLE_NAME", "items"),
			UseAzureTable: getEnvBool("USE_AZURE_TABLE", false),
			UseAzurite:    getEnvBool("USE_AZURITE", false),
		},
		Server: ServerConfig{
			Host:            getEnv("SERVER_HOST", ""),
			Port:            getEnv("SERVER_PORT", "8081"),
			ReadTimeout:     getEnvDuration("SERVER_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    getEnvDuration("SERVER_WRITE_TIMEOUT", 10*time.Second),
			IdleTimeout:     getEnvDuration("SERVER_IDLE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: getEnvDuration("SERVER_SHUTDOWN_TIMEOUT", 30*time.Second),
		},
		Logging: LogConfig{
			Level: getEnv("LOG_LEVEL", "info"),
			File:  getEnv("LOG_FILE", ""),
		},
	}

	// Validate the configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// Helper functions for environment variables
func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if value := os.Getenv(key); value != "" {
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fallback
		}
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		v, err := strconv.Atoi(value)
		if err != nil {
			return fallback
		}
		return v
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		v, err := time.ParseDuration(value)
		if err != nil {
			return fallback
		}
		return v
	}
	return fallback
}

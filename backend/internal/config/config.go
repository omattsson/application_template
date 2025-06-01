package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port string
	Env  string
}

func LoadConfig() Config {
	// Try to load .env file, but don't fail if it doesn't exist
	_ = godotenv.Load()

	return Config{
		Port: getEnv("PORT", "8081"),
		Env:  getEnv("ENV", "development"),
	}
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

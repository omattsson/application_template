//go:build integration

package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/database"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// integrationDBConfig is the database of make test-backend-all.
func integrationDBConfig() config.DatabaseConfig {
	return config.DatabaseConfig{
		Host:            getenvDefault("DB_HOST", "localhost"),
		Port:            getenvDefault("DB_PORT", "3306"),
		User:            getenvDefault("DB_USER", "appuser"),
		Password:        getenvDefault("DB_PASSWORD", "apppass"),
		DBName:          getenvDefault("DB_NAME", "app"),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
	}
}

func TestDatabaseInitialization(t *testing.T) {
	t.Parallel()
	// Use mock config instead of loading from environment
	cfg := &config.Config{
		App: config.AppConfig{
			Name:        "testapp",
			Environment: "testing",
			Debug:       true,
		},
		Database: integrationDBConfig(),
		Server: config.ServerConfig{
			Host:            "localhost",
			Port:            "8082",
			ReadTimeout:     10 * time.Second,
			WriteTimeout:    time.Duration(0),
			ShutdownTimeout: 30 * time.Second,
		},
		Logging: config.LogConfig{
			Level: "debug",
			File:  "",
		},
	}

	// Create a mock database instance
	db, err := database.NewFromAppConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, db)
}

// TestAuthLoginLogoutAgainstMySQL is Review Focus 2 against real MySQL with
// AUTH_ENABLED=true and the MySQL session store: login, an authenticated call
// and an open WebSocket work; after logout the token is refused, the socket
// is closed and a new upgrade with the token is refused.
func TestAuthLoginLogoutAgainstMySQL(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)

	cfg := bootstrapTestConfig()
	cfg.Database = integrationDBConfig()
	// A fresh admin name: the database is shared with other runs.
	admin := "it-admin-" + uuid.NewString()[:8]
	const password = "integration-admin-password"
	cfg.Auth = config.AuthConfig{
		Enabled:                true,
		JWTSecret:              "integration-test-secret-0123456789",
		JWTExpiration:          time.Hour,
		AccessTokenExpiration:  15 * time.Minute,
		SessionIdleTimeout:     30 * time.Minute,
		SessionMaxLifetime:     12 * time.Hour,
		RefreshTokenExpiration: 168 * time.Hour,
		RefreshReuseGrace:      30 * time.Second,
		AdminUsername:          admin,
		AdminPassword:          password,
		SessionStore:           config.SessionStoreMySQL,
	}

	repo, err := initDatabase(cfg) // MySQL, with the migrations
	require.NoError(t, err)
	s, router, err := buildServices(cfg, repo)
	require.NoError(t, err)
	srv := startHTTPServer(cfg, router)
	s.servers = append(s.servers, srv)
	base := "http://" + srv.Addr
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, shutdown(ctx, s))
	})

	status, _ := call(t, http.MethodGet, base+"/api/v1/items", "", nil)
	assert.Equal(t, http.StatusUnauthorized, status)

	tok := loginAs(t, base, admin, password)
	status, body := call(t, http.MethodGet, base+"/api/v1/items", "", bearerOf(tok))
	require.Equal(t, http.StatusOK, status, body)

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws?token=" + url.QueryEscape(tok)
	conn, resp, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })

	h := bearerOf(tok)
	h["X-Requested-With"] = "XMLHttpRequest"
	status, body = call(t, http.MethodPost, base+"/api/v1/auth/logout", "", h)
	require.Equal(t, http.StatusOK, status, body)

	status, _ = call(t, http.MethodGet, base+"/api/v1/items", "", bearerOf(tok))
	assert.Equal(t, http.StatusUnauthorized, status, "the logged-out token is refused")

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		_, _, err = conn.ReadMessage()
		if err != nil {
			break
		}
	}
	var ce *gorilla.CloseError
	require.True(t, errors.As(err, &ce), "the open socket is closed: %v", err)
	assert.Equal(t, gorilla.ClosePolicyViolation, ce.Code)

	_, resp, err = gorilla.DefaultDialer.Dial(wsURL, nil)
	require.ErrorIs(t, err, gorilla.ErrBadHandshake)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a new upgrade with the token is refused")
	_ = resp.Body.Close()
}

package main

import (
	"backend/internal/config"
	"backend/internal/health"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// MockDB is a mock implementation of the database.Database interface
type MockDB struct {
	mock.Mock
}

func (m *MockDB) AutoMigrate() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockDB) DB() *gorm.DB {
	args := m.Called()
	return args.Get(0).(*gorm.DB)
}

// MockRepository is a mock implementation of the models.Repository interface
type MockRepository struct {
	mock.Mock
}

func (m *MockRepository) Create(ctx context.Context, entity interface{}) error {
	args := m.Called(ctx, entity)
	return args.Error(0)
}

func (m *MockRepository) FindByID(ctx context.Context, id uint, dest interface{}) error {
	args := m.Called(ctx, id, dest)
	return args.Error(0)
}

func (m *MockRepository) Update(ctx context.Context, entity interface{}) error {
	args := m.Called(ctx, entity)
	return args.Error(0)
}

func (m *MockRepository) Delete(ctx context.Context, entity interface{}) error {
	args := m.Called(ctx, entity)
	return args.Error(0)
}

func (m *MockRepository) List(ctx context.Context, dest interface{}, conditions ...interface{}) error {
	args := m.Called(ctx, dest, conditions)
	return args.Error(0)
}

func (m *MockRepository) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockRepository) Close() error {
	args := m.Called()
	return args.Error(0)
}

// MockSQLDB is a mock implementation of the sql.DB interface
type MockSQLDB struct {
	mock.Mock
}

func (m *MockSQLDB) Ping() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockSQLDB) Close() error {
	args := m.Called()
	return args.Error(0)
}

// setupTestEnv sets the environment variables of the config test. t.Setenv
// restores them when the test ends, so they cannot leak into the other tests
// of the process (TestDatabaseInitialization reads DB_HOST).
func setupTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("APP_NAME", "testapp")
	t.Setenv("GO_ENV", "testing")
	t.Setenv("APP_DEBUG", "true")
	t.Setenv("DB_HOST", "testhost")
	t.Setenv("DB_PORT", "3306")
	t.Setenv("DB_USER", "testuser")
	t.Setenv("DB_PASSWORD", "testpass")
	t.Setenv("DB_NAME", "testdb")
	t.Setenv("SERVER_HOST", "localhost")
	t.Setenv("SERVER_PORT", "8082") // Use different port for tests
}

// Mock functions to replace the actual implementations
func mockLoadConfig() (*config.Config, error) {
	return &config.Config{
		App: config.AppConfig{
			Name:        "testapp",
			Environment: "testing",
			Debug:       true,
		},
		Database: config.DatabaseConfig{
			Host:            "testhost",
			Port:            "3306",
			User:            "testuser",
			Password:        "testpass",
			DBName:          "testdb",
			MaxOpenConns:    5,
			MaxIdleConns:    2,
			ConnMaxLifetime: 5 * time.Minute,
		},
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
	}, nil
}

// Tests for main.go functions

func TestConfigLoading(t *testing.T) {
	// Not parallel: t.Setenv changes the process environment, and a serial
	// test restores it before the parallel tests continue.
	setupTestEnv(t)

	// Test LoadConfig directly
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Verify loaded config matches expected values
	assert.Equal(t, "testapp", cfg.App.Name)
	assert.Equal(t, "testing", cfg.App.Environment)
	assert.True(t, cfg.App.Debug)
	assert.Equal(t, "testhost", cfg.Database.Host)
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	// Create a test Gin router
	gin.SetMode(gin.TestMode)
	r := gin.Default()

	// Initialize health checker
	healthChecker := health.New()
	healthChecker.SetReady(true)

	// Register health endpoints
	r.GET("/health/live", func(c *gin.Context) {
		status := healthChecker.CheckLiveness(c.Request.Context())
		c.JSON(http.StatusOK, status)
	})

	r.GET("/health/ready", func(c *gin.Context) {
		status := healthChecker.CheckReadiness(c.Request.Context())
		if status.Status == "DOWN" {
			c.JSON(http.StatusServiceUnavailable, status)
			return
		}
		c.JSON(http.StatusOK, status)
	})

	// Test liveness endpoint
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "/health/live", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "UP")

	// Test readiness endpoint
	w = httptest.NewRecorder()
	req, _ = http.NewRequestWithContext(context.Background(), "GET", "/health/ready", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "UP")

	// Test with failing health check
	healthChecker.AddCheck("test", func(_ context.Context) error {
		return errors.New("test error")
	})

	w = httptest.NewRecorder()
	req, _ = http.NewRequestWithContext(context.Background(), "GET", "/health/ready", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "DOWN")
}

func TestDatabaseHealthCheck(t *testing.T) {
	t.Parallel()
	// Create a new health checker
	healthChecker := health.New()

	// Test with SQLite database
	mockRepo := new(MockRepository)
	mockRepo.On("Ping", mock.Anything).Return(nil)

	// Add database health check
	healthChecker.AddCheck("database", func(_ context.Context) error {
		return mockRepo.Ping(context.Background())
	})

	// Check readiness
	status := healthChecker.CheckReadiness(context.Background())
	assert.Equal(t, "DOWN", status.Status) // Initially DOWN because we haven't set ready

	// Mark as ready
	healthChecker.SetReady(true)

	// Check again
	status = healthChecker.CheckReadiness(context.Background())
	assert.Equal(t, "UP", status.Status)
	assert.Equal(t, "UP", status.Checks["database"].Status)

	// Test with failing database connection
	mockRepo = new(MockRepository)
	mockRepo.On("Ping", mock.Anything).Return(errors.New("connection failed"))

	healthChecker = health.New()
	healthChecker.SetReady(true)
	healthChecker.AddCheck("database", func(_ context.Context) error {
		return mockRepo.Ping(context.Background())
	})

	status = healthChecker.CheckReadiness(context.Background())
	assert.Equal(t, "DOWN", status.Status)
	assert.Equal(t, "DOWN", status.Checks["database"].Status)
	assert.Contains(t, status.Checks["database"].Message, "connection failed")
}

func TestServerConfiguration(t *testing.T) {
	t.Parallel()
	// Test server configuration based on config
	cfg, _ := mockLoadConfig()

	// Configure server address
	addr := cfg.Server.Host + ":" + cfg.Server.Port
	assert.Equal(t, "localhost:8082", addr)

	// Configure server timeouts
	server := &http.Server{
		Addr:         addr,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	assert.Equal(t, addr, server.Addr)
	assert.Equal(t, 10*time.Second, server.ReadTimeout)
	assert.Equal(t, time.Duration(0), server.WriteTimeout)
}

# Backend Service (Go + Gin)

A robust Go backend service built with the Gin framework, following clean architecture principles and modern Go practices.

## Features

- 🚀 High-performance REST API with Gin
- 📝 Auto-generated Swagger/OpenAPI documentation
- 🔍 Built-in health check endpoints
- 🔐 Middleware support for logging, CORS, and more
- 🧪 Comprehensive test coverage
- 🐳 Docker support for development and production
- 🏗️ Clean architecture with proper separation of concerns

## Project Structure

```text
backend/
├── api/              # Main application entry point
│   └── main.go
├── cmd/             # Command line tools
├── docs/            # Auto-generated Swagger documentation
├── internal/        # Private application code
│   ├── api/        # API implementation
│   │   ├── handlers/
│   │   ├── middleware/
│   │   └── routes/
│   ├── config/     # Configuration handling
│   ├── health/     # Health check implementation
│   └── models/     # Data models
├── pkg/            # Public packages
│   └── utils/      # Utility functions
├── Dockerfile      # Multi-stage Docker build
├── go.mod         # Go module definition
└── Makefile       # Build and development commands
```

## Prerequisites

- Go 1.24 or later
- Docker and Docker Compose (for containerized development)
- Make (optional, for using Makefile commands)

## Development Setup

1. **Install dependencies**:

   ```bash
   go mod download
   ```

2. **Start the server**:

   With Docker (recommended):

   ```bash
   docker compose up backend
   ```

   Without Docker:

   ```bash
   go run api/main.go
   ```

   The API will be available at [http://localhost:8081](http://localhost:8081)

## Available Commands

### Development

- `make build` - Build the application
- `make run` - Run the application locally
- `make test` - Run tests
- `make test-coverage` - Run tests with coverage report
- `make swagger` - Generate Swagger documentation
- `make fmt` - Format code
- `make lint` - Run linters
- `make clean` - Clean build artifacts

### Docker Commands

- Development: `docker compose up backend`
- Production: `docker compose -f docker-compose.prod.yml up backend`
- Build: `docker build -t backend .`

## API Documentation

The API documentation is automatically generated using Swagger/OpenAPI:

- Local: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html)
- Generate: `make swagger`

## Health Checks

The service provides health check endpoints:

- Liveness: `GET /health/live`
- Readiness: `GET /health/ready`

## Configuration

Configuration is handled through environment variables:

- `GO_ENV` - Environment (development/production)
- `GIN_MODE` - Gin framework mode (debug/release)
- `PORT` - Server port (default: 8081)
- `LOG_LEVEL` - Logging level (debug/info/warn/error)

## Testing

The project includes comprehensive unit tests:

```bash
# Run all tests
make test

# Run tests with coverage
make test-coverage

# Run specific package tests
go test ./internal/api/...
```

## Directory Structure Details

### `/api`

Contains the main application entry point (`main.go`).

### `/internal`

Private application code:

- `/api` - HTTP API implementation
  - `/handlers` - Request handlers
  - `/middleware` - HTTP middleware
  - `/routes` - Route definitions
- `/config` - Configuration management
- `/health` - Health check implementation
- `/models` - Data models

### `/pkg`

Public packages that can be imported by other projects:

- `/utils` - Shared utility functions

## Contributing

1. Follow Go best practices and project conventions
2. Add tests for new features
3. Update documentation
4. Format code (`make fmt`)
5. Ensure all tests pass (`make test`)

## Error Handling

We follow standard Go error handling practices:

```go
if err != nil {
    return fmt.Errorf("operation failed: %w", err)
}
```

## Logging

The application uses structured logging with log levels:

```go
logger.Info("server starting", "port", config.Port)
```

## Dependencies

Key dependencies:

- `gin-gonic/gin` - Web framework
- `swaggo/swag` - Swagger generation
- `stretchr/testify` - Testing toolkit

## Docker Support

The project includes a multi-stage Dockerfile optimized for both development and production:

- Development: Uses Go modules directly, enables hot reload
- Production: Multi-stage build for minimal image size
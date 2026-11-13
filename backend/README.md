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

- Go 1.27 or later (see `go.mod`)
- Docker and Docker Compose (for containerized development)
- Make (optional, for using Makefile commands)

## Development Setup

1. **Install dependencies**:

   ```bash
   go mod download
   ```

2. **Start the server**:

   With Docker (recommended), from the repository root:

   ```bash
   make dev-backend
   ```

   It builds the development image (`GO_ENV=development`, hot reload with
   air) and sets `APP_DEBUG=true` and `ENABLE_SWAGGER=true`. A plain
   `docker compose up backend` builds the production image.

   Without Docker:

   ```bash
   go run ./api
   ```

   The API will be available at [http://localhost:8081](http://localhost:8081)
   (in Compose, host port `BACKEND_PORT`, default 8081; the container always
   listens on 8081).

## Available Commands

### Development

- `make build` - Build the application
- `make run` - Run the application locally
- `make test` - Run tests
- `make test-coverage` - Run tests with coverage report
- `make docs` - Generate Swagger documentation
- `make fmt` - Format code
- `make lint` - Run linters
- `make clean` - Clean build artifacts

### Docker Commands

- Development: `make dev-backend` (repository root)
- Production: `make prod-backend` (repository root; API on host port 8080)
- Build: `docker build -t backend .`

## API Documentation

The API documentation is automatically generated using Swagger/OpenAPI:

- Local: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html), served only with `ENABLE_SWAGGER=true` (default: false)
- Generate: `make docs`

## Health Checks

The service provides health check endpoints:

- Liveness: `GET /health/live`
- Readiness: `GET /health/ready`

## Configuration

Configuration is handled through environment variables:

- `GO_ENV` - Environment label (default `development`; nothing branches on it; Compose uses it to pick the image target)
- `APP_DEBUG` - Gin debug mode when true, release mode when false
- `GIN_MODE` - Optional; when set it wins over `APP_DEBUG`
- `SERVER_PORT` - Server port inside the process or container (default: 8081)
- `AUTH_ENABLED` - Optional authentication, off by default; see the root README "Authentication"
- `TRUSTED_PROXIES`, `RATE_LIMIT`, `LOGIN_RATE_LIMIT` - Client IP behind a proxy and the per-IP limits; see "Behind a proxy" in the root README and `.env.example`
- `LOG_LEVEL` - Logging level (debug/info/warn/error)
- `ENABLE_SWAGGER` - Serve the Swagger UI at `/swagger/index.html` (default: false)

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
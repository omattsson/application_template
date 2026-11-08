# Full-Stack Application Template

A modern, production-ready template for full-stack web applications using Go (Gin) for the backend and React (TypeScript + Vite) for the frontend.

## Features

### Backend (Go + Gin)

- 🚀 Modular Go architecture with clean separation of concerns
- 🔍 Built-in health checks and monitoring
- 📝 Swagger/OpenAPI documentation
- 🧪 Comprehensive test suite
- 🛠️ Development and production Docker configurations
- 🔒 Middleware support (logging, recovery, CORS)

### Frontend (React + TypeScript)

- ⚡️ Vite for lightning-fast development
- 🎨 Modern UI components
- 📱 Responsive layout structure
- 🔄 API client with axios
- 🛣️ React Router for navigation
- 🐳 Docker support for development and production

### Development Workflow

- 📋 Standardized PR templates for different types of changes
- 📊 Project structure for maintainable, scalable applications
- 🔄 Clear contribution guidelines

## Quick Start

### Prerequisites

- Docker and Docker Compose
- Go 1.24 or later (for local development)
- Node.js 20 or later (for local development)
- Make (optional, for using Makefile commands)

### Development Setup

1. **Clone the template:**

   ```bash
   git clone <repository-url> your-project-name
   cd your-project-name
   ```

2. **Install dependencies (if developing locally):**

   ```bash
   make install
   ```

3. **Start the application:**

   Full stack (recommended):

   ```bash
   make dev
   ```

   Backend only:

   ```bash
   make dev-backend
   ```

   Frontend only:

   ```bash
   make dev-frontend
   ```

   The `make dev*` targets build the development images (`GO_ENV=development`:
   hot reload with air; Vite dev server), set `APP_DEBUG=true` (Gin debug mode)
   and `ENABLE_SWAGGER=true`. The services will be available at:

   - Frontend: [http://localhost:3000](http://localhost:3000) (host port `FRONTEND_PORT`, default 3000)
   - Backend: [http://localhost:8081](http://localhost:8081) (host port `BACKEND_PORT`, default 8081; the development frontend calls this address)
   - API Documentation: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html)

   `make prod*` builds the production images (nginx on host port 80, the API on
   host port 8080, `APP_DEBUG=false`, Swagger off).

## Project Structure

```text
.
├── backend/                # Go backend service
│   ├── api/               # API entry point
│   ├── internal/          # Internal packages
│   │   ├── api/          # API implementation
│   │   ├── config/       # Configuration
│   │   ├── health/       # Health checks
│   │   └── models/       # Data models
│   └── pkg/              # Public packages
├── frontend/             # React frontend
│   ├── src/
│   │   ├── api/         # API integration
│   │   ├── components/  # Reusable components
│   │   ├── pages/       # Page components
│   │   └── services/    # Business logic
│   └── public/          # Static assets
└── docker-compose.yml    # Docker services configuration
```

## Available Commands

### Development

- `make dev` - Start both services in development mode
- `make dev-backend` - Start backend in development mode
- `make dev-frontend` - Start frontend in development mode
- `make install` - Install dependencies
- `make test` - Run all tests
- `make test-backend` - Run backend tests
- `make test-frontend` - Run frontend tests
- `make docs` - Generate API documentation
- `make fmt` - Format code
- `make lint` - Run linters

### Production

- `make prod` - Start both services in production mode
- `make prod-backend` - Start backend in production mode
- `make prod-frontend` - Start frontend in production mode

### Maintenance

- `make clean` - Stop and remove containers
- `make prune` - Clean up Docker resources

## Configuration

### Environment Variables

Backend:

- `GO_ENV` - Image target and `APP_ENV` (development/production; `make dev*` sets development, `make prod*` production)
- `APP_DEBUG` - Gin debug mode and SQL values in the log (`make dev*`: true, `make prod*`: false)
- `GIN_MODE` - Optional; when set it wins over `APP_DEBUG` for the Gin mode
- `BACKEND_PORT` - Host port of the API (default: 8081; `make prod*`: 8080). The container listens on 8081.
- `ENABLE_SWAGGER` - Serve the Swagger UI at `/swagger/index.html` (default: false; `make dev*` set it to true)
- `SERVER_SHUTDOWN_TIMEOUT` - Budget of each shutdown step (default: 30s). Compose gives the backend `stop_grace_period: 130s` (4 steps x 30s plus a margin); raise both together.

Frontend:

- `NODE_ENV` - Image target (development: Vite dev server on container port 3000; production: nginx on container port 80)
- `FRONTEND_PORT` - Host port of the web UI (default: 3000; `make prod*`: 80)
- `FRONTEND_CONTAINER_PORT` - Container port of the web UI (default: 3000; `make prod*` sets 80 for nginx)
- `VITE_API_URL` - Backend API URL

## API Documentation

The API documentation is automatically generated using Swagger/OpenAPI. The backend serves the Swagger UI only when `ENABLE_SWAGGER=true` (default: false). `make dev` and `make dev-backend` set it, so in development it is at:

- Development: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html)
- Production: off by default. With `ENABLE_SWAGGER=true` it is at `http://[your-domain]/swagger/index.html`.

To regenerate the API documentation:

```bash
make docs
```

## Testing

The template includes comprehensive testing setups for both frontend and backend:

```bash
# Run all tests
make test

# Run backend tests only
make test-backend

# Run frontend tests only
make test-frontend
```

## Health Checks

Both services include health check endpoints:

- Backend: [http://localhost:8081/health/live](http://localhost:8081/health/live)
- Frontend: Built into the Docker container

## Contributing

We welcome contributions to this project! Please check out our [Contributing Guidelines](.github/CONTRIBUTING.md) for detailed information on how to get started.

### GitHub Templates and Workflows

This repository includes a comprehensive set of templates and workflows to streamline the development process:

#### Pull Request Templates

- **Default Template**: Used automatically when creating a PR
- **Bug Fix Template**: For PRs that fix bugs (`?template=bugfix.md`)
- **Feature Template**: For PRs that add new features (`?template=feature.md`)
- **Documentation Template**: For PRs that update docs (`?template=documentation.md`)

To use a specialized template, add the query parameter to the PR URL:
```
https://github.com/username/application_template/compare/main...your-branch?template=feature.md
```

#### Issue Templates

- **Bug Report**: For reporting bugs and unexpected behavior
- **Feature Request**: For suggesting new features or improvements
- **Documentation Issue**: For reporting issues with documentation

#### Other Resources

- **Code of Conduct**: Our [Code of Conduct](.github/CODE_OF_CONDUCT.md) outlines expectations for participation
- **CODEOWNERS**: Automatically assigns reviewers based on the files changed
- **GitHub Actions**: Automatically validates pull requests with linting, testing, and security scans

### Getting Started

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'feat: add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request using one of our templates

- **Default Template**: Used automatically when creating a PR
- **Bug Fix Template**: For PRs that fix bugs (`?template=bugfix.md`)
- **Feature Template**: For PRs that add new features (`?template=feature.md`) 
- **Documentation Template**: For PRs that update docs (`?template=documentation.md`)

To use a specialized template, add the query parameter to the PR URL:
```
https://github.com/your-username/application_template/compare/main...your-branch?template=feature.md
```

## License

This project is licensed under the MIT License - see the LICENSE file for details

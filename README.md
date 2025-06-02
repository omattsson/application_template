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

   The services will be available at:

   - Frontend: [http://localhost:3000](http://localhost:3000)
   - Backend: [http://localhost:8081](http://localhost:8081)
   - API Documentation: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html)

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

- `GO_ENV` - Environment (development/production)
- `GIN_MODE` - Gin framework mode (debug/release)
- `BACKEND_PORT` - Backend port (default: 8081)

Frontend:

- `NODE_ENV` - Environment (development/production)
- `PORT` - Frontend port (default: 3000 in dev, 80 in prod)
- `VITE_API_URL` - Backend API URL

## API Documentation

The API documentation is automatically generated using Swagger/OpenAPI. You can access it at:

- Development: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html)
- Production: [http://[your-domain]/swagger/index.html](http://[your-domain]/swagger/index.html)

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

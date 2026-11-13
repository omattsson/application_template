# Full-Stack Application Template

A modern, production-ready template for full-stack web applications using Go (Gin) for the backend and React (TypeScript + Vite) for the frontend.

## Features

### Backend (Go + Gin)

- 🚀 Modular Go architecture with clean separation of concerns
- 🔍 Built-in health checks and monitoring
- 📝 Swagger/OpenAPI documentation
- 🧪 Comprehensive test suite
- 🛠️ Development and production Docker configurations
- 🔒 Middleware support (logging, recovery, CORS, security headers, rate limiting)
- 🔑 Optional authentication (off by default): local login with refresh sessions, API keys, OIDC single sign-on, user administration, audit log

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
- Go 1.27 or later (for local development; the version of `backend/go.mod`)
- Node.js 22.12 or later (for local development)
- MySQL 8.0.17 or later (Compose runs `mysql:8.0` for you)
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
   - Backend: [http://localhost:8081](http://localhost:8081) (host port `BACKEND_PORT`, default 8081; for direct calls such as curl and Swagger. The web UI does not call this port: it calls its own origin, whose proxy forwards `/api` and `/ws` to the backend)
   - API Documentation: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html)

   `make prod*` builds the production images (nginx on host port 80, the API on
   host port 8080, `APP_DEBUG=false`, Swagger off).

## Project Structure

```text
.
├── backend/                # Go backend service (module `backend`)
│   ├── api/               # Entry point and bootstrap (config, database, server, shutdown)
│   ├── internal/          # Internal packages
│   │   ├── api/          # Routes, handlers, middleware
│   │   ├── config/       # Configuration from environment variables
│   │   ├── database/     # MySQL, migrations, repositories
│   │   ├── models/       # Entities, validation, repository interfaces
│   │   ├── sessionstore/ # Token and session blocklist, OIDC state
│   │   ├── websocket/    # WebSocket hub
│   │   └── health/       # Health checks
│   └── pkg/              # Shared packages
├── frontend/             # React frontend
│   ├── src/
│   │   ├── api/         # API client, session, token refresh
│   │   ├── components/  # Reusable components
│   │   ├── context/     # UI config, auth, WebSocket, theme
│   │   └── pages/       # Page components
│   └── e2e/             # Playwright specs
├── docs/claude/          # Reference docs (structure, auth, operations)
├── scripts/              # Check and test scripts
└── docker-compose.yml    # Docker services configuration
```

The complete file tree is in [docs/claude/structure.md](docs/claude/structure.md).

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

- `GO_ENV` - Image target for Compose (development/production; `make dev*` sets development, `make prod*` production). The backend reads `GO_ENV` as a label only; Compose also passes `APP_ENV` and `PORT`, which the backend does not read (its port is `SERVER_PORT`, default 8081)
- `APP_DEBUG` - Gin debug mode and SQL values in the log (`make dev*`: true, `make prod*`: false)
- `GIN_MODE` - Optional; when set it wins over `APP_DEBUG` for the Gin mode
- `BACKEND_PORT` - Host port of the API (default: 8081; `make prod*`: 8080). The container listens on 8081.
- `ENABLE_SWAGGER` - Serve the Swagger UI at `/swagger/index.html` (default: false; `make dev*` set it to true)
- `TRUSTED_PROXIES` - Proxies allowed to set the client IP (default in Compose: the frontend container, see [Behind a proxy](#behind-a-proxy))
- `RATE_LIMIT` - API requests per minute per client IP (default: 100)
- `LOGIN_RATE_LIMIT` - Login attempts per minute per client IP (default: 10)
- `SERVER_SHUTDOWN_TIMEOUT` - Budget of each shutdown step (default: 30s). Compose gives the backend `stop_grace_period: 170s` (5 steps x 30s plus a margin); raise both together.
- `AUTH_ENABLED`, `JWT_SECRET`, `ADMIN_USERNAME`, `ADMIN_PASSWORD`, `SECURE_COOKIES`, `OIDC_*` - Optional authentication, see [Authentication](#authentication)
- `CORS_ALLOWED_ORIGINS` - Allowed origins (default `*`: for development only; the bundled web UI is same-origin and needs no entry)

Every variable the backend reads, with its default, is in [backend/.env.example](backend/.env.example) and in [docs/claude/operations.md](docs/claude/operations.md); `make check-env-docs` keeps them in step with the code.

Frontend:

- `NODE_ENV` - Image target (development: Vite dev server on container port 3000; production: unprivileged nginx on container port 8080)
- `FRONTEND_PORT` - Host port of the web UI (default: 3000; `make prod*`: 80)
- `FRONTEND_CONTAINER_PORT` - Container port of the web UI (default: 3000; `make prod*` sets 8080 for nginx)
- `VITE_API_URL` - Target of the Vite dev proxy for `/api` and `/ws` (default `http://localhost:8081`; Compose sets `http://backend:8081`). The browser always calls the web UI origin.

### Behind a proxy

The backend rate limits per client IP. It reads the client IP from
`X-Forwarded-For` / `X-Real-IP` only when the request comes from an address in
`TRUSTED_PROXIES`; otherwise the client IP is the TCP peer. With an empty list
(the default outside Compose) every request through a proxy has the proxy's
address, so all users share one rate-limit bucket (`RATE_LIMIT` requests and
`LOGIN_RATE_LIMIT` logins per minute in total).

In Compose, the frontend container (nginx in production, the Vite dev server
in development) is the proxy. It has the fixed address `FRONTEND_PROXY_IP`
(default `172.30.80.10`) on the `frontend-net` network (subnet
`FRONTEND_NET_SUBNET`, default `172.30.80.0/24`), adds the client address to
`X-Forwarded-For`, and the backend trusts exactly that address. Change the
three together if the subnet is in use on your host.

Pitfalls:

- Trust only the proxy addresses, not a whole network: a network gateway is
  the source of traffic from the host on some Docker setups, and a trusted
  gateway lets a client pick its IP. `0.0.0.0/0` and `::/0` are refused at
  startup.
- Behind another load balancer, add its addresses to `TRUSTED_PROXIES` and
  make sure it appends to `X-Forwarded-For` rather than passing a client
  value through unchanged.

## Authentication

Authentication is optional and **off by default** (`AUTH_ENABLED=false`): the API and the web UI are open, so do not expose a deployment in that mode to people you do not trust.

| Mode | Switch | What you get |
|---|---|---|
| Off | `AUTH_ENABLED=false` (default) | No login. The auth, API key, user and audit log routes answer 404. |
| Local login | `AUTH_ENABLED=true` | Username and password, a short-lived access token with a rotating refresh session (httpOnly cookie), API keys, user administration (admin), an audit log of changes. |
| OIDC | `AUTH_ENABLED=true`, `OIDC_ENABLED=true` | Single sign-on added to the local login (authorization code flow with PKCE, roles from a token claim). |

To turn it on, set at least these variables. With Compose, put them in a `.env` file next to `docker-compose.yml` (it is not committed; Compose passes them to the backend). A backend that you run on the host reads `backend/.env` or the environment:

```bash
AUTH_ENABLED=true
JWT_SECRET=$(openssl rand -base64 48)   # required, 16 or more characters, no default
ADMIN_USERNAME=<not "admin">            # the break-glass admin
ADMIN_PASSWORD=<strong, unique>         # 8 to 72 bytes; no admin is created when empty
```

Notes:

- `SECURE_COOKIES` defaults to `true`: the refresh cookie is `__Host-refresh_token` and `Secure`. Browsers accept that on `http://localhost`. On plain HTTP on any other host name set `SECURE_COOKIES=false` (development only).
- The first admin is created at start from `ADMIN_USERNAME` and `ADMIN_PASSWORD`. It is a service account for break-glass use: it can still sign in with the password when OIDC is the only interactive login. An existing user of that name is never changed. Use a name other than `admin` (five wrong passwords lock a name for 15 minutes) and a strong password.
- `SELF_REGISTRATION=true` lets anyone create an account with the role `user`.
- OIDC needs `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` and `OIDC_REDIRECT_URL`. Behind the bundled nginx or the Vite dev server (both strip one `/api`) the redirect URI to register at the identity provider is `<origin>/api/api/v1/auth/oidc/callback`.
- Compose passes the auth variables to the backend. The database tables for users, sessions, API keys and the audit log exist in both modes.

Details, including the session and revocation design: [docs/claude/auth-sessions.md](docs/claude/auth-sessions.md). All variables: [docs/claude/operations.md](docs/claude/operations.md).

## Start a new project from this template

1. **Copy the template.** Clone it (or use "Use this template" on GitHub) into your project name and start a fresh history. Remove `docs/superpowers/`, the design notes of the template itself.
2. **Rename the Go module.** The module is called `backend` and 100 or so files import `backend/...`. Pick your module path (for example `github.com/acme/shop/backend`) and run, from the repository root:

   ```bash
   cd backend
   go mod edit -module github.com/acme/shop/backend
   grep -rl --include='*.go' '"backend/' . | xargs perl -pi -e 's#"backend/#"github.com/acme/shop/backend/#g'
   go build ./... && go test ./... -short
   make docs   # regenerates the Swagger files (needs `swag`)
   ```

3. **Change the title.** The product name is in several places:
   - `APP_TITLE` (backend): the title that the web UI reads from `GET /api/v1/ui-config` (the login page shows it). The default is `Application Template`.
   - `frontend/index.html` (`<title>`), `frontend/src/components/Layout/index.tsx` (app bar and footer) and `frontend/src/pages/Home/index.tsx`.
   - The tests and e2e specs that assert those texts (search for `Full Stack App` and `Full Stack Application`), `frontend/package.json` (`name`) and this README and `WIKI.md`.
   - `@title` in `backend/api/main.go` (then `make docs`), `APP_NAME` in `docker-compose.yml`.
   - `.github/CODEOWNERS`, `.github/SECURITY.md` and `.github/CODE_OF_CONDUCT.md` name contacts that must become yours.
4. **Set the secrets.** For Compose, create a `.env` file next to `docker-compose.yml`; for a backend on the host, copy `backend/.env.example` to `backend/.env` (neither is committed). Set your own values:
   - `JWT_SECRET` and `ADMIN_PASSWORD` have no default (see [Authentication](#authentication)); generate them, never reuse a value from a document.
   - The database credentials in `docker-compose.yml` and `.env.example` (`MYSQL_PASSWORD=apppass`, `MYSQL_ROOT_PASSWORD=rootpass`) are **dev only**. Set your own in the environment of every shared or production deployment, or do not publish the MySQL port.
   - Set `CORS_ALLOWED_ORIGINS` to a list of origins (or leave it unused: the bundled UI is same-origin), `APP_DEBUG=false`, and `TRUSTED_PROXIES` to your proxy addresses.
5. **Enable auth** (see [Authentication](#authentication)). Decide whether to keep local login only or add OIDC, and whether `SELF_REGISTRATION` stays off.
6. **Replace the example.** Items is the example resource. Add your own resources by following [EXTENDING.md](EXTENDING.md), then remove Items (model, repository code, handler, routes, page, tests, `item` entries in the audit constants) when you no longer need it.
7. **Check.** `make lint`, `make test`, `make test-backend-all`, `make test-e2e` and `make test-e2e-auth` must pass. Branch protection and the CI checks are described in [RELEASING.md](RELEASING.md).

## Documentation

| Document | Content |
|---|---|
| [CLAUDE.md](CLAUDE.md) | Rules and commands for AI assistants and developers (also read by GitHub Copilot through `.github/copilot-instructions.md`) |
| [ARCHITECTURE.md](ARCHITECTURE.md) | How the system fits together |
| [EXTENDING.md](EXTENDING.md) | Adding an API resource and a page, optional modules |
| [RELEASING.md](RELEASING.md) | Versions, CI checks, branch protection, releases |
| [docs/claude/](docs/claude/) | File trees, authentication and sessions, operations and every environment variable |
| [WIKI.md](WIKI.md) | Getting started and troubleshooting |

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

# Backend unit and integration tests (starts MySQL in Docker; one package at a time)
make test-backend-all

# End-to-end tests (Playwright): auth off, then auth on
make test-e2e
make test-e2e-auth
```

Before you push, run `make lint` (go vet, ESLint, workflow and environment variable docs checks) and `make lint-docs` (Markdown).

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

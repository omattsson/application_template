# Application Template Wiki

## Overview

This project is a full-stack application template featuring a Go (Gin) backend and a React (Vite) frontend. It is designed for rapid development, clean architecture, and easy deployment with Docker.

---

## Project Structure

```
.
├── backend/                # Go backend service
│   ├── api/               # API entry point
│   ├── internal/          # Internal packages (API, config, database, health, models)
│   ├── pkg/               # Public packages (utils, dberrors)
│   ├── docs/              # Swagger/OpenAPI docs
│   ├── config/            # Configuration files (e.g., MySQL)
│   └── Makefile           # Backend build/test commands
├── frontend/               # React frontend (Vite)
│   ├── src/               # Source code (api, components, pages, services, utils)
│   └── public/            # Static assets
├── docker-compose.yml      # Docker Compose for dev/prod
└── README.md               # Project overview
```

---

## Features

### Backend (Go + Gin)
- Modular, clean architecture
- Auto-generated Swagger/OpenAPI documentation
- Health check endpoints (`/health/live`, `/health/ready`)
- Robust configuration system with `.env` support
- Database migrations and connection pooling
- Comprehensive error handling
- Middleware: logging, recovery, CORS, security headers, rate limiting
- Optional authentication (off by default): local login with refresh sessions, API keys, OIDC single sign-on, user administration, audit log
- WebSocket hub for live events
- Docker support for development and production

### Frontend (React + Vite)
- Modern React with TypeScript
- Fast development with Vite
- Modular component structure
- API client with axios
- Docker support

---

## Getting Started

### Prerequisites
- Go 1.27+ (the version of `backend/go.mod`)
- Node.js 22.12+
- MySQL 8.0.17+ (Docker Compose runs it for you)
- Docker & Docker Compose

### Backend Setup

```bash
make mysql-start                      # from the repository root: MySQL in Docker on localhost:3306
cd backend
cp config/.env.example .env           # the same as .env.example with DB_HOST=localhost
# Edit .env as needed (DB_HOST=db is for a backend inside Docker Compose)
go run ./api
```

For the whole stack with hot reload, run `make dev` from the repository root. See `docs/claude/operations.md` for every variable.

- Swagger docs: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html) (set `ENABLE_SWAGGER=true` in `.env`; default: false)
- Health checks: `/health/live`, `/health/ready`

### Frontend Setup

```bash
cd frontend
npm install
npm run dev
```

- App: [http://localhost:3000](http://localhost:3000). The dev server proxies `/api` and `/ws` to the backend (`VITE_API_URL`, default `http://localhost:8081`), so the browser only ever calls port 3000.

### Docker Compose

```bash
make dev    # development: Vite dev server on http://localhost:3000
make prod   # production: nginx on http://localhost:80
```

These set the image targets and ports for you (see `README.md`); a plain `docker compose up --build` starts the production images with the web UI on host port 3000. Use `make dev-frontend` or `make prod-frontend` for the web UI alone (the backend it depends on starts too).

### Authentication

Off by default (`AUTH_ENABLED=false`: the API is open). To turn it on set `AUTH_ENABLED=true`, `JWT_SECRET` (16 or more characters, no default) and `ADMIN_PASSWORD` (no default), and use an `ADMIN_USERNAME` other than `admin`. `SECURE_COOKIES` defaults to `true`; set it to `false` only for plain HTTP on a host name other than `localhost`. See the "Authentication" section of `README.md` and `docs/claude/auth-sessions.md`.

---

## Testing

- Backend: `make test` or `make test-coverage` (in `backend/`), `make test-backend-all` at the root for the MySQL integration tests
- Frontend: `npm test`
- End to end: `make test-e2e` (auth off) and `make test-e2e-auth` (auth on)

---

## Migrations

- Migrations are run automatically on backend startup.
- To add a migration, add a new versioned function to `internal/database/migrations.go` and register it in `registerMigrations`. Never edit a migration that exists.
- Replicas that start together take a MySQL advisory lock for the migrations and wait up to 5 minutes. MySQL 8.0.17 or later is required.

---

## Troubleshooting

- **Database connection errors:**  
  - Ensure MySQL is running and accessible.
  - Use `DB_HOST=localhost` for local, `DB_HOST=db` for Docker Compose.
- **Duplicate index errors:**  
  - Migrations check for existing indexes before creating them.
- **Login works but the session is lost on reload (auth on):**  
  - On plain HTTP on a host name other than `localhost`, set `SECURE_COOKIES=false`; the browser drops a `Secure` cookie there.
- **OIDC redirect URI mismatch:**  
  - Behind the bundled nginx or the Vite dev server register `<origin>/api/api/v1/auth/oidc/callback` (both proxies strip one `/api`).
- **All users share one rate limit behind a proxy:**  
  - Put the proxy address in `TRUSTED_PROXIES`.

---

## Contributing

- Follow Go and React best practices.
- Add tests for new features.
- Update documentation as needed.
- Project rules and architecture: `CLAUDE.md`, `ARCHITECTURE.md`, `EXTENDING.md`, `RELEASING.md`.

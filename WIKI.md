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
- Middleware: logging, recovery, CORS
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
- Go 1.20+
- Node.js 18+
- Docker & Docker Compose

### Backend Setup

```bash
cd backend
cp .env.example .env
# Edit .env as needed (DB_HOST=localhost for local, DB_HOST=db for Docker)
make dev
```

- Swagger docs: [http://localhost:8081/swagger/index.html](http://localhost:8081/swagger/index.html)
- Health checks: `/health/live`, `/health/ready`

### Frontend Setup

```bash
cd frontend
npm install
npm run dev
```

- App: [http://localhost:3000](http://localhost:3000)

### Docker Compose

```bash
docker compose up --build
```

---

## Testing

- Backend: `make test` or `make test-coverage`
- Frontend: `npm test`

---

## Migrations

- Migrations are run automatically on backend startup.
- To add a migration, edit `internal/database/migrations.go`.

---

## Troubleshooting

- **Database connection errors:**  
  - Ensure MySQL is running and accessible.
  - Use `DB_HOST=localhost` for local, `DB_HOST=db` for Docker Compose.
- **Duplicate index errors:**  
  - Migrations now check for existing indexes before creating them.

---

## Contributing

- Follow Go and React best practices.
- Add tests for new features.
- Update documentation as needed.

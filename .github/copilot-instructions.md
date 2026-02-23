# Copilot Instructions

## Architecture Overview

Full-stack app: **Go (Gin) backend** + **React (TypeScript, Vite, MUI) frontend**, with **MySQL** (GORM) or **Azure Table Storage** as swappable data stores. Docker Compose orchestrates all services.

- **Backend entry point**: `backend/api/main.go` — loads config, initializes DB, sets up Gin router, runs with graceful shutdown.
- **Frontend entry point**: `frontend/src/main.tsx` → `App.tsx` → `routes.tsx` for page routing.
- Backend serves API at `:8081`, frontend at `:3000`. Frontend proxies to backend via axios (`frontend/src/api/config.ts`).

## Backend Structure & Patterns

```
backend/
  api/main.go              # Service bootstrap (config → DB → router → server)
  internal/
    api/routes/routes.go   # All route registration (Gin route groups)
    api/handlers/           # HTTP handlers (items.go uses Handler struct with Repository)
    api/middleware/          # CORS, Logger, Recovery middleware
    config/config.go        # Env-based config with .env support (godotenv)
    database/               # DB layer: factory.go, migrations.go, repository.go, errors.go
    database/azure/         # Azure Table Storage alternative backend
    models/models.go        # Domain models (Item, User) + Repository interface
    health/                 # Liveness/readiness probes
  pkg/dberrors/             # Shared DB error types
```

**Key patterns:**
- **Repository interface** (`models.Repository`) abstracts data access. Implementations: `GenericRepository` (GORM/MySQL) and `azure.TableRepository`. Selected at startup via `database.NewRepository()` factory based on config.
- **Handler struct** (`handlers.Handler`) receives a `Repository` via constructor injection (`NewHandler(repo)`). Use this pattern for new resource handlers.
- **Routes**: Registered in `internal/api/routes/routes.go` using Gin route groups. Health routes at `/health/*`, API resources under `/api/v1/*`.
- **Error handling**: Custom `DatabaseError` type in `internal/database/errors.go` with sentinel errors (`ErrNotFound`, `ErrDuplicateKey`, `ErrValidation`). Use `errors.As`/`errors.Is` for checking. See `handleDBError()` in `handlers/items.go`.
- **Migrations**: Version-based in `internal/database/migrations.go` using `schema.Migrator`. Run automatically on startup.
- **Swagger**: Comments on handler functions generate docs via `swag init -g api/main.go`. Run `make docs` to regenerate.
- **Config**: All settings via env vars (see `docker-compose.yml` for the full list). Loaded by `config.LoadConfig()` with godotenv `.env` fallback.

## Frontend Structure

```
frontend/src/
  api/client.ts     # Axios instance with interceptors
  api/config.ts     # API_BASE_URL (localhost:8081 dev, /api prod)
  routes.tsx        # React Router route definitions
  components/Layout # Shared layout wrapper
  pages/            # Page components (Home, Health)
```

Uses MUI for components, react-router-dom for routing, axios for HTTP. Add new pages in `pages/`, register in `routes.tsx`.

## Development Commands

| Task | Command |
|---|---|
| Full stack (Docker) | `make dev` |
| Backend only (Docker) | `make dev-backend` |
| Frontend only (Docker) | `make dev-frontend` |
| Backend tests (unit, no DB) | `make test-backend` or `cd backend && go test ./... -v -short` |
| Backend tests (with MySQL) | `make test-backend-all` (starts MySQL container) |
| Azure integration tests | `make test-backend-azure-integration` |
| Coverage (80% threshold) | `cd backend && make test-coverage` |
| Swagger docs | `make docs` or `cd backend && make docs` |
| Install deps | `make install` |
| Lint | `make lint` (runs `go vet` + `npm run lint`) |
| MySQL shell | `make mysql-shell` |

## Testing Conventions

- Backend tests use `testify/assert` and table-driven tests (`tests := []struct{...}`).
- Handler tests use `MockRepository` (in-memory, defined in `handlers/mock_repository.go`) — no DB needed for unit tests.
- Test setup: `gin.SetMode(gin.TestMode)`, `httptest.NewRecorder()`, route setup via `setupTestRouter()`.
- JSON schema validation in tests via `gojsonschema`.
- Tests are parallelized (`t.Parallel()`).
- Integration tests are tagged by name pattern: `TestDatabase*` (MySQL), `TestAzureTable*`/`TestAzure*Integration` (Azure).

## Adding a New API Resource

1. Define model in `internal/models/models.go` (embed `Base` for ID/timestamps/soft-delete).
2. Add migration in `internal/database/migrations.go`.
3. Create handler file in `internal/api/handlers/` — use `Handler` struct with `Repository` dependency.
4. Register routes in `internal/api/routes/routes.go` under the `/api/v1` group.
5. Add Swagger comment annotations on handler functions, then run `make docs`.
6. Write tests with `MockRepository` and table-driven test pattern from `items_test.go`.

# Copilot Instructions

## Architecture Overview

Full-stack app: **Go (Gin) backend** + **React (TypeScript, Vite, MUI) frontend**, with **MySQL** (GORM) or **Azure Table Storage** as swappable data stores. Docker Compose orchestrates all services.

**Bootstrap flow**: `backend/api/main.go` → `config.LoadConfig()` → `database.NewRepository(cfg)` (factory selects MySQL or Azure based on `USE_AZURE_TABLE`) → `routes.SetupRoutes(router, repo, healthChecker, cfg)` → `http.Server` with graceful shutdown (`SIGINT`/`SIGTERM`).

**Ports**: Backend `:8081` on host, frontend `:3000` in dev. Inside Docker, nginx and Vite proxy `/api` to `backend:8080`. Local non-Docker dev hits `localhost:8081` directly (`frontend/src/api/config.ts`).

## Backend Structure

```
backend/
  api/main.go                    # Bootstrap: config → repo → router → server → shutdown
  internal/
    api/routes/routes.go         # All route registration + middleware ordering
    api/handlers/items.go        # CRUD handler pattern (reference implementation)
    api/handlers/health.go       # Health handlers (closure-injection, not Handler struct)
    api/handlers/rate_limiter.go # Per-IP sliding window, returned from SetupRoutes for shutdown
    api/handlers/mock_repository.go  # In-memory mock for tests (same package)
    api/middleware/middleware.go  # CORS, Logger, Recovery, RequestID, MaxBodySize
    config/config.go             # Env vars with godotenv .env fallback, typed config structs
    database/factory.go          # MySQL connection with retry (5x, 2s delay)
    database/repository.go       # NewRepository() factory: MySQL vs Azure Table
    database/migrations.go       # Versioned migrations via schema.Migrator, auto-run on startup
    database/errors.go           # Re-exports from pkg/dberrors (single source of truth)
    models/models.go             # Domain models + Repository interface + Filter/Pagination
    models/validation.go         # Validator interface implementations
    health/health.go             # Dependency health checks (liveness/readiness)
  pkg/dberrors/errors.go         # Canonical error types: ErrNotFound, ErrDuplicateKey, ErrValidation
```

## Key Backend Patterns

**Repository interface** (`models.Repository`): All data access uses `Create`, `FindByID`, `Update`, `Delete`, `List` — all take `context.Context` first. Two implementations: `GenericRepository` (GORM/MySQL) and `azure.TableRepository`. The repository auto-calls `Validate()` on create/update if the model implements `Validator`.

**Handler struct**: `handlers.Handler` holds `models.Repository` via constructor injection (`NewHandler(repo)`). CRUD handlers are receiver methods. Health handlers use a different pattern — factory functions returning `gin.HandlerFunc` with `*health.HealthChecker` via closure. If a new resource needs dependencies beyond Repository, create a separate handler struct.

**Error flow**: Repository returns `*dberrors.DatabaseError` wrapping sentinel errors → `handleDBError()` in `handlers/items.go` maps via `errors.As`/`errors.Is` to HTTP status (400 validation, 404 not found, 409 duplicate/version conflict, 500 internal). **Never expose raw error messages for 500s** — always return `"Internal server error"`.

**Optimistic locking**: Models embed `Version uint` field. Repository `Update()` uses `WHERE version = ?` — returns `"version mismatch"` error (mapped to 409). Handlers read-then-update: if client sends `Version > 0`, it overrides; if 0 (omitted), uses the just-read version.

**Filter whitelist**: `GenericRepository` has `allowedFilterFields` map. `NewRepository()` hardcodes Item fields ("name", "price"). New entities need `NewRepositoryWithFilterFields()` or the existing repo must be extended.

**Routes registration**: `SetupRoutes()` returns `*RateLimiter` (caller must call `Stop()` on shutdown). Middleware order: RequestID → Logger → Recovery → CORS → MaxBodySize (1MB). Health at `/health/*` (no rate limit), API at `/api/v1/*` (100 req/min per IP).

## Frontend Structure

```
frontend/src/
  api/config.ts          # API_BASE_URL: localhost:8081 (dev) | /api (prod)
  api/client.ts          # Axios instance + service objects (e.g., healthService)
  routes.tsx             # Route definitions: / → Home, /health → Health
  components/Layout/     # AppBar + nav + footer shell
  pages/{Name}/index.tsx # Page components (one dir per page)
```

**Patterns**: MUI components (no raw HTML), `sx` prop for styling, functional components only, `useState`/`useEffect` for state, service objects with async methods for API calls. New pages: create `pages/{Name}/index.tsx`, register in `routes.tsx`, add nav in `Layout/index.tsx`.

## Development Commands

| Task | Command |
|---|---|
| Full stack (Docker) | `make dev` |
| Backend tests (unit, no DB) | `cd backend && go test ./... -v -short` |
| All backend tests (unit + integration) | `make test-backend-all` (starts MySQL + Azurite) |
| Frontend tests | `cd frontend && npm test` |
| E2E tests | `make test-e2e` (starts infra + backend + Playwright) |
| Swagger docs | `cd backend && make docs` (runs `swag init -g api/main.go`) |
| Coverage (80% threshold) | `cd backend && make test-coverage` |
| Lint | `make lint` (`go vet` + `npm run lint`) |
| Install deps | `make install` |

## Testing Conventions

**Backend**: `testify/assert`, table-driven (`tests := []struct{...}` + `t.Run`), `t.Parallel()` on parent and subtests, capture loop var `tt := tt`. Use `MockRepository` (in-memory, same package as handlers) — type-asserts to `*models.Item`, must be extended for new entity types. Test setup: `setupTestRouter()` returns `(*gin.Engine, *MockRepository)`. JSON responses validated against schemas in `test_schemas.go` via `gojsonschema`.

**Frontend**: Vitest + Testing Library (unit), Playwright (e2e).

**Integration test naming**: `TestDatabase*` (MySQL), `TestAzureTable*`/`TestAzure*Integration` (Azure).

## Adding a New API Resource

1. Model in `internal/models/models.go` (embed `Base` for ID/timestamps/soft-delete)
2. Validation in `internal/models/validation.go` (implement `Validator` interface)
3. Migration in `internal/database/migrations.go` (incrementing version string)
4. Handler file in `internal/api/handlers/` (follow `items.go` CRUD pattern with `handleDBError()`)
5. Routes in `internal/api/routes/routes.go` under `/api/v1` group
6. Swagger annotations on handlers, then `cd backend && make docs`
7. Tests with `MockRepository` + table-driven pattern (extend mock if new entity type)
8. Frontend service methods in `api/client.ts`, new page in `pages/`, register in `routes.tsx`

See `.github/instructions/api-extension.instructions.md` for detailed examples.

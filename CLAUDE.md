# Application Template

## Architecture Overview

A full-stack application template. Items is the example entity: copy its shape for your own resources.

- Backend: Go (Gin). Frontend: React (TypeScript, Vite, MUI). Data store: MySQL (GORM). Docker Compose runs all services.
- Versions: Go 1.27.4 (`backend/go.mod`; CI reads the version from `backend/go.mod`, the images build with `golang:1.27.4`), Node 22.12 or later (`frontend/package.json`; the images use `node:26-alpine`, `frontend/.nvmrc` has the major for CI), MySQL 8.0.17 or later.
- Authentication is optional and off by default (`AUTH_ENABLED=false`): with it off the API is open. See `docs/claude/auth-sessions.md`.

**Bootstrap flow.** `backend/api/main.go` calls helpers in `backend/api/bootstrap.go` in this order:

```text
config.LoadConfig()             # .env file, environment, validation (invalid settings stop the start)
initDatabase                    # MySQL connection, migrations (advisory lock), repository
buildServices                   # health checker, WebSocket hub, in-flight tracker,
                                #   auth stores (auth on), router (routes.Setup), admin user (auth on),
                                #   leader worker group
startHTTPServer                 # binds the address, serves in a goroutine
signal -> shutdown              # ordered, see below
```

**Graceful shutdown** (`SIGINT` or `SIGTERM`), in this order; every step runs even when an earlier one failed:

1. HTTP server: no new requests, wait for the open ones (`SERVER_SHUTDOWN_TIMEOUT`).
2. Audit recorder (auth on): drain the pending audit writes.
3. Leader worker group: stop the background workers.
4. In-flight work (`inflight.Tracker`): wait for work that outlives its request.
5. WebSocket hub: close the client connections.
6. Telemetry flush (OpenTelemetry; a no-op with `OTEL_ENABLED=false` and `METRICS_ENABLED=false`).
7. Rate limiters: stop their cleanup goroutines.
8. Session store (auth on): stop its cleanup goroutine, after everything that reads it.
9. Database: close the pool last.

The steps after the HTTP step have their own budgets, so the worst case is about five times `SERVER_SHUTDOWN_TIMEOUT`. The orchestrator's stop grace period must be longer: `docker-compose.yml` sets `stop_grace_period: 170s` for the default 30 s. Details: `docs/claude/operations.md`.

**Gin mode.** `GIN_MODE` wins when it is set. Otherwise the mode is debug when `APP_DEBUG` is true and release when it is false. The code default of `APP_DEBUG` is `true` and nothing warns about it: set `APP_DEBUG=false` in production (`make prod*` does).

**Ports and proxies.**

- The backend listens on `SERVER_PORT` (default 8081; there is no `PORT` variable). Host ports: API `BACKEND_PORT` (8081), web UI `FRONTEND_PORT` (3000 in development, 80 with `make prod*`).
- The browser always calls the web UI origin, never the backend port. Both the Vite dev server and the bundled nginx forward `/api/*` and `/ws` to the backend and strip one `/api`: the SPA calls `/api/api/v1/items`, the backend sees `/api/v1/items`. `frontend/src/api/config.ts` sets `API_BASE_URL = '/api'`. Same-origin calls are required with auth on, because the refresh cookie is `SameSite=Strict`.
- The OIDC callback URL that you register at the identity provider is therefore `<origin>/api/api/v1/auth/oidc/callback`.
- The backend trusts the proxy's `X-Forwarded-For` only from `TRUSTED_PROXIES` (Compose sets it to the frontend container).

## Development Commands

| Task | Command |
|---|---|
| Full stack (Docker, development images) | `make dev` |
| Backend only, or frontend only (also starts the backend) | `make dev-backend`, `make dev-frontend` |
| Full stack, production images | `make prod` (web UI on port 80, API on 8080) |
| Backend unit tests (no database) | `cd backend && go test ./... -v -short` |
| Backend unit tests with the race detector (as CI) | `make test-backend-unit` |
| All backend tests (unit and integration, starts MySQL) | `make test-backend-all` |
| Frontend tests | `cd frontend && npm test` |
| Unit tests (backend and frontend) | `make test` |
| End-to-end tests, auth off | `make test-e2e` |
| End-to-end tests, auth on | `make test-e2e-auth` (`E2E_SECURE_COOKIES=true` for the production cookie) |
| Swagger docs | `make docs` (runs `swag init -g api/main.go` in `backend`) |
| Coverage (80% threshold) | `cd backend && make test-coverage` |
| Format | `make fmt` |
| Lint (go vet, ESLint, workflow, env docs and agent checks) | `make lint` |
| Markdown lint | `make lint-docs` |
| Security scan (backend) | `make gosec` |
| Environment variable docs match the code | `make check-env-docs` |
| Claude agents, skill and settings are consistent | `make check-agents` |
| Domain terms check | `scripts/check-domain-terms.sh` |
| Install dependencies | `make install` |

Integration tests (tag `integration`) run one package at a time (`-p 1`): the packages share one database. A CI job that runs them must copy that flag.

## Reference Docs

These files do not load on every request. Read the matching file before you work in its area.

| Area | File |
|---|---|
| File trees of the backend, the frontend, the scripts and the workflows | `docs/claude/structure.md` |
| Login, sessions, tokens, roles, API keys, OIDC, audit log, WebSocket auth | `docs/claude/auth-sessions.md` |
| Every environment variable, shutdown, replicas, proxies, rate limits, e2e switches | `docs/claude/operations.md` |
| How the system fits together | `ARCHITECTURE.md` |
| Adding an API resource and a page, optional modules | `EXTENDING.md` |
| Versions, CI checks, branch protection | `RELEASING.md` |

Directory rules: `backend/CLAUDE.md`, `frontend/CLAUDE.md`. GitHub Copilot reads `.github/copilot-instructions.md` and `.github/instructions/*.md`, which point back here.

## Key Backend Patterns

**Repository interface.**

- The generic `models.Repository` interface (`Create`, `FindByID`, `Update`, `Delete`, `ListPaged`, `Ping`, `Close`, and a deprecated `List`) is the Item repository. All methods take `context.Context` first. `GenericRepository` (GORM, MySQL) implements it and calls `Validate()` on create and update when the model implements `Validator`.
- The auth entities have their own repository interfaces in their model files (`UserRepository`, `RefreshTokenRepository`, `APIKeyRepository`, `AuditLogRepository`) and GORM implementations in `internal/database/`. Their signatures differ from `models.Repository`. Follow that shape for a new entity (see `EXTENDING.md`).
- `database.BaseRepository` gives a repository the lock retry: write through `Tx` or `Write`, so a deadlock or lock wait timeout runs the statement or transaction again.
- A list endpoint uses `ListPaged(..., limit, offset, filter)`: a typed filter, a count query with the same conditions, column projection and an indexed order. A new list endpoint must follow this pattern.
- Computed response fields are model fields with `gorm:"-"`.

**Handler struct.** `handlers.Handler` holds `models.Repository` and an optional `websocket.BroadcastSender`: `NewHandler(repo)` or `NewHandlerWithHub(repo, hub)`. Other handlers (auth, users, API keys, audit log) use their own structs with the repositories they need. Health handlers are factory functions that return a `gin.HandlerFunc`.

**Error flow.**

- A repository returns `*dberrors.DatabaseError` wrapping a sentinel (`ErrNotFound`, `ErrDuplicateKey`, `ErrValidation`, `ErrNotImplemented`).
- `mapError(err, entityName)` in `handlers/errors.go` maps it to a status: 400 validation, 404 not found, 409 duplicate, 501 not implemented, 500 otherwise. `handleDBError` in `handlers/items.go` calls it with `"Item"`.
- A cancelled request context (`context.Canceled`) gives 499 and a debug log, not 500. `context.DeadlineExceeded` stays 500.
- A request body that cannot be bound: `respondBindError(c, err)` gives 413 when the body is over the 1 MB limit and 400 otherwise. Every JSON handler uses it instead of a hand-written 400.
- Never expose a raw error message for a 500. Always return `"Internal server error"`.
- Context rule: a repository read on the request path uses `c.Request.Context()`. A write whose side effects follow the commit uses a detached context with a deadline (`detachedCtx(c)`, `ctxutil.Detach`), so a client that leaves cannot skip it and a stalled database cannot hold the request. The caller must call the returned cancel function. Never use a bare `context.WithoutCancel`.

**Optimistic locking.** Only `Item` has `Version uint` (`default:1`). `GenericRepository.Update` uses `WHERE version = ?` and returns a `"version mismatch"` error on a conflict, which the Items handler maps to 409. The server owns the version: a create forces it to 1.

**Filter whitelist.** `GenericRepository` has an `allowedFilterFields` map for the deprecated `List`. A new entity needs `NewRepositoryWithFilterFields()` or its own repository.

**Routes registration.** `routes.Setup(router, deps)` registers every route. It returns `*Result` (rate limiters, auth handler, audit recorder), which the caller must stop on shutdown.

- Global middleware order: RequestID, RedactWSToken, Logger, Recovery, SecurityHeaders, CORS, MaxBodySize (1 MB).
- `/api/v1` adds the API rate limiter (`RATE_LIMIT`, default 100 per minute per client IP). With auth on, login, register and the OIDC routes get their own `LOGIN_RATE_LIMIT` budgets (default 10).
- With auth on, the API group adds `CombinedAuth` (JWT and API key) and the audit recorder. Role gates are `RequireAdmin()` and `RequireRole`.
- WebSocket: `/ws`. Health: `/health/live`, `/health/ready`, `/health`. Swagger UI: `/swagger/*` only with `ENABLE_SWAGGER=true`; it stays open with auth on.
- Client IP: only peers in `TRUSTED_PROXIES` may set it with `X-Forwarded-For` or `X-Real-IP`.

**WebSocket messages.** Client to server: `subscribe` and `unsubscribe` with `{"topic": "..."}`. Server to client: `item.created`, `item.updated`, `item.deleted` (Items handler, to every socket). With auth on, topics are authorised per user. Details: `docs/claude/auth-sessions.md`.

## Frontend

**Stack:** React 19, MUI v9, react-router-dom v7, Vite 8, TypeScript 6, Vitest 5, Playwright (`frontend/e2e/`). Details and patterns: `frontend/CLAUDE.md`.

## Testing Conventions

**Backend.** Use `testify/assert`. Write table-driven tests with `t.Run`; call `t.Parallel()` in the parent test and in the subtests and capture the loop variable (`tt := tt`). Use `MockRepository` (in-memory, Item type only, `handlers/mock_repository.go`). `setupTestRouter()` returns `(*gin.Engine, *MockRepository)`. Validate JSON responses against the schemas in `test_schemas.go` with `gojsonschema`. Integration tests are named `TestDatabase*` or carry the `integration` build tag.

**Frontend.** Vitest and Testing Library (unit), Playwright (e2e, `frontend/e2e/*.spec.ts`).

## Adding a New API Resource

The full walkthrough with Items as the example is in `EXTENDING.md`. In short: model and validation, repository (interface in `models`, GORM implementation in `database`), migration, handler, route, audit entity type, Swagger annotations and `make docs`, tests, then the frontend service, page, route and navigation.

## Audit Trail

With auth on, `middleware.AuditRecorder` writes an entry for every successful `POST`, `PUT`, `PATCH` or `DELETE` of an authenticated user, after the response. Not recorded: reads, failed requests, unauthenticated requests and everything under `/auth/`.

- A handler calls `middleware.SkipAudit(c)` for a successful request that changed nothing, `middleware.AuditCommitted(c)` when it committed a change and then answers with an error, and `middleware.SetAuditEntityID(c, id)` in a create handler.
- An operation that is not plain CRUD needs an entry in `auditRoutes` (`middleware/audit.go`). Without one, the action comes from the HTTP method and the entity type from the last route segment.
- Add new actions and entity types to `KnownAuditActions` and `KnownAuditEntityTypes` (and `entityTypeBySegment` for a segment that is not the entity name with `-` replaced by `_`). The routes test fails for a mutating route with an unknown entity type.

## Security Rules

- Never expose a raw error message for a 500. Return `"Internal server error"`.
- Set all secrets through environment variables. Never hard-code credentials. Values that are marked DEV ONLY (the Compose database password, the e2e test credentials) are for local runs and must never reach production.
- GORM parameterizes queries. Never interpolate user input into raw SQL.
- `CORS_ALLOWED_ORIGINS=*` (the code default) is for development only. The bundled web UI is same-origin and needs no entry. Set a list of origins for other browser clients.
- `SECURE_COOKIES` defaults to `true`. Plain HTTP on a host other than `localhost` needs `SECURE_COOKIES=false`; never use that in production.
- Rate limiting: a per-IP sliding window on the API routes, and stricter limits on login. The IP is the TCP peer, or the client behind a proxy of `TRUSTED_PROXIES`. Never trust all proxies: a client could then pick a new IP for each request.
- The `SecurityHeaders` middleware sets baseline headers on every response and `Recovery` catches panics. Never remove either.
- `RedactWSToken` removes `?token=` from `/ws` URLs before logging. Keep it first after RequestID. A proxy in front of the backend must not log the query of `/ws` either.
- Do not leave `AUTH_ENABLED=false` on anything that is reachable by people you do not trust.
- `JWT_SECRET` and `ADMIN_PASSWORD` have no default. Generate them (`openssl rand -base64 48`), never reuse a value from a document.

## Scalability Rules

Details: `docs/claude/operations.md`.

- Paginate every new list endpoint (`limit` and `offset` with `ListPaged`). Select only the columns that the list needs. Give the query an index that covers its filter and its order. Use batch queries instead of N+1 loops.
- Add a periodic job as a `leader.Worker` with a restartable `Run(ctx)`, passed to `leader.NewGroup` in `buildServices` before `workers.Start`. Keep state that must survive a leader change in the database. There is no leader election yet: every process runs the workers, so a job must be safe on several replicas.
- Work that outlives its request starts with `inflight.Tracker.Go`. Shutdown waits for it before the database closes.
- Send WebSocket messages through the hub (`Broadcast`, `BroadcastToTopic`, `BroadcastToUser`) and revoke through `DisconnectUser`, `DisconnectToken` or `DisconnectAPIKey`, never by writing to a connection. Hub state is per replica: a message reaches only the sockets of the replica that sent it.
- Register every external dependency with `healthChecker.AddCheck()`.
- The rate limiters, the username login throttle and the WebSocket hub are per replica and in memory.
- Struct field order: put 8-byte fields first for memory alignment (`govet fieldalignment`).

## Project Documents

`ARCHITECTURE.md` describes the system, `EXTENDING.md` how to extend it, `RELEASING.md` the CI checks and releases. `WIKI.md` and `README.md` are the human getting-started docs; `README.md` has "Start a new project from this template".

# Extending the Template

How to add to the template: a new API resource and its page, a gate by role, a WebSocket event, a background job, a configuration variable and an optional module. Items is the example entity throughout: every step names the Items file to copy. The snippets use a made-up resource `Note` (string UUID ID, `title`, `body`).

Rules that apply to all of it are in `CLAUDE.md`, `backend/CLAUDE.md` and `frontend/CLAUDE.md`. Read `docs/claude/auth-sessions.md` before you touch authentication.

## Adding a new API resource

Items is a reference for the shape of a CRUD resource. Its repository is the generic `models.Repository`, which is typed to `Item` (`ListPaged` takes an `ItemFilter`), so a new resource gets a repository of its own, shaped like the auth repositories (`models.APIKeyRepository` and `database.GORMAPIKeyRepository`). Do the steps in this order; run the tests after each one.

### 1. Model, validation and repository interface

Items: `internal/models/models.go` (`Item`, `Validator`, `Repository`) and `internal/models/validation.go` (`Item.Validate`).

Create `internal/models/note.go`:

```go
// Note is the example resource of EXTENDING.md.
type Note struct {
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
    ID        string    `json:"id" gorm:"primaryKey;size:36"`
    Title     string    `json:"title" gorm:"size:255;not null"`
    Body      string    `json:"body" gorm:"type:text"`
}

// NoteFilter selects notes for ListPaged. A zero field does not filter.
type NoteFilter struct{ Title string }

// NoteRepository stores notes. Every method takes the context first.
type NoteRepository interface {
    Create(ctx context.Context, note *Note) error
    FindByID(ctx context.Context, id string) (*Note, error)
    Update(ctx context.Context, note *Note) error
    Delete(ctx context.Context, id string) error
    // ListPaged returns one page and the number of all matches.
    ListPaged(ctx context.Context, filter NoteFilter, limit, offset int) ([]*Note, int64, error)
}

// Validate implements Validator. The repository calls it before a write,
// as GORMAPIKeyRepository.Create does.
func (n *Note) Validate() error { ... }
```

- The auth models use a string UUID `ID` that the repository sets in `Create` (`uuid.New().String()`); `Item` has a numeric ID through `Base` and a `Version` for optimistic locking. Do not embed `Base` in a new model: it is only for Item.
- Put sentinel validation errors next to the existing ones in `validation.go` or in the model file.
- Order the struct fields for memory alignment (8-byte fields first): the `fieldalignment` linter fails otherwise.
- Deletes are hard deletes. `Base.DeletedAt` is a plain field, not `gorm.DeletedAt`.

### 2. Repository

Items: `GenericRepository` in `internal/models/models.go`. The pattern for a new resource is `internal/database/api_key_repository.go`.

Create `internal/database/note_repository.go`:

```go
var _ models.NoteRepository = (*GORMNoteRepository)(nil)

type GORMNoteRepository struct{ *BaseRepository }

func NewGORMNoteRepository(db *gorm.DB) *GORMNoteRepository {
    return &GORMNoteRepository{BaseRepository: NewBaseRepository(db)}
}
```

- Write through `r.Write(ctx, ...)` (one statement) or `r.Tx(ctx, ...)` (several), so a deadlock or lock wait timeout is retried. Map every error with `repoError(ctx, "create", err)`; it gives `dberrors.ErrNotFound`, `ErrDuplicateKey` and a context error that the handler maps to 499. Validation failures go through `validationError("create", err)`.
- `ListPaged`: one filter struct, a count query and a page query with the same `where`, column projection, a stable indexed `ORDER BY`. Never return all rows.
- Never interpolate a request value into SQL: use `?` placeholders.

### 3. Migration

Items: `createBaseTablesMigration` in `internal/database/migrations.go`.

Add a function that returns a `schema.Migration` with the next version string (`YYYYMMDDNNNNNN`, after the last one in `registerMigrations`), a `Name`, a `Description`, an `Up` and a `Down`, and register it with `migrator.AddMigration(...)`. Define the table in a private struct of its own (like `sessionEntryV1`) and call `tx.AutoMigrate` on it, so the migration keeps its behavior when the model changes later; the `Down` drops the table. **Never edit a migration that exists**: add a new one. Run `make test-backend-all` (the integration tests apply every migration to a real MySQL).

### 4. Handler

Items: `internal/api/handlers/items.go`. Create `handlers/notes.go` with a `NoteHandler` struct and `NewNoteHandler(repo models.NoteRepository)`, one method per route. Follow Items in these rules:

- Bind with `c.ShouldBindJSON(&v)` and answer a failure with `respondBindError(c, err)`: 413 for an oversized body, 400 otherwise. Never write your own 400 there.
- Validate in the handler for the fast 400 (`Name is required` in Items), and in the model for the rest.
- Read with `c.Request.Context()`. A write whose effects follow the commit uses `ctx, cancel := detachedCtx(c); defer cancel()`.
- Map every repository error with `mapError(err, "Note")`: 404, 409, 400, 499 for a cancelled request and `Internal server error` for a 500. Never return `err.Error()` for a 500.
- Parse path IDs and answer 400 `Invalid ID format` (Items parses a `uint`; a UUID resource checks the format).
- In a create handler call `middleware.SetAuditEntityID(c, note.ID)`; in a handler that changes nothing call `middleware.SkipAudit(c)`.
- A list handler reads `limit` and `offset`, rejects a bad value with 400 and calls `ListPaged`. Cap the page size with `models.MaxPageSize`.
- To publish an event, take a `websocket.BroadcastSender` as Items does (`NewHandlerWithHub`) and call `Broadcast` with a `{"type": "note.created", "payload": ...}` message.

### 5. Routes and wiring

Items: `routes.Setup` in `internal/api/routes/routes.go` (`items := api.Group("/items")`) and `api/bootstrap.go`.

- Register the routes on the `api` group, which is open with auth off and carries `CombinedAuth` and the audit recorder with auth on: `notes := api.Group("/notes")` and `GET ""`, `GET "/:id"`, `POST ""`, `PUT "/:id"`, `DELETE "/:id"`. For a resource that only admins may change, add a gate: `api.Group("/notes", middleware.RequireAdmin())`. The role check in the UI is a hint only.
- Give the repository to `Setup` through `routes.Deps` (a new field `Notes models.NoteRepository`) and build it in `api/bootstrap.go` on the shared database handle: `database.NewGORMNoteRepository(db)`, where `db` comes from the `gormRepository` interface as in `openAuthStores`. Add the field to `routerDeps` and pass it in `buildRouter`. Register the routes only when the field is not nil, so tests that do not need it stay small.
- Do not add a route outside `/api/v1`, or one that skips the rate limiter, without a reason.

### 6. Audit

Items: the constants and `KnownAuditEntityTypes` in `internal/api/middleware/audit.go`.

With auth on, every successful `POST`, `PUT`, `PATCH` or `DELETE` is recorded. The entity type comes from the last route segment. Add `AuditEntityNote = "note"` to the constants and to `KnownAuditEntityTypes`, and map the segment in `entityTypeBySegment` (`"notes": AuditEntityNote`). The routes test fails for a mutating route with an unknown entity type, so a missing entry cannot ship. An operation that is not plain CRUD (for example `PUT /notes/:id/archive`) needs an entry in `auditRoutes` and a known action in `KnownAuditActions`.

### 7. Swagger

Every handler has the annotations that `items.go` shows: `@Summary`, `@Description`, `@Tags`, `@Accept`, `@Produce`, `@Param`, `@Success`, `@Failure` (400, 404 and 409 where they apply, 413 for a bound body, 499, 500), `@Security BearerAuth`, `@Security ApiKeyAuth`, `@Failure 401` and `@Router /api/v1/notes [post]`. Run `make docs` and commit the generated files in `backend/docs/`.

### 8. Backend tests

Items: `handlers/items_test.go`, `handlers/mock_repository.go`, `handlers/test_schemas.go`, `database/repository_items_test.go`, `database/repository_mysql_integration_test.go`.

- Handler tests: a mock of `NoteRepository` in the handlers package, `setupTestRouter`-style setup, table-driven subtests with `t.Parallel()`, JSON responses checked against a schema in `test_schemas.go` (`gojsonschema`). Cover 400, 404, 409, 413, 499 and a 500 that answers `Internal server error` without the cause.
- Repository tests: unit tests on SQLite, and `TestDatabase*` integration tests (build tag `integration`) on MySQL for ordering, filters and duplicates.
- Routes: `routes_test.go` (auth off) and `routes_auth_test.go` (auth on) build the router with `routes.Setup`; cover the new route in both, so it is checked open and behind authentication.
- Run `go test ./... -short`, then `make test-backend-all`, then `make lint` and `make gosec`.

### 9. Frontend

Items: `src/api/client.ts` (`itemService`), `src/pages/Items/index.tsx`, `src/routes.tsx`, `src/components/Layout/index.tsx`, `src/pages/Items/__tests__/Items.test.tsx`, `e2e/items.spec.ts`.

1. In `src/api/client.ts` add an interface (`Note`) and a `noteService` with `list`, `get`, `create`, `update` and `delete`. Each method has a TSDoc comment with `@param`, `@returns` and `@see` (HTTP method and route), and logs with `summarizeRequestError`. Calls use the paths `/api/v1/notes`: the base URL is the same-origin `/api`.
2. Create `src/pages/Notes/index.tsx`: loading state, then error state, then content or `EmptyState`; MUI components and `sx`; `ConfirmDialog` before a delete; `getApiErrorMessage` for errors. Subscribe to `note.*` messages through `useWebSocketContext().subscribe` if the backend broadcasts them.
3. Register the page in `src/routes.tsx` inside `ProtectedRoute` (add `roles={['admin']}` for an admin page) and add a navigation `Button` in `src/components/Layout/index.tsx`.
4. Add `src/pages/Notes/__tests__/Notes.test.tsx` (Vitest and Testing Library, at least a render test and a delete flow), a Playwright spec in `e2e/` and the page to `PAGES` in `e2e/accessibility.spec.ts`.
5. Run `cd frontend && npm run lint && npm test`, then `make test-e2e` and `make test-e2e-auth`.

### 10. Documentation

Add the route to the Swagger output (step 7), the entity to the table in `ARCHITECTURE.md` and the files to `docs/claude/structure.md`. A new environment variable follows the rules below.

## Gating by role

Roles are `user` < `devops` < `admin`. `middleware.RequireRole(roles...)`, `RequireAdmin()`, `RequireDevOps()` and `RequireUser()` are Gin handlers for a route or group. A gate for an unknown role, or with no role, closes. The role in the token is a claim: a handler that changes state for another user also reads the user row (see `handlers/users.go`). With `AUTH_ENABLED=false` no gate runs: the whole API is open. Routes that must not be reachable with an API key (key management, user administration, logout-all) add `middleware.RequireSessionAuth()`.

## Adding a WebSocket event

Send from a handler through the `BroadcastSender` (`Broadcast`, to every socket) or the hub (`BroadcastToTopic`, `BroadcastToUser`). Messages are JSON `{"type": "...", "payload": ...}`. For a topic-scoped stream add the authorisation rule to `DefaultTopicAuthorizer` in `handlers/websocket_auth.go` (deny by default: an unlisted topic is refused) and a test in `websocket_topics_test.go`. In the SPA, subscribe with `useWebSocketContext().subscribe(type, handler)`. The hub is per replica: a message reaches only the sockets of the replica that sent it.

## Adding a background job

A periodic job is a `leader.Worker` with a restartable `Run(ctx)` (use `periodicWorker(name, interval, fn)` in `api/bootstrap.go`). Add it to the `leaderWorkers` list in `buildServices`, before the group starts. With `LEADER_ELECTION_ENABLED=false` (the default) every process runs it; with `true` only the lease holder does, but a paused or cut-off leader can overlap its successor briefly, so the job must be idempotent and safe on several replicas either way, and must return within 5 s after its context is cancelled. Keep state that must survive in the database. Work that outlives its request starts with `inflight.Tracker.Go` and must respect cancellation. A new external dependency gets `healthChecker.AddCheck`.

## Adding a configuration variable

1. Read it in `internal/config/config.go` (or a file next to it) with `getEnv`, `getEnvBool`, `getEnvDuration`, `getEnvInt32` or a `parseEnv*Strict` variant. Use a strict parse for a security setting (a typo must stop the start, not fall back silently). Validate it in `Validate`.
2. Add it with a comment to `backend/.env.example` and `backend/config/.env.example` (the two differ in the `DB_HOST` line only). Mark values for local development as DEV ONLY.
3. Add a row to the right table in `docs/claude/operations.md`.
4. Pass it through `docker-compose.yml` when a container needs it.
5. Run `make check-env-docs`. It fails when the code, the two examples and `operations.md` disagree, and when an example documents a variable that no code reads.

## Adding an optional module

Authentication is the model: a module has one switch that is off by default, validates its settings strictly only when it is on, and leaves no route behind when it is off (404).

1. Add the switch and its settings to `config` (parse the switch strictly; read the other settings leniently while the module is off, strictly while it is on).
2. Build the module's stores and workers in `api/bootstrap.go` only when the switch is on, and stop them in `shutdown` in the right place (after everything that reads them). Update the shutdown order in `docs/claude/operations.md`, `CLAUDE.md` and `ARCHITECTURE.md`.
3. Register its routes in `routes.Setup` only when the switch is on. A missing dependency with the switch on is an error at startup (see `ErrAuthDepsMissing`), never a silent skip.
4. Expose the flag to the web UI through `GET /api/v1/ui-config` (`handlers/ui_config.go`, `UIConfigContext.tsx`) so the SPA shows the module only when it exists.
5. Test both modes: the route table with the module off and on, the config validation, and an e2e pass (`scripts/e2e.sh` runs the auth module as a mode).
6. Document the switch in `README.md`, `docs/claude/operations.md` and `ARCHITECTURE.md` ("Optional Modules").

## Authentication modes

The behaviour of the three modes (off, local login, OIDC) and what a new route inherits in each is in `docs/claude/auth-sessions.md` ("Modes"). A new route on the `api` group needs no auth code: with auth on it already sits behind `CombinedAuth` and the audit recorder, and with auth off it is open.

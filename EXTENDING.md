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

- The auth models use a string UUID `ID` that the repository sets in `Create` (`uuid.New().String()`); `Item` has a numeric ID through `Base` and a `Version` for optimistic locking. Do not embed `Base` in a new model: it is only for Item (the one exception is an entity that opts in to soft delete, see "Adding soft delete to an entity").
- Put sentinel validation errors next to the existing ones in `validation.go` or in the model file.
- Order the struct fields for memory alignment (8-byte fields first): the `fieldalignment` linter fails otherwise.
- Deletes are hard deletes. `Base.DeletedAt` is a plain field, not `gorm.DeletedAt`. A repository can opt in to soft delete: see "Adding soft delete to an entity" below.

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
- `ListPaged`: one filter struct, a count query and a page query with the same `where`, column projection, a stable indexed `ORDER BY`. Never return all rows. For a client-chosen order add `SortBy string` and `SortDesc bool` to the filter, a `NoteSortKeys` list in the model and a private map from key to column in the repository (the only place where a key becomes a column; an unknown key is `validationError`/`dberrors.ErrValidation`). Order with `clause.OrderByColumn{Column: clause.Column{Name: col}, Desc: f.SortDesc}`, then `id` in the same direction. Never put the key into a SQL string.
- Never interpolate a request value into SQL: use `?` placeholders.

### 3. Migration

Items: `createBaseTablesMigration` in `internal/database/migrations.go`.

Add a function that returns a `schema.Migration` with the next version string (`YYYYMMDDNNNNNN`, after the last one in `registerMigrations`), a `Name`, a `Description`, an `Up` and a `Down`, and register it with `migrator.AddMigration(...)`. Define the table in a private struct of its own (like `sessionEntryV1`) and call `tx.AutoMigrate` on it, so the migration keeps its behavior when the model changes later; the `Down` drops the table. **Never edit a migration that exists**: add a new one. A sortable column needs an index so that `ORDER BY col, id` (and `DESC`) does not filesort: InnoDB appends the primary key to a secondary index, so one index on `(col)` serves both directions. Migration `20261115000001` (`add_sort_indexes`, `addSortIndexesMigration`) is the example; it creates an index only when it is missing and its `Down` drops only its own. Run `make test-backend-all` (the integration tests apply every migration to a real MySQL).

### 4. Handler

Items: `internal/api/handlers/items.go`. Create `handlers/notes.go` with a `NoteHandler` struct and `NewNoteHandler(repo models.NoteRepository)`, one method per route. Follow Items in these rules:

- Bind with `c.ShouldBindJSON(&v)` and answer a failure with `respondBindError(c, err)`: 413 for an oversized body, 400 otherwise. Never write your own 400 there.
- Validate in the handler for the fast 400 (`Name is required` in Items), and in the model for the rest.
- Read with `c.Request.Context()`. A write whose effects follow the commit uses `ctx, cancel := detachedCtx(c); defer cancel()`.
- Map every repository error with `mapError(err, "Note")`: 404, 409, 400, 499 for a cancelled request and `Internal server error` for a 500. Never return `err.Error()` for a 500.
- Parse path IDs and answer 400 `Invalid ID format` (Items parses a `uint`; a UUID resource checks the format).
- In a create handler call `middleware.SetAuditEntityID(c, note.ID)`; in a handler that changes nothing call `middleware.SkipAudit(c)`.
- A list handler reads `limit` and `offset`, rejects a bad value with 400 and calls `ListPaged`. `GET /api/v1/items` sends the total of `ListPaged` in the `X-Total-Count` header (the body stays a plain array; `@Header 200 {integer} X-Total-Count`; CORS exposes it). Cap the page size with `models.MaxPageSize`. For sorting call `key, desc, ok := parseSort(c, models.NoteSortKeys)` (`handlers/sort.go`): it answers 400 for a `sort` outside the whitelist or an `order` other than `asc` or `desc`. Add `@Param sort query string false "..." Enums(...)` and `@Param order query string false "..." Enums(asc, desc)`.
- To publish an event, take a `websocket.BroadcastSender` as Items does (`NewHandlerWithHub`) and call `Broadcast` with a `{"type": "note.created", "payload": ...}` message.

### 5. Routes and wiring

Items: `routes.Setup` in `internal/api/routes/routes.go` (`items := api.Group("/items")`) and `api/bootstrap.go`.

- Register the routes on the `api` group, which is open with auth off and carries `CombinedAuth` and the audit recorder with auth on: `notes := api.Group("/notes")` and `GET ""`, `GET "/:id"`, `POST ""`, `PUT "/:id"`, `DELETE "/:id"`. For a resource that only admins may change, add a gate: `api.Group("/notes", middleware.RequireAdmin())`. The role check in the UI is a hint only.
- Give the repository to `Setup` through `routes.Deps` (a new field `Notes models.NoteRepository`) and build it in `api/bootstrap.go` on the shared database handle: `database.NewGORMNoteRepository(db)`, where `db` comes from the `gormRepository` interface as in `openAuthStores`. Add the field to `routerDeps` and pass it in `buildRouter`. Register the routes only when the field is not nil, so tests that do not need it stay small.
- Do not add a route outside `/api/v1`, or one that skips the rate limiter, without a reason.

### 6. Audit

Items: the constants and `KnownAuditEntityTypes` in `internal/api/middleware/audit.go`.

With auth on, every successful `POST`, `PUT`, `PATCH` or `DELETE` is recorded. The entity type comes from the last route segment. Add `AuditEntityNote = "note"` to the constants and to `KnownAuditEntityTypes`, and map the segment in `entityTypeBySegment` (`"notes": AuditEntityNote`). The routes test fails for a mutating route with an unknown entity type, so a missing entry cannot ship. An operation that is not plain CRUD (for example `PUT /notes/:id/archive`) needs an entry in `auditRoutes` and a known action in `KnownAuditActions`. Routes under `/auth/` are not audited unless they have such an entry (`PUT /auth/me/password` does).

### 7. Swagger

Every handler has the annotations that `items.go` shows: `@Summary`, `@Description`, `@Tags`, `@Accept`, `@Produce`, `@Param`, `@Success`, `@Failure` (400, 404 and 409 where they apply, 413 for a bound body, 499, 500), `@Security BearerAuth`, `@Security ApiKeyAuth`, `@Failure 401` and `@Router /api/v1/notes [post]`. Run `make docs` and commit the generated files in `backend/docs/`.

### 8. Backend tests

Items: `handlers/items_test.go`, `handlers/mock_repository.go`, `handlers/test_schemas.go`, `database/repository_items_test.go`, `database/repository_mysql_integration_test.go`.

- Handler tests: a mock of `NoteRepository` in the handlers package, `setupTestRouter`-style setup, table-driven subtests with `t.Parallel()`, JSON responses checked against a schema in `test_schemas.go` (`gojsonschema`). Cover 400, 404, 409, 413, 499 and a 500 that answers `Internal server error` without the cause.
- Repository tests: unit tests on SQLite, and `TestDatabase*` integration tests (build tag `integration`) on MySQL for ordering, filters and duplicates.
- Routes: `routes_test.go` (auth off) and `routes_auth_test.go` (auth on) build the router with `routes.Setup`; cover the new route in both, so it is checked open and behind authentication.
- Run `go test ./... -short`, then `make test-backend-all`, then `make lint` and `make gosec`.

### 9. Frontend

Items: `src/api/client.ts` (`itemService`), `src/pages/Items/index.tsx`, `src/routes.tsx`, `src/components/Layout/navItems.ts`, `src/pages/Items/__tests__/Items.test.tsx`, `e2e/items.spec.ts`.

1. In `src/api/client.ts` add an interface (`Note`) and a `noteService` with `list`, `get`, `create`, `update` and `delete`. Each method has a TSDoc comment with `@param`, `@returns` and `@see` (HTTP method and route), and logs with `summarizeRequestError`. Calls use the paths `/api/v1/notes`: the base URL is the same-origin `/api`.
2. Create `src/pages/Notes/index.tsx`: loading state, then error state, then content or `EmptyState`; MUI components and `sx`; `ConfirmDialog` before a delete; `getApiErrorMessage` for errors; `useNotification().notify` for the result of an action (no local `Snackbar`). Subscribe to `note.*` messages through `useWebSocketContext().subscribe` if the backend broadcasts them.
3. Register the page in `src/routes.tsx` inside `ProtectedRoute` (add `roles={['admin']}` for an admin page) and add an entry to `NAV_ITEMS` in `src/components/Layout/navItems.ts` (the app bar, the drawer and the Home page read it; `adminOnly: true` for an admin page).
4. Add `src/pages/Notes/__tests__/Notes.test.tsx` (Vitest and Testing Library, at least a render test and a delete flow), a Playwright spec in `e2e/` and the page to `PAGES` in `e2e/accessibility.spec.ts`.
5. Run `cd frontend && npm run lint && npm test`, then `make test-e2e` and `make test-e2e-auth`.

### 10. Documentation

Add the route to the Swagger output (step 7), the entity to the table in `ARCHITECTURE.md` and the files to `docs/claude/structure.md`. A new environment variable follows the rules below.

## Gating by role

Roles are `user` < `devops` < `admin`. `middleware.RequireRole(roles...)`, `RequireAdmin()`, `RequireDevOps()` and `RequireUser()` are Gin handlers for a route or group. A gate for an unknown role, or with no role, closes. The role in the token is a claim: a handler that changes state for another user also reads the user row (see `handlers/users.go`). With `AUTH_ENABLED=false` no gate runs: the whole API is open. Routes that must not be reachable with an API key (key management, user administration, logout-all, the password change) add `middleware.RequireSessionAuth()`.

## Adding a WebSocket event

Send from a handler through the `BroadcastSender` (`Broadcast`, to every socket) or the hub (`BroadcastToTopic`, `BroadcastToUser`). Messages are JSON `{"type": "...", "payload": ...}`. For a topic-scoped stream add the authorisation rule to `DefaultTopicAuthorizer` in `handlers/websocket_auth.go` (deny by default: an unlisted topic is refused) and a test in `websocket_topics_test.go`. In the SPA, subscribe with `useWebSocketContext().subscribe(type, handler)`. The hub is per replica: a message reaches only the sockets of the replica that sent it.

With the outbox module (`OUTBOX_ENABLED=true`) an event that must survive a crash after the commit, or reach the sockets of every replica, goes through the outbox instead: write the event in the transaction of the change through the change hook of the repository (`models.WithChangeHook`; `outbox.ItemHook` is the Items one, which builds the same `{"type", "payload"}` message as the handler) and never broadcast it from the handler when the module is on; the poller of every replica delivers it from `outbox_events`. The hook may use only the transaction it gets and must have no side effect outside the database (the repository runs the whole transaction again after a deadlock). Delivery is at most once per socket and has no replay (`docs/claude/operations.md`, "Outbox"). With the module off the handler broadcasts after the commit, as above.

## Adding a background job

There are two kinds. A periodic job runs on a schedule and is a `leader.Worker`; a job on demand is started by a request or an event and runs from the queue.

### A periodic job

A periodic job is a `leader.Worker` with a restartable `Run(ctx)` (use `periodicWorker(name, interval, fn)` in `api/bootstrap.go`). Add it to the `leaderWorkers` list in `buildServices`, before the group starts. With `LEADER_ELECTION_ENABLED=false` (the default) every process runs it; with `true` only the lease holder does, but a paused or cut-off leader can overlap its successor briefly, so the job must be idempotent and safe on several replicas either way, and must return within 5 s after its context is cancelled. Keep state that must survive in the database. Work that outlives its request starts with `inflight.Tracker.Go` and must respect cancellation. A new external dependency gets `healthChecker.AddCheck`.

### A job on demand (the queue)

Work that a request starts and that must survive a restart, be retried and not hold the request (a report, an export, a call to a slow service) is a job of the optional queue (`JOBS_ENABLED=true`; `docs/claude/operations.md`, "Background jobs"). The template registers no job type: the three steps below are yours.

1. **Register a handler** in `registerJobs` (`backend/api/jobs.go`), the one place that fills the registry before the pool starts. A type name is 1 to 100 characters of `a-z0-9._-`; a duplicate or invalid name panics at start.

   ```go
   func registerJobs(reg *jobs.Registry) {
       reg.Register("report.generate", func(ctx context.Context, job jobs.Job) error {
           var p reportPayload
           if err := json.Unmarshal(job.Payload, &p); err != nil {
               return jobs.Permanent(err) // a payload that does not parse never gets better
           }
           return generateReport(ctx, p.ReportID) // honour ctx: shutdown cancels it
       })
   }
   ```

2. **Enqueue from a handler.** The handler struct holds the `*jobs.Queue`; `routes.Setup` receives it as `deps.JobQueue` (nil with the module off, and a nil queue answers `jobs.ErrDisabled`), so pass it to your handler's constructor there.

   ```go
   type ReportHandler struct {
       queue *jobs.Queue
   }

   func (h *ReportHandler) Create(c *gin.Context) {
       // ... bind and validate the request ...
       id, err := h.queue.Enqueue(c.Request.Context(), "report.generate",
           reportPayload{ReportID: reportID}, jobs.WithMaxAttempts(3))
       if errors.Is(err, jobs.ErrDisabled) {
           c.JSON(http.StatusNotImplemented, gin.H{"error": "Background jobs are not enabled"})
           return
       }
       if err != nil {
           slog.ErrorContext(c.Request.Context(), "Failed to enqueue the report job", "error", err)
           c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
           return
       }
       c.JSON(http.StatusAccepted, gin.H{"job_id": id})
   }
   ```

   `Enqueue` needs no registered handler in this process (a rolling deploy can enqueue a type that only the new replicas handle). Options: `WithDelay`, `WithRunAt` (uses the clock of this process; prefer `WithDelay`) and `WithMaxAttempts`. The payload is marshalled to JSON and at most 64 KiB: put an ID in it, not the data, and never a secret, because the row lives for `JOBS_RETENTION` or `JOBS_DEAD_RETENTION`. A handler that enqueues is a mutating route: register its route and audit entity type as for any resource.

3. **Make the handler safe to run twice.** Delivery is at least once. A worker that crashes after the side effect and before the result is written, a lease that passes while the process hangs, or a retry after a timeout runs the handler again, possibly at the same time on another replica. There is no deduplication or unique key (two `Enqueue` calls are two jobs), so key the side effect on something in the payload (a report ID, an idempotent upsert, a conditional update) and treat "already done" as success.

**Outcome.** A handler that returns nil is `succeeded`. An error is retried after `min(JOBS_BACKOFF_BASE * 2^(attempt-1), JOBS_BACKOFF_MAX)` (shortened by up to 20% of jitter) until `attempts` reaches `max_attempts` (`JOBS_MAX_ATTEMPTS`, or `WithMaxAttempts`), then the job is `dead` (the dead letter: it stays for `JOBS_DEAD_RETENTION` and nothing runs it again). `jobs.Permanent(err)` goes to `dead` at once. A panic counts as a failure. `last_error` holds the error text (cut to 2000 characters), so never put a secret or personal data in an error. A crashed replica's job runs again after `JOBS_VISIBILITY_TIMEOUT`.

**See it.** `GET /api/v1/jobs` (admin with auth on) lists the jobs with `status`, `type`, `sort` and `order` filters, their attempts, schedule and `last_error`, never the payload. There is no API to retry, cancel or delete a job; a dead job is read, fixed in code and enqueued again. The health check `jobs` shows DEGRADED when due jobs wait longer than `JOBS_STALE_AFTER` (also for a type that no replica handles).

**Tests.** Register the handler on a `jobs.NewRegistry()` in a test and call it with a `jobs.Job`; test the enqueue handler with a `*jobs.Queue` on a fake `models.JobRepository`. Integration tests of the queue itself need MySQL (`make test-backend-all`).

## Adding soft delete to an entity

Deletes are hard by default. A repository opts in to soft delete when a deleted row must be recoverable or kept for a while. The template ships no soft delete entity (Items stay hard); `internal/models/soft_delete_test.go` and `internal/database/soft_delete_mysql_integration_test.go` show the pattern with test-only entities.

**What fits.** The option belongs to `models.GenericRepository`, so it fits the entities that repository serves: Items-shaped entities that embed `Base` (numeric `uint` ID, indexed `deleted_at`). That is the exception to "do not embed `Base` in a new model" in step 1: embed it here, and only for such an entity. Two limits follow:

- `GenericRepository.ListPaged` is Item-specific (it queries `Model(&Item{})` and its `dest` must be `*[]Item`). The option covers its queries for Items. Another `GenericRepository` entity has `Create`, `FindByID`, `Update`, `Delete` and the deprecated `List` with the option; its own list queries add `deleted_at IS NULL` themselves.
- An entity on `BaseRepository` with its own interface and string IDs (steps 1 and 2) has no option: write its soft delete in its own repository, set `deleted_at` in `Delete` with the database clock, and repeat `deleted_at IS NULL` in every query of that repository (qualify it with the table name in a join: `notes.deleted_at IS NULL`). `Restore(ctx, id uint)` of `SoftDeleter` has a numeric ID, so it does not fit such an entity either.

**Soft delete is not erasure.** The row stays in the table, in the backups and (with the outbox) in the payload of its events until a purge removes it, so set the retention of step 4 on purpose.

The steps, with `Note` as a `GenericRepository` entity that embeds `Base`:

1. **The option.** Build the repository with `models.NewRepositoryWithFilterFields(db, fields, models.WithSoftDeleteOf(&Note{}))`. For Items add `models.WithSoftDelete()` to the options that `repositoryOptions(cfg)` returns in `internal/database/repository.go` (today it returns nil without the outbox; return a slice that holds the option, with or without the change hook). The option is per repository: another repository on the same table is unaffected. Without it nothing changes (same SQL, hard deletes). With it:
   - `Create` sets `deleted_at` to nil first: the field binds from JSON, and a row must never be created already deleted (hidden, outside the unique guard of step 5, back-dated past the retention).
   - `Delete` sets `deleted_at` to the database time (`UTC_TIMESTAMP(3)`, UTC, milliseconds; the replica clock does not matter) instead of removing the row, through the same lock retry, and the change hook still runs in that transaction (the outbox gets its delete event). A deleted or missing row is `ErrNotFound`. The entity you pass is not changed.
   - Every read skips the rows with a `deleted_at`: `FindByID` and `Update` give `ErrNotFound` for a deleted row (also for an entity with a `Version`: a stale version on a live row stays a version mismatch), and `List` (and, for Items, `ListPaged` and its count query) do not return it. The condition is added in one place (`GenericRepository.live`), so no query of the repository can forget it. A query of your own on the table (a join, a raw count) must add it.
   - `Update` never writes `deleted_at`: only `Delete` and `Restore` do.
2. **The JSON.** `deleted_at` is `omitempty`: a live row never shows it. A delete handler answers as before and a deleted row is a `404` on a read. No endpoint returns a deleted row unless you add one (an admin list of the deleted rows needs its own query).
3. **Restore.** `GenericRepository` always implements `models.SoftDeleter` (`Restore(ctx, id)` and `PurgeDeleted(ctx, before, limit)`), so a type assertion always succeeds: check `SoftDeleteEnabled()` to know whether the option is on (both methods return an `ErrNotImplemented` error without it). Add `POST /api/v1/notes/:id/restore` under `RequireAdmin()` (or `RequireRole`), answer through `mapError(err, "Note")` (`ErrNotFound` for a row that is missing or not deleted, `ErrDuplicateKey` as 409 when a unique index now conflicts, see step 5) and add an `auditRoutes` entry (`"POST /notes/:id/restore": {Action: AuditActionRestore, EntityType: AuditEntityNote, IDParam: "id"}`), with `AuditActionRestore = "restore"` in `KnownAuditActions` (section "6. Audit"). Restore runs no change hook: send the WebSocket event from the handler, or extend the hook of your repository.
4. **Purge.** Soft deleted rows stay until you remove them. Register a worker in `buildServices`, before the leader group starts (section "Adding a background job"):

   ```go
   if repo.SoftDeleteEnabled() { // repo is the *models.GenericRepository
       leaderWorkers = append(leaderWorkers, purgeWorker("note-purge", time.Hour, 30*24*time.Hour, repo))
   }
   ```

   `purgeWorker` (`api/soft_delete.go`) calls `PurgeDeleted` in batches of 1000 (at most 100 per run) until a batch is short, stops at once when its context ends, and is safe on several replicas (a second call finds the rows gone). The cutoff is `now - retention` by the clock of the process: use a retention of hours or days. Nothing registers it for you, because nothing in the template deletes softly.
5. **Unique names.** A unique index on `name` would keep a deleted name taken forever. A unique index ignores NULLs, so index a value that is the name for a live row and NULL for a deleted one: a generated column. MySQL 8:

   ```sql
   ALTER TABLE notes
     ADD COLUMN active_name VARCHAR(255)
       GENERATED ALWAYS AS (IF(deleted_at IS NULL, name, NULL)) STORED,
     ADD UNIQUE KEY uq_notes_active_name (active_name);
   ```

   A composite `UNIQUE (name, deleted_at)` does not work: `deleted_at` is NULL for every live row, so two live rows with the same name would both pass. With the generated column, creating a name that a deleted row had works, and a restore that would make two live rows with the same name fails with `ErrDuplicateKey` (409). Put the column in a migration (section "3. Migration"), never in the model: MySQL refuses a write to a generated column, so the GORM struct must have no field for it, and `AutoMigrate` must not manage it. The integration test `TestDatabaseMySQLSoftDeleteUniqueName` proves the behavior.
6. **Indexes.** `Base` already indexes `deleted_at`, and an InnoDB secondary index ends with the primary key, so `WHERE deleted_at IS NULL ORDER BY id LIMIT n` can be served in id order without a sort, and the purge (`deleted_at < ?`, oldest first) uses the same index. Which index the optimizer picks for the default order depends on the statistics: usually PRIMARY in id order, skipping the deleted rows, which is cheap while most rows are live. A table that is mostly deleted rows needs a purge more than an index. A list sorted by another column (`sort=name`) needs a composite index with `deleted_at` first: `KEY (deleted_at, name)`; without it MySQL sorts the live rows. Add one for each sort key you expose and check the plan with `EXPLAIN`. `TestDatabaseMySQLSoftDeleteIndexes` checks the order property (no filesort, with `FORCE INDEX` for the single column index) and, on a mostly deleted table, that an ordered query by name has no filesort with the composite index (`FORCE INDEX` proves it can serve the order); it does not prove which index the optimizer chooses for the default order.
7. **Tests.** Cover delete, the hidden reads, update of a deleted row, restore, purge by cutoff, the unique reuse on MySQL, a create with `deleted_at` in the body, and that a repository built without the option still deletes hard.

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

The idempotency module (`IDEMPOTENCY_ENABLED`, see `docs/claude/operations.md`, "Idempotency keys") is the smallest example: `config/idempotency.go` (strict switch, lenient durations while off), `routes.Setup` mounts `middleware.Idempotency` only when the switch is on and returns `ErrIdempotencyDepsMissing` without its repository, `buildServices` builds the repository and the `idempotency-cleanup` worker only when it is on. It has no UI, so it skips step 4. A module that stores a response must keep secrets out of it: the module's skip list (`idempotencySkipPrefixes`) names the routes it never touches.

The mail and password reset modules (`MAIL_ENABLED`, `PASSWORD_RESET_ENABLED`) are the example of a module with a UI and public routes: `config/mail.go`, `internal/mail` (an interface, an SMTP implementation, a fake), `routes.Setup` registers the two routes only when the switch is on and returns `ErrPasswordResetDepsMissing` without their dependencies, `api/password_reset.go` builds the mailer, its readiness check and the `password-reset-cleanup` worker, and the handler (`handlers/password_reset.go`) does its work after the answer in `inflight.Tracker.Go`. A public route that must be audited mounts `a.recorder.Middleware()` itself and names its actor with `middleware.SetAuditActor`.

## Authentication modes

The behaviour of the three modes (off, local login, OIDC) and what a new route inherits in each is in `docs/claude/auth-sessions.md` ("Modes"). A new route on the `api` group needs no auth code: with auth on it already sits behind `CombinedAuth` and the audit recorder, and with auth off it is open.

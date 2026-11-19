# Backend: Go Instructions

## Project Layout

The module is `backend` (see `go.mod`, Go 1.27). Use `internal/` for private packages and `pkg/` for shared utilities (`pkg/dberrors/`, `pkg/utils/`). Shared test utilities are in `internal/test/test_helpers.go`. The full file tree is in `docs/claude/structure.md` at the repository root.

Packages in `internal/`:

- `api/routes`, `api/handlers`, `api/middleware`: routes (`routes.Setup`), HTTP handlers and Gin middleware.
- `auth`: OIDC provider client (discovery, PKCE, ID token validation). `auth/authtest` is a fake identity provider for tests.
- `config`: `LoadConfig`, `AuthConfig`, `OIDCConfig`, `ParseTrustedProxies`, `MySQLSessionParams`.
- `ctxutil`: `Detach`, a bounded context that a client who leaves cannot cancel.
- `database`: connection factory, migrations (`schema/` is the migrator), the GORM repositories of the auth entities, `BaseRepository` (lock retry), the GORM logger.
- `health`: liveness and readiness checks.
- `inflight`: `Tracker` for work that outlives its request.
- `leader`: `Worker` and `Group` for leader-only background jobs; `Lease`, the MySQL row lease that elects the leader.
- `models`: Item, User, RefreshToken, APIKey, AuditLog, ReplicaHeartbeat, IdempotencyKey, OutboxEvent, validation, repository interfaces, `GenericRepository` (with an optional change hook), `RetryOnLock`.
- `logutil`: `Limiter`, one log line per interval for an error that repeats on every request.
- `outbox`: the optional outbox module: `Poller` (reads `outbox_events` after a cursor and sends each message to the hub, with a gap window for ids that commit out of order), `ItemHook` (the change hook of the Items repository) and `Metrics`.
- `replica`: `ProcessID` (host name plus a random suffix) and `Heartbeat`, the per-process row in `replica_heartbeats`.
- `telemetry`: OpenTelemetry (`Init`, `Shutdown`, `LogHandler`, `StartDBMetrics`). Off by default and then a true no-op.
- `sessionstore`: token and session blocklist and OIDC login state. `SESSION_STORE=mysql` is the default; `memory` is for tests and one replica.
- `websocket`: hub, clients, topics, revalidation, message types.

The entry point is `api/main.go`. The helpers (database, services, router, HTTP server, ordered shutdown) are in `api/bootstrap.go`; the metrics and pprof servers are in `api/servers.go`.

## Handler Pattern

- The Items handler (`handlers/items.go`) uses the generic `Handler` struct with repository injection and is the reference for a CRUD handler.
- Other handlers (`AuthHandler`, `UserHandler`, `APIKeyHandler`, `AuditLogHandler`, `OIDCHandler`) use dedicated structs, each with the repositories it needs.
- Use `NewHandlerWithHub` when a handler must broadcast WebSocket events.
- Register routes in `internal/api/routes/routes.go` under the `api` group of `/api/v1` (which carries the auth middleware when auth is on).
- Every JSON handler binds its body with `ShouldBindJSON` and answers a failure with `respondBindError(c, err)` (`handlers/errors.go`): 413 `request body too large` when the body is over the 1 MB limit (`*http.MaxBytesError`, which a chunked body without `Content-Length` reaches only here, because `MaxBodySize` rejects a declared length up front) and 400 `Invalid request format` otherwise. A hand-written `c.JSON(400, ...)` after a failed bind turns an oversized body into a 400.

## Repository Interface

- `models.Repository` is the Item repository: `Create`, `FindByID`, `Update`, `Delete`, `ListPaged` (and the deprecated `List`), `Ping`, `Close`. Every method takes `context.Context` first. `GenericRepository` (GORM, MySQL) implements it; `database.NewRepository` builds it from the config and runs the migrations.
- The auth entities have their own interfaces in `internal/models/` and GORM implementations in `internal/database/` (`GORMUserRepository` and its siblings). Every method takes `context.Context` first. `BaseRepository` has no method without a context, so a repository built on it cannot write without one.
- Writes go through `BaseRepository.Tx` (several statements) or `Write` (one statement) so that a deadlock (1213) or lock wait timeout (1205) runs the whole attempt again. The function you pass must reset in-memory state it changed and have no side effect outside the database. A repository built on a transaction handle runs once and leaves the retry to the outer `Tx`.
- Pass `c.Request.Context()` to a repository read on the request path. A write whose side effects follow the commit (token block, refresh-token revoke, WebSocket disconnect, audit entry) uses a detached context with a deadline: `detachedCtx(c)` (`handlers/detach.go`) or `ctxutil.Detach(ctx, timeout)` (`ctxutil.WriteTimeout`, 30 s). Each returns a cancel function that the caller must call (`defer cancel()`). A timeout gives 500 (`DeadlineExceeded`). A bare `context.WithoutCancel` anywhere in the backend fails `ctxutil.TestNoUnboundedDetach`; background jobs detach with `ctxutil.Detach` too.
- New or changed list endpoints use `ListPaged` with column projection and a filter struct (`ItemFilter`); the count query uses the same conditions. Never add a new unbounded `List()` call from a handler.
- Client-controlled sorting (`sort`, `order`): the handler checks the key against a whitelist with `parseSort` (`handlers/sort.go`, 400 `Invalid sort parameter` or `Invalid order parameter`, lower case only, empty means absent) and puts it in the filter (`ItemFilter.SortBy`, `SortDesc`; `UserFilter` alike). The repository maps the key to a column through its own map (`itemSortColumns`, `userSortColumns`), orders with `clause.OrderByColumn` and refuses an unknown key with `ErrValidation`; request input is never put into SQL. The order ends with `id` in the same direction, the count query does not sort, and without `sort` the default order stays (items `id`, users `username`). A new sortable column needs an index (migration `20261115000001` has the item and user ones).
- `GenericRepository` takes an optional change hook at construction (`models.NewRepository(db, models.WithChangeHook(hook))`; nil means none and the interface does not change). With a hook, `Create`, `Update` and `Delete` run the change and the hook in one transaction inside the lock retry: the hook uses only the `tx` it gets, keeps no state and has no side effect outside the database, and an error rolls the change back. `database.NewRepository` installs `outbox.ItemHook` only with `OUTBOX_ENABLED=true`.
- Soft delete is an opt-in per repository (`models.NewRepository(db, models.WithSoftDelete())`, `models.WithSoftDeleteOf(&Entity{})` for another entity; `internal/models/soft_delete.go`). It fits `GenericRepository` entities that embed `Base` (uint id); `ListPaged` is Item-specific; an entity on `BaseRepository` repeats `deleted_at IS NULL` in its own queries. `Create` clears `deleted_at`; `Delete` sets it to `UTC_TIMESTAMP(3)` in the same lock retry and hook transaction; `FindByID`, `ListPaged` with its count, `List` and `Update` add `deleted_at IS NULL` through `GenericRepository.live` (never add a read that skips it); `Restore` and `PurgeDeleted` are `models.SoftDeleter` (always implemented, `ErrNotImplemented` without the option: check `SoftDeleteEnabled()`, not a type assertion). Soft delete is not erasure (data stays until a purge). `purgeWorker` (`api/soft_delete.go`) is a leader worker that is not registered in `buildServices`. Without the option nothing changes. Steps, unique-name reuse and indexes: `EXTENDING.md`.
- Use batch methods (`FindByIDs`) instead of N+1 loops for enrichment.
- A duplicate-key error is detected with `dberrors.IsDuplicateKey`, never by message text in new code.

## Error Handling

- Sentinel errors: `ErrNotFound`, `ErrDuplicateKey`, `ErrValidation`, `ErrConnectionFailed`, `ErrNotImplemented` (`pkg/dberrors`, re-exported by `internal/database`).
- Wrap errors with context: `dberrors.NewDatabaseError("operation", err)`.
- Check errors with `errors.Is` and `errors.As(err, &dbErr)`.
- `mapError(err, entityName)` in `handlers/errors.go` returns `(statusCode, message)`; `handleDBError` in `handlers/items.go` calls it with `"Item"`.
- A cancelled request context (`context.Canceled`) gives 499 (client closed request) with a debug log, not 500, through `mapError` or `respondCancelled`. `context.DeadlineExceeded` stays 500. The one exception is an upstream failure with its own timeout, such as the OIDC token exchange, which answers 502.
- A 500 answers `"Internal server error"` and nothing else, in handlers and in the `Recovery` middleware.

## Testing

- Use `testify/assert`. Never use `testing` alone for assertions.
- Write table-driven tests with `t.Parallel()` in the parent test and in the subtests. Capture range variables: `tt := tt` before `t.Run`.
- Use `MockRepository` from `handlers/mock_repository.go` (Item type only). It type-asserts to `*models.Item`: extend it, or write a mock of your own repository interface, for a new entity.
- Test setup: `setupTestRouter()` returns `(*gin.Engine, *MockRepository)`. `TestMain` of the handlers package sets Gin's mode once.
- Validate JSON responses with `gojsonschema` against the schemas in `test_schemas.go`.
- Run unit tests: `go test ./... -short` (`-v` only for one package), or `make test-backend-unit` at the root (with `-race`).
- Integration tests need MySQL and the build tag `integration`: `make test-backend-all` at the root starts MySQL and runs them with `-p 1`.
- Coverage (80% for each package of `coverage-packages.txt`, not a total): `make test-coverage` in this directory or at the root. A package you add goes into that list in the same change.

## Swagger/OpenAPI

Every handler needs these annotations: `@Summary`, `@Description`, `@Tags`, `@Accept`, `@Produce`, `@Param`, `@Success`, `@Failure`, `@Router`. A handler that binds a JSON body also lists `@Failure 413`; a handler on an authenticated route lists `@Security BearerAuth`, `@Security ApiKeyAuth` and `@Failure 401`. Regenerate the docs with `make docs` (never edit `docs/` by hand). The Swagger UI is served at `/swagger/index.html` only with `ENABLE_SWAGGER=true`.

## Struct Field Alignment

Order the fields for memory alignment. Put 8-byte fields before smaller fields (`govet fieldalignment` runs in the linter).

## Configuration

All configuration uses environment variables with a `.env` fallback (godotenv). See `internal/config/config.go` and `oidc.go`. Every variable has a default, a comment in `.env.example` and a row in `docs/claude/operations.md`. `scripts/check-env-docs.sh` (`make check-env-docs`) fails when the code, `backend/.env.example`, `backend/config/.env.example` and `operations.md` disagree: add a variable to all four together. The two example files differ in the `DB_HOST` line only.

Key groups:

- `AUTH_ENABLED` (default false), `JWT_SECRET`, `ADMIN_*`, `SELF_REGISTRATION`, `SECURE_COOKIES` (default true), the session and token durations, `SESSION_STORE`, `OIDC_*`. With auth on, an invalid value stops the start; with auth off the auth settings are read leniently. `AUTH_ENABLED` itself is always strict.
- `DB_*`. MySQL session variables (`sql_mode`, `time_zone` UTC, `foreign_key_checks`) are DSN parameters (`config.MySQLSessionParams`), which the driver sets on every pooled connection. Never set them with a `SET` statement after connecting: that reaches one connection only. MySQL 8.0.17 or later.
- `SERVER_*`, `RATE_LIMIT` (default 100 per minute), `LOGIN_RATE_LIMIT` (default 10 per minute; login, register, OIDC and the password change each have a budget), `WS_MAX_CONNECTIONS_PER_USER` (default 16 open `/ws` sockets per user and per API key, enforced in `Hub.Register`), `TRUSTED_PROXIES` (validated by `config.ParseTrustedProxies` at load, applied by `routes.Setup`), `ENABLE_SWAGGER`.
- `LOG_LEVEL`: `debug`, `info` (default), `warn` or `error`. `main` applies it to the default `slog` logger, which writes text to stderr. GORM SQL errors and slow queries (200 ms or more) go through `slog`; with `APP_DEBUG=false` the SQL values are replaced with `?`.
- `GIN_MODE` wins when set. Otherwise the mode is debug when `APP_DEBUG` is true and release when it is false. The code default of `APP_DEBUG` is `true`: set `false` in production.
- `OTEL_ENABLED` (default false), `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_TRACE_SAMPLE_RATE`, `METRICS_ENABLED` (default false), `METRICS_ADDR` (default `:9090`, read with `METRICS_ENABLED=true`). The two switches are strict booleans; the other three are read and validated only with `OTEL_ENABLED=true`.
- `PPROF_ENABLED` (default false, strict), `PPROF_ADDR` (default `127.0.0.1:6060`), `PPROF_ALLOW_NON_LOOPBACK`, `PPROF_MUTEX_FRACTION`, `PPROF_BLOCK_RATE`. The metrics and pprof servers are in `api/servers.go`, on ports of their own (never the API router), and `shutdown` stops them first. pprof refuses a non-loopback address unless `PPROF_ALLOW_NON_LOOPBACK=true`: a heap dump exposes secrets. `/metrics` has no authentication: keep it on a private network.
- `CORS_ALLOWED_ORIGINS` (default `*`, development only). It also gates the WebSocket `Origin` header.
- `LEADER_ELECTION_ENABLED` and `REPLICA_HEARTBEAT_ENABLED` (both default false, strict booleans). The lease timing is fixed in code (`leader.DefaultLeaseConfig`).
- `IDEMPOTENCY_ENABLED` (default false, strict boolean), `IDEMPOTENCY_TTL` (default 24h) and `IDEMPOTENCY_LOCK_TIMEOUT` (default 60s): the durations are read leniently while the module is off and must be positive with it on (`config/idempotency.go`).
- `COMPRESSION_ENABLED` (default true, strict boolean) and `COMPRESSION_MIN_SIZE` (default 1024): the size is read leniently while the module is off and must be a positive integer of at most 1048576 with it on (`config/compression.go`).
- `OUTBOX_ENABLED` (default false, strict), `OUTBOX_POLL_INTERVAL` (default 500ms), `OUTBOX_RETENTION` (default 1h). The durations are read leniently while the module is off; with it on they must be positive and the retention at least 1m and 10 times the poll interval (`config.OutboxConfig.Validate`).

## Migrations

`internal/database/migrations.go` registers versioned migrations with the `schema.Migrator`; they run at every start under the MySQL advisory lock `schema_migrations.<database>` (`GET_LOCK`, waits up to 5 minutes). Never edit a migration that has been released: add a new one with the next version string (`YYYYMMDDNNNNNN`). A migration that touches a table that a later model changes uses a private struct of the old shape (see `legacyUser`, `sessionEntryV1`), so it keeps its behavior when the model changes. Give every migration a `Down`.

## Background Workers

- Leader-only workers are built in `buildServices` (`api/bootstrap.go`) and run in a `leader.Group`. With auth on they are `refresh-token-cleanup` (hourly) and `session-store-cleanup` (every 15 minutes); with `REPLICA_HEARTBEAT_ENABLED=true` also `replica-heartbeat-cleanup` (every 15 minutes); with `IDEMPOTENCY_ENABLED=true` also `idempotency-cleanup` (hourly, batches of 1000); with `OUTBOX_ENABLED=true` also `outbox-cleanup` (every minute, deletes the rows older than `OUTBOX_RETENTION`). With `LEADER_ELECTION_ENABLED=false` (the default) every process runs them. With `true` the group runs only on the replica that holds the MySQL lease (`leader.Lease`, table `leader_leases`, database clock; `electedLeader` in the bootstrap): it starts on acquisition and stops when renewals fail for the renew deadline (10 s, 5 s before the 15 s lease expires) or another holder appears. A job must still be idempotent and safe on several replicas (no fencing token), and return within 5 s after its context is cancelled. Details: `docs/claude/operations.md` ("Leader election").
- With `OUTBOX_ENABLED=true` every process runs one `outbox.Poller` (not a leader worker: each replica delivers to its own sockets). It is started at the end of `buildServices` and stopped in its own shutdown step (fixed 2 s, `outboxStopTimeout`) after the HTTP server and the in-flight work and before the hub and the database close. `buildServices` refuses the module when the repository has no change hook.
- With `REPLICA_HEARTBEAT_ENABLED=true` `replica.Heartbeat` upserts this process's row in `replica_heartbeats` every 30 s and deletes it in the shutdown step before the database closes.
- A worker has `Run(ctx)`. It blocks until `ctx` is done and must work again after it returned: no closed stop channels, no one-shot state. Use `periodicWorker` for an interval job.
- Add a new periodic job as a `leader.Worker` passed to `leader.NewGroup` before the group starts (`Group.Add` takes effect only at the next term), not as a goroutine in `main.go`. Keep state that must survive a leader change in the database.
- Work that outlives its request or worker run starts with `inflight.Tracker.Go(fn)`. It returns `false` once shutdown has begun; the caller then does the work inline or drops it. Never add work after `Wait`. A panic in tracked work is recovered and logged.

## Middleware Order

`routes.Setup()` applies this order: RequestID, RedactWSToken, Logger, Recovery, Compression, SecurityHeaders, CORS, MaxBodySize (1 MB).

- `middleware.Compression` (`COMPRESSION_ENABLED`, default true; `COMPRESSION_MIN_SIZE`, default 1024, at most 1 MiB) is mounted only when enabled, right after `Recovery`. It gzips a response for a client that sends `Accept-Encoding: gzip` when the type is on the allowlist, the body reaches the minimum and nothing forbids it (HEAD, 1xx/204/304/206, an existing `Content-Encoding`, `no-transform`); it adds `Vary: Accept-Encoding` to every compressible response. It skips `/ws` (by path and by `Upgrade`) and never decompresses a request body. Middleware inside it (idempotency, audit) sees the uncompressed bytes; a new module that writes a response goes after it, never before. Rules: `docs/claude/operations.md` ("Response compression").
- The `/api/v1` group adds the API rate limiter.
- With auth on, the authenticated group adds `CombinedAuth` (JWT and API key) and the audit recorder.
- With `IDEMPOTENCY_ENABLED=true` `middleware.Idempotency` follows them (with auth off it sits right after the rate limiter): a `POST` with an `Idempotency-Key` runs once and a repeat replays the stored response. MySQL arbitrates (`models.IdempotencyRepository`, one atomic statement per decision); the store of the response and the release use `ctxutil.Detach`. `/auth/*` and `/api-keys` are on its skip list (`idempotencySkipPrefixes`): never add a route whose response holds a secret. A new `/api/v1` POST needs the `Idempotency-Key` header in its Swagger annotations (`@Param Idempotency-Key header string false`). With the module off nothing is mounted and CORS is unchanged.
- `Setup` returns `*Result`: call `Result.RateLimiters.Stop()` on shutdown and `Result.AuditRecorder.Shutdown(ctx)` before the database closes (the bootstrap does).

## Telemetry

Read `docs/claude/operations.md` ("Telemetry") for the signals and variables. The rules:

- `main` calls `initTelemetry` before the database and the router: the router, the hub and the revalidation checker read the global OTel providers when they are built. `attachTelemetry` makes `Telemetry.Shutdown` the telemetry step of `shutdown`.
- With both switches off nothing may be registered or allocated per request: `routes.Setup` adds `HTTPMetrics` and `otelgin` only when a switch is on, and the auth outcome counters return at once on the no-op provider (`authInstruments.off`). A new per-request metric must keep that property (test it with `testing.AllocsPerRun`).
- Register `otelgin` and `HTTPMetrics` after `RedactWSToken`: the `/ws` query token must never reach a span attribute or a label.
- Never use user input as a metric label (a path, a username, an error text). Use route patterns and fixed outcome constants.
- An error that repeats on every request goes through `logutil.Limiter` (one Error line per interval, the rest at Debug) and is counted in a metric, so the limit hides nothing. Never log an Error per request or per frame for a failing dependency.
- The `slog` bridge (`setupOTelLogging`) builds its own text handler. Never pass `slog.Default().Handler()` to `LogHandler`: it deadlocks with the `log` package or chains a second bridge.

## Sessions, Tokens, OIDC

Read `docs/claude/auth-sessions.md` before you change them. The short rules:

- Login issues a JWT with `jti` and `sid` and an opaque refresh token (hashed at rest) in an httpOnly cookie. Refresh rotates it inside a session family.
- Logout and every admin action that ends sessions revoke the refresh tokens first, then block (`sessionstore`), then close the sockets. The REST path fails closed on a store error.
- All auth paths (login, refresh, OIDC, API key) reject a disabled user.
- A custom `sessionstore.SessionStore` must honour `ctx` and make a block visible to every replica.
- `/auth/*` is not audited, except routes with an entry in `auditRoutes` (the password change, `PUT /auth/me/password`). The OIDC provider lives in `internal/auth`; the state is stored in `sessionstore`.

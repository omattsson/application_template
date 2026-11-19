# Architecture

How the template fits together. Items is the example entity. For commands and rules see `CLAUDE.md`; for how to add a resource see `EXTENDING.md`.

## System Overview

```text
 Browser (React SPA)
    |  same origin: the web UI host
    v
 Web UI proxy (nginx in production, Vite dev server in development)
    |  /api/*  -> strips one /api  -> backend /api/v1/...
    |  /ws     -> WebSocket upgrade
    |  /*      -> static files, SPA fallback
    v
 Backend (Go, Gin)  :8081 ----- MySQL 8.0.17+ (GORM)
```

- Docker Compose runs three services: `db` (MySQL), `backend` and `frontend`. The networks are `backend-net` (db and backend) and `frontend-net` (backend and frontend proxy). The frontend container has a fixed address on `frontend-net`, which the backend lists in `TRUSTED_PROXIES`, so it reads the real client IP from the proxy's `X-Forwarded-For`.
- The SPA is always same-origin. In the browser the API lives under `/api` (so a request is `/api/api/v1/items`: the proxy removes the first `/api`).
- The backend is stateless except for MySQL: several replicas can run behind one load balancer. What is per replica (rate limiters, the username login throttle, the WebSocket hub) is listed in `docs/claude/operations.md`, "Replicas".

## Request Flow

1. The proxy forwards the request to `:8081` with `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Real-IP`.
2. Gin middleware, in order: `RequestID`, `RedactWSToken`, `Logger`, `Recovery`, `Compression` (gzip for clients that ask, `COMPRESSION_ENABLED`, default on; never `/ws`), `SecurityHeaders`, `CORS`, `MaxBodySize` (1 MB).
3. `/api/v1` adds the per-IP rate limiter. With auth on, the protected routes add `CombinedAuth` (bearer JWT or `X-API-Key`; revocation checks that fail closed) and the audit recorder. With `IDEMPOTENCY_ENABLED=true` the idempotency middleware follows (after the authentication, so the user is the scope; with auth off, after the rate limiter).
4. A handler (`internal/api/handlers`) binds and validates the input and calls a repository interface from `internal/models`.
5. The repository (`internal/models.GenericRepository` for Items, `internal/database` for the rest) talks to MySQL through GORM. Errors are wrapped as `*dberrors.DatabaseError` with a sentinel (`ErrNotFound`, `ErrDuplicateKey`, `ErrValidation`).
6. `mapError` turns the sentinel into a status and a safe message: 400, 404, 409, 501, 499 for a cancelled request, and 500 with the text `Internal server error` for anything else.
7. After the response the audit recorder writes an entry for a successful mutating request of an authenticated user.

## Package Structure

```text
backend/
  api/                    main.go: config -> database -> services -> server -> signal -> shutdown
                          bootstrap.go: the helpers, including the ordered shutdown
  internal/
    api/routes            routes.Setup: every route, the middleware order, auth wiring
    api/handlers          one file per resource (items.go) plus auth, users, API keys, audit log, WebSocket
    api/middleware        CORS, logging, recovery, auth, roles, audit recorder, /ws token helpers
    config                typed configuration from environment variables, validation
    database              factory, migrations, schema/ (migrator), repositories, BaseRepository (lock retry)
    models                entities, validation, repository interfaces, GenericRepository (Item)
    sessionstore          blocklist of revoked tokens, sessions and users; pending OIDC logins
    auth                  OIDC client: discovery, PKCE, ID token validation
    websocket             hub, clients, topics, revalidation of open sockets
    health                liveness and readiness checks
    leader, inflight      leader-only background workers; work that outlives its request
    ctxutil               detached, bounded contexts for post-commit writes
    telemetry             OpenTelemetry traces, metrics, logs (off by default); pool metrics
    logutil               Limiter: one log line per interval for a repeating error
  pkg/dberrors            sentinel errors and duplicate-key detection
frontend/src
  api/                    Axios client, session (in-memory token), refresh, config
  context/                UI config, auth, WebSocket, theme
  components/, pages/     shared components; one directory per page
```

The complete file tree is in `docs/claude/structure.md`.

## Data Model

| Table | Entity | Notes |
|---|---|---|
| `items` | `models.Item` | The example entity. Numeric ID, `version` for optimistic locking. Hard delete (soft delete is an opt-in of the repository, see "Hard by default"). |
| `users` | `models.User` | String UUID ID. Unique username; `(auth_provider, external_id)` unique for SSO accounts. Role `user`, `devops` or `admin`. `service_account` marks the break-glass admin. |
| `refresh_tokens` | `models.RefreshToken` | SHA-256 of the opaque token, `family_id` (the session), `revoked`, expiry. |
| `api_keys` | `models.APIKey` | SHA-256 of the key, prefix, owner, expiry, last used. |
| `audit_logs` | `models.AuditLog` | Who, action, entity type and ID, details (JSON), timestamp. |
| `idempotency_keys` | `models.IdempotencyKey` | The claim and the stored response of a `POST` with an `Idempotency-Key` (optional module). Primary key `(scope, idempotency_key)`, request hash, lock owner, status, content type, body, expiry (indexed). |
| `session_entries` | session store | One table: `token_block`, `session_block`, `user_block` and `oidc_state` rows with an expiry. |
| `schema_versions` | migrator (`schema.SchemaVersion`) | The applied migration versions. |

The auth tables exist whether or not `AUTH_ENABLED` is true. Migrations run at every start under a MySQL advisory lock, so replicas that start together serialize; a replica waits up to 5 minutes for the lock. MySQL 8.0.17 or later is required (the collation of `session_entries.entry_key`).

## Authentication Modes

| Mode | Switch | Effect |
|---|---|---|
| Off (default) | `AUTH_ENABLED=false` | The API and `/ws` are open. The auth, API key, user and audit log routes answer 404. The UI shows no login. |
| Local login | `AUTH_ENABLED=true` | Username and password, a 15 minute access token plus a rotating refresh session in an httpOnly cookie, API keys, user administration, the audit log. The first admin comes from `ADMIN_USERNAME` and `ADMIN_PASSWORD`. |
| OIDC | `AUTH_ENABLED=true` and `OIDC_ENABLED=true` | Single sign-on with the authorization code flow and PKCE. Roles come from an ID token claim. |

The session design (tokens, rotation, logout, revocation, WebSocket auth, audit) is in `docs/claude/auth-sessions.md`. Two decisions shape the code: a revocation is written to the shared session store first and every replica reads it, and every store error on the request path fails closed.

## Optional Modules

| Module | How to turn it on | Where |
|---|---|---|
| Authentication | `AUTH_ENABLED=true` with `JWT_SECRET` and `ADMIN_PASSWORD` | `routes.Setup` builds the auth wiring only when it is on; `bootstrap.go` builds the stores and the cleanup workers |
| OIDC single sign-on | `OIDC_ENABLED=true` and the `OIDC_*` settings | `internal/auth`, `handlers/oidc.go` |
| Self-registration | `SELF_REGISTRATION=true` | `POST /api/v1/auth/register`, role `user` only |
| Session store | `SESSION_STORE=mysql` (default) or `memory` (one replica) | `internal/sessionstore` |
| Swagger UI | `ENABLE_SWAGGER=true` | `/swagger/index.html`; open even with auth on |
| Trusted proxies | `TRUSTED_PROXIES=<ips or CIDRs>` | client IP for rate limits and audit entries |
| Response compression | `COMPRESSION_ENABLED` (default `true`), `COMPRESSION_MIN_SIZE` (default 1024) | `middleware.Compression` after `Recovery` (gzip of compressible responses for a client that sends `Accept-Encoding: gzip`, `Vary: Accept-Encoding`, never `/ws`); rules in `docs/claude/operations.md` |
| Idempotency keys | `IDEMPOTENCY_ENABLED=true` (`IDEMPOTENCY_TTL`, `IDEMPOTENCY_LOCK_TIMEOUT`) | `middleware.Idempotency` on `/api/v1` (a `POST` with an `Idempotency-Key` header runs once, a repeat replays the stored response), table `idempotency_keys`, worker `idempotency-cleanup`; rules in `docs/claude/operations.md` |
| Transactional outbox | `OUTBOX_ENABLED=true` (`OUTBOX_POLL_INTERVAL`, `OUTBOX_RETENTION`) | `models.GenericRepository` change hook (`outbox.ItemHook`), `internal/outbox` poller on every replica, `outbox-cleanup` leader worker; the Items events go through the table `outbox_events` |
| Mail | `MAIL_ENABLED=true` (`SMTP_HOST`, `SMTP_PORT`, `SMTP_TLS`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `MAIL_FROM`, `SMTP_TIMEOUT`) | `internal/mail` (`Mailer`, `SMTPMailer`, `FakeMailer`), the informational `mail` readiness check (cached, bounded, never gates readiness); adds no route by itself; rules in `docs/claude/operations.md` |
| Password reset | `PASSWORD_RESET_ENABLED=true` (needs auth, mail and the password login; `PASSWORD_RESET_URL`, `PASSWORD_RESET_TOKEN_TTL`, `PASSWORD_RESET_MIN_INTERVAL`) | `POST /api/v1/auth/password-reset` and `/confirm` (`handlers/password_reset.go`), table `password_reset_tokens` (only the SHA-256 of the token), worker `password-reset-cleanup`; flow in `docs/claude/auth-sessions.md` |

The WebSocket hub, the leader worker group and the in-flight tracker are always built. Which workers run depends on the modules: with auth on, `refresh-token-cleanup` (hourly) and `session-store-cleanup` (every 15 minutes); with the idempotency module on, `idempotency-cleanup` (hourly); with the outbox on, `outbox-cleanup` (every minute); with the password reset on, `password-reset-cleanup` (hourly). The outbox poller is not a leader worker: every process runs one.

## WebSocket

`GET /ws` upgrades to a WebSocket owned by `websocket.Hub`. The Items handler sends `item.created`, `item.updated` and `item.deleted` to every client; with `OUTBOX_ENABLED=true` the repository writes them to `outbox_events` in the transaction of the change and the poller of every replica delivers them to its own hub, so a replica that did not handle the request also reaches its sockets and a crash after the commit loses nothing (see `docs/claude/operations.md`, "Outbox"). A client may subscribe to topics (`items`, `item:<id>`, `user:<id>`, `admin:<name>`); with auth on, topics are authorised (deny by default) and the hub closes a socket when its credential is revoked or expires, and revalidates all sockets every minute. Messages are `{"type": "...", "payload": ...}`; the hub is per replica.

## Shutdown

`SIGINT` or `SIGTERM` runs `shutdown` (`api/bootstrap.go`). Every step runs even when an earlier one failed:

1. HTTP server drain, `SERVER_SHUTDOWN_TIMEOUT`.
2. Audit recorder drain (auth on).
3. Leader worker group.
4. In-flight work.
5. Outbox poller (`OUTBOX_ENABLED=true`), within 2 s, before the hub and the database close.
6. WebSocket hub.
7. Telemetry flush (OpenTelemetry; a no-op with telemetry off).
8. Rate limiters.
9. Session store (auth on), after everything that reads it.
10. Replica heartbeat (`REPLICA_HEARTBEAT_ENABLED=true`): delete this process's row, within 2 s.
11. Database pool.

The steps 1, 2, 3, 4 and 7 have their own detached budgets, so the worst case is about five times `SERVER_SHUTDOWN_TIMEOUT` (plus 2 s each for the diagnostics servers, the outbox poller and the heartbeat); the orchestrator's stop grace period must be longer (Compose: `stop_grace_period: 170s`). With `LEADER_ELECTION_ENABLED=true` step 3 also releases the leader lease, unless a worker did not stop in time: then the lease is kept (renewed) until the process exits.

## Design Decisions

- **MySQL only.** The production factory always builds MySQL. SQLite exists for unit tests only (`DB_TYPE=sqlite`).
- **Hard by default, soft delete per repository.** `Base.DeletedAt` is a plain field, not `gorm.DeletedAt`, so `Delete` removes the row and GORM adds no filter. A repository opts in to soft delete with `models.WithSoftDelete()` (`WithSoftDeleteOf(&Note{})` for another entity): `Delete` then sets `deleted_at` to the database time, every read (`FindByID`, `ListPaged` and its count, `List`, `Update`) skips the marked rows through one condition (`GenericRepository.live`), and `SoftDeleter` adds `Restore` and `PurgeDeleted` (a purge worker removes old rows). The option fits `GenericRepository` entities that embed `Base` (numeric ID); `ListPaged` is Item-specific. `Create` clears a `deleted_at` that the body carries. Soft delete is not erasure: the data stays in the table, the backups and the outbox payloads until a purge runs, so the retention is set on purpose. The template ships no soft delete entity and Items stay hard; the steps are in `EXTENDING.md`, "Adding soft delete to an entity".
- **Server-owned version.** An item create forces `version` to 1; an update compares the stored version, and a mismatch is a 409.
- **Detached post-commit writes.** Side effects that must follow a commit (token blocks, audit entries) run on a context that a leaving client cannot cancel, with a 30 second deadline.
- **Secrets never in the browser.** The access token lives in memory only; the refresh token is an httpOnly cookie that JavaScript cannot read.
- **Same origin.** The refresh cookie is `SameSite=Strict`, so the SPA and the API share one origin; CORS needs no entry for the bundled UI.
- **No domain concepts in the template.** `scripts/check-domain-terms.sh` keeps the vocabulary of the project this template was extracted from out of the tree.

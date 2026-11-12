# KSM Backport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the generic, production-hardened parts of k8s-stack-manager (KSM) into application_template so new projects start with them.

**Architecture:** Port and strip. Each task copies a generic KSM file or package, removes every Kubernetes/stack/Helm concept, and adapts its tests to the template's `Items` reference entity. Four ordered workstreams (WS1 backend foundations, WS2 auth/RBAC/audit, WS3 frontend/CI/AI tooling, WS4 observability/ops) on one branch. Each task is red, green, refactor.

**Tech Stack:** Go (Gin, GORM, MySQL; SQLite for unit tests), React 19 + MUI + Vite + TypeScript + Vitest + Playwright, Docker Compose, GitHub Actions, OpenTelemetry.

**Spec:** `docs/superpowers/specs/2026-11-08-ksm-backport-design.md` (read it first; section numbers below refer to it).

## Conventions

- `WT` = `/Users/olofm/git/application_template/.claude/worktrees/feat-backport-ksm-foundations` (the only place to edit). `KSM` = `/Users/olofm/git/k8s-stack-manager` (read-only port source).
- Backend test command: `cd WT/backend && go test <pkg> -race -count=1`. Unit tests use `-short` and no database. Frontend: `cd WT/frontend && npm test` and `npx tsc --noEmit`.
- Go test style: `testify/assert`, table-driven, `t.Parallel()` in parent and subtests, `tt := tt`. Swagger annotations on every handler. Struct fields ordered for alignment.
- Commits are conventional (`feat:`, `fix:`, `chore:`, `docs:`, `ci:`, `test:`) and end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- After every port, run `scripts/check-domain-terms.sh` (Task 1). A hit is removed or added to the reviewed allowlist with a reason.
- Never `git stash`; never edit files outside `WT` except the read-only KSM source.
- Agent per task is given in **Agent**: `go-api-developer` (backend), `frontend-developer`, `devops-engineer`, `qa-engineer`. Tasks marked **Gate** are run by `code-reviewer` after the last task of a workstream; its findings are fixed or logged in spec section 11 before the next workstream starts.

## Global Constraints

- Template has no `k8s.io/*` module in `backend/go.mod`, and no cluster, helm, kubeconfig, deployer, gitprovider or quota concept in code, config or docs.
- MySQL only. SQLite only in unit tests.
- Go version is the one in KSM `backend/go.mod` (1.27.4); Dockerfiles use matching images and run non-root.
- Auth is off by default (`AUTH_ENABLED=false`). With `AUTH_ENABLED=true` startup fails unless `JWT_SECRET` is set and at least 16 characters. No default admin password. `SELF_REGISTRATION` defaults to false.
- Roles are exactly `admin`, `devops`, `user`.
- A 500 returns `"Internal server error"`. A cancelled request context returns 499. `context.DeadlineExceeded` stays 500. Oversized body returns 413. `ctxutil.WriteTimeout` is 30 s. No bare `context.WithoutCancel`.
- Defaults: `RATE_LIMIT` 100/min, `LOGIN_RATE_LIMIT` 10/min, `LOG_LEVEL` info, `ENABLE_SWAGGER` false, `TRUSTED_PROXIES` empty, `SESSION_STORE` mysql, `PPROF_ADDR` 127.0.0.1:6060, `METRICS_ADDR` :9090, `LEADER_ELECTION_ENABLED` false, `OTEL_ENABLED` false.
- Existing migrations are never edited or renumbered. Existing Items behavior with auth off is unchanged.
- Coverage: every package added or ported reaches 80%; `database`, `config`, `schema` do not drop below 44.8%, 76.5%, 71.9%.
- Mempalace tools and `git-provider` / `helm-values` agents are not ported. No sharded e2e, no `release-drafter`, no Helm chart.

## Review Focus

1. Auth off, request carries a garbage `Authorization` header: Items is still served (not 401). Pinned in Task 21.
2. Auth on, a logged-out or disabled user's open WebSocket is closed, and a new `/ws` upgrade with that token is refused. Pinned in Task 21.
3. Two replicas with election enabled: exactly one leader; takeover after lease expiry; a failed lease renewal (database down) stops leader workers rather than keeping them running. Pinned in Task 34.
4. Items concurrent update with a stale `version` still returns 409 after the lock-retry change, and a retried deadlock does not apply the write twice. Pinned in Task 8.
5. `X-Forwarded-For` sent by an untrusted peer does not change the rate-limit key; malformed JSON gives 400, not 500; migrations run cleanly twice and on a database that already holds the old `users` table. Pinned in Tasks 7 and 13.

## File Structure

Backend additions: `internal/ctxutil/`, `internal/leader/` (group, then lease), `internal/replica/`, `internal/sessionstore/`, `internal/auth/` (OIDC), `internal/telemetry/`, `internal/api/handlers/{errors,detach,ui_config,auth*,users,api_keys,audit_logs}.go`, `internal/api/middleware/{auth,combined_auth,role,audit,ws_redact,metrics,otel}.go`, `internal/models/{user,refresh_token,api_key,audit_log,replica_heartbeat}.go`, `internal/database/{base_repository,lock_retry,gorm_logger,*_repository}.go`, `api/bootstrap.go`. Frontend additions: `src/theme/`, `src/context/{ThemeContext,AuthContext,UIConfigContext}.tsx`, `src/components/{ErrorBoundary,ConfirmDialog,EmptyState,LoadingState,ProtectedRoute}/`, `src/pages/{Login,AuthCallback,NotFound,Admin/Users}/`, `src/api/refresh.ts`. Root: `scripts/`, `config/otel/`, `loadtest/`, `.github/workflows/`, `.claude/`, `docs/claude/`, `CLAUDE.md` files.

---

## WS1. Backend foundations

### Task 0: Baseline

**Agent:** qa-engineer. **Files:** Create `docs/superpowers/baseline.md`.

- [ ] **Step 1:** Run `go build ./... && go vet ./... && go test ./... -cover -short` in `WT/backend` and `npm ci && npm test && npx tsc --noEmit` in `WT/frontend`. Expected: all pass, 64 frontend tests (matches spec section 4).
- [ ] **Step 2:** Start the stack (`docker compose up -d db backend frontend` from `WT`) and run `npm run test:e2e` in `WT/frontend`. Record pass/fail per spec file in `baseline.md` (failing specs are existing failures, not regressions).
- [ ] **Step 3:** Commit `docs: record baseline before KSM backport`.

### Task 1: Domain-term check

**Agent:** devops-engineer. **Files:** Create `scripts/check-domain-terms.sh`, `scripts/domain-terms.allowlist`, `scripts/check-domain-terms.test.sh`.
**Produces:** `scripts/check-domain-terms.sh [--self-test]` exits 1 and prints `file:line:term` when a forbidden term appears in tracked files outside the allowlist. Forbidden: `kubeconfig`, `k8s.io`, `kubernetes`, `helm`, `deployer`, `gitprovider`, `quota`, `azure`, `azurite`, `mempalace`, `cluster` (word), `namespace` and `stack` (word, each needs an allowlist entry per use). Allowlist lines are `path-glob|term|reason`. The script ignores `docs/superpowers/**` and `scripts/domain-terms.allowlist`.

- [ ] **Step 1:** Write `check-domain-terms.test.sh` with temp-dir fixtures: a file containing `kubeconfig` fails with exit 1 and names the line; the same file passes when allowlisted; a clean file passes.
- [ ] **Step 2:** Run `bash scripts/check-domain-terms.test.sh`. Expected: FAIL (script missing).
- [ ] **Step 3:** Implement the script with `git ls-files` + `grep -inwE`. Run it on the repo and add allowlist entries only for generic uses (for example "call stack"); leave Azure hits failing for Task 2.
- [ ] **Step 4:** Run the test script. Expected: PASS.
- [ ] **Step 5:** Commit `chore: add domain-term check`.

### Task 2: Remove Azure

**Agent:** go-api-developer. **Files:** Delete `backend/internal/database/azure/`, `azure_repository_test.go`, `backend/api/azure_test.go`, `table_test.go.bak`, `backend/lint_result.txt`. Modify `backend/internal/config/config.go` and `config_test.go`, `backend/internal/database/repository.go`, `backend/api/main.go`, `main_test.go`, `main_integration_test.go`, `backend/.env.example`, `backend/config/.env.example`, `docker-compose.yml`, `Makefile`, `backend/go.mod`/`go.sum`, `.github/copilot-instructions.md`, `.github/instructions/{go,scalability,security}.instructions.md`, `.github/agents/{go-api-developer,devops-engineer,qa-engineer}.md`.

- [ ] **Step 1:** Add `TestLoadConfigIgnoresDBTypeAzure` in `config_test.go`: with `DB_TYPE=azure` set, `LoadConfig()` returns an error containing `unsupported`; with `DB_TYPE=mysql` or unset it succeeds. Add a build-level assertion by running the domain-term check (Step 2).
- [ ] **Step 2:** Run `go test ./internal/config -run TestLoadConfigIgnoresDBTypeAzure` (FAIL) and `scripts/check-domain-terms.sh` (reports Azure hits).
- [ ] **Step 3:** Delete and edit the files above; `go mod tidy`. `DB_TYPE` keeps `mysql` and `sqlite` only.
- [ ] **Step 4:** Run `go build ./... && go vet ./... && go test ./... -short` (PASS) and the domain-term check (no Azure hits).
- [ ] **Step 5:** Commit `refactor: remove Azure Table Storage backend`.

### Task 3: Toolchain, Dockerfile, gosec

**Agent:** devops-engineer. **Files:** Modify `backend/go.mod`, `backend/Dockerfile`, `frontend/Dockerfile` (base image only; Node upgrade is Task 23), `backend/Makefile`, `Makefile`; Create `backend/.dockerignore`, `frontend/.dockerignore`, `scripts/gosec.sh`.
**Produces:** `make gosec` runs `scripts/gosec.sh` (pinned gosec built with the local Go, run inside `backend/`); `make lint` runs `go vet` and the frontend lint.

- [ ] **Step 1:** Add `scripts/check-dockerfiles.sh` that fails if a Dockerfile has no `USER` other than root in its production stage or uses an image older than the pinned versions. Run it (FAIL on current files).
- [ ] **Step 2:** Set `go 1.27.4` (no `toolchain` line) and `golang:1.27.4` images; production stage `alpine` with a non-root user for the backend; copy KSM `backend/Dockerfile` dev/build/prod stage structure without the `helm` stage. `.dockerignore` files copied from KSM.
- [ ] **Step 3:** Port `$KSM/scripts/gosec.sh`; add the Make targets; remove `golint` usage.
- [ ] **Step 4:** Run `scripts/check-dockerfiles.sh` (PASS), `docker build -t tpl-backend ./backend`, `make gosec`, `make lint`. Fix findings or add narrowly scoped `#nosec` with a reason.
- [ ] **Step 5:** Commit `build: Go 1.27, non-root images, gosec`.

### Task 4: ctxutil

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/ctxutil/{detach.go,detach_test.go}`. **Files:** Create `backend/internal/ctxutil/detach.go`, `detach_test.go`.
**Produces:** `const WriteTimeout = 30 * time.Second`; `func Detach(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc)` (keeps values, drops cancellation, applies `timeout`).

- [ ] **Step 1:** Port the tests: `TestDetachSurvivesParentCancel` (child `Err()` is nil after parent cancel), `TestDetachKeepsValues`, `TestDetachHasDeadline` (deadline within `timeout`), `TestNoUnboundedDetach` (walks `backend/` Go files and fails on any `context.WithoutCancel` outside `ctxutil`).
- [ ] **Step 2:** Run `go test ./internal/ctxutil` (FAIL: package missing).
- [ ] **Step 3:** Implement `Detach` and `WriteTimeout`.
- [ ] **Step 4:** Run tests with `-race -count=3` (PASS).
- [ ] **Step 5:** Commit `feat(ctxutil): detached contexts with deadline`.

### Task 5: Error and detach helpers

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/api/handlers/{errors.go,detach.go}` and their tests. **Files:** Create `backend/internal/api/handlers/errors.go`, `detach.go`, `errors_test.go`; Modify `items.go`, `items_test.go`.
**Consumes:** `ctxutil.Detach`, `dberrors` sentinels. **Produces:** `mapError(err error, entityName string) (int, string)`, `cancelledRequestError(err error) (int, string, bool)`, `respondCancelled(c *gin.Context, err error) bool`, `detachedCtx(c *gin.Context) (context.Context, context.CancelFunc)`. `handleDBError` in `items.go` delegates to `mapError`.

- [ ] **Step 1:** Tests: table for `mapError` (validation 400, not found 404 with entity name in message, duplicate 409, not implemented 501, other 500 with message exactly `"Internal server error"`); `context.Canceled` gives 499 and `DeadlineExceeded` gives 500; `TestItemsUpdateCancelledReturns499`; `TestDetachedCtxSurvivesRequestCancel`.
- [ ] **Step 2:** Run `go test ./internal/api/handlers -run 'MapError|Cancelled|DetachedCtx'` (FAIL).
- [ ] **Step 3:** Port both files; switch Items create/update/delete writes to `detachedCtx(c)` with `defer cancel()`; reads keep `c.Request.Context()`.
- [ ] **Step 4:** Run `go test ./internal/api/handlers -race -count=1` (all existing Items tests PASS).
- [ ] **Step 5:** Commit `feat(handlers): mapError, 499 on cancel, detached writes`.

### Task 6: Config and logging

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/config/{config.go,trusted_proxies.go,trusted_proxies_test.go}` (relevant parts), `$KSM/backend/api/bootstrap.go` (`setupLogging` equivalent). **Files:** Modify `backend/internal/config/config.go`, `config_test.go`; Create `backend/internal/config/trusted_proxies.go`, `trusted_proxies_test.go`; Modify `backend/api/main.go`, `backend/.env.example`.
**Produces:** `Config.LogLevel slog.Level`, `ServerConfig.TrustedProxies []string`, `ServerConfig.EnableSwagger bool`, `ServerConfig.RateLimit int32`, `ServerConfig.LoginRateLimit int32`, `ServerConfig.AppDebug bool`, `func ParseTrustedProxies(s string) ([]string, error)`, `const MySQLSessionParams`, `func (c *Config) GinMode() string`.

- [ ] **Step 1:** Tests: `LOG_LEVEL=bogus` makes `LoadConfig` fail; `GinMode()` is `release` when `APP_DEBUG=false`, `debug` when true, and `GIN_MODE` wins when set; `ParseTrustedProxies` rejects `0.0.0.0/0`, `::/0`, IPv4-mapped IPv6 and invalid entries and accepts IPs and CIDRs; defaults `RATE_LIMIT=100`, `LOGIN_RATE_LIMIT=10`; the MySQL DSN contains `MySQLSessionParams`.
- [ ] **Step 2:** Run `go test ./internal/config` (FAIL).
- [ ] **Step 3:** Port the fields and parsers; apply `LOG_LEVEL` to the default `slog` logger in `main`; invalid value stops startup.
- [ ] **Step 4:** Run `go test ./internal/config -race -count=3`; coverage of `config` must be at least 76.5%.
- [ ] **Step 5:** Commit `feat(config): trusted proxies, log level, rate limits, MySQL session params`.

### Task 7: Security middleware and routes wiring

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/api/middleware/{middleware.go (SecurityHeaders),ws_redact.go}` and tests; `$KSM/backend/internal/api/routes/routes.go` (middleware order, limiters). **Files:** Modify `backend/internal/api/middleware/middleware.go`, `middleware_test.go`; Create `ws_redact.go`, `ws_redact_test.go`; Modify `backend/internal/api/routes/routes.go`, `routes_test.go`, `handlers/rate_limiter.go`.
**Produces:** `middleware.SecurityHeaders() gin.HandlerFunc`, `middleware.RedactWSToken() gin.HandlerFunc`. Global order: RequestID, RedactWSToken, Logger, Recovery, SecurityHeaders, CORS, MaxBodySize(1 MB). `SetupRoutes` calls `router.SetTrustedProxies(cfg.Server.TrustedProxies)` and returns `*RateLimiters` with `Stop()`; login route gets `LoginRateLimit`.

- [ ] **Step 1:** Tests: security headers present on every response (`X-Content-Type-Options: nosniff`, `X-Frame-Options`, `Referrer-Policy`); `?token=abc` never appears in the logged path; body over 1 MB gives 413; malformed JSON on `POST /api/v1/items` gives 400; with no trusted proxies, a request with `X-Forwarded-For: 1.2.3.4` is rate-limited by the TCP peer, not `1.2.3.4` (send 101 requests with different forged values from one peer, expect 429); with the peer in `TrustedProxies`, the forwarded address is used.
- [ ] **Step 2:** Run `go test ./internal/api/... -run 'Security|Redact|TooLarge|Malformed|TrustedProxy'` (FAIL).
- [ ] **Step 3:** Port the middleware and routes changes.
- [ ] **Step 4:** Run `go test ./internal/api/... -race -count=1` (PASS).
- [ ] **Step 5:** Commit `feat(api): security headers, token redaction, trusted proxies`.

### Task 8: Database base repository and logging

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/database/{base_repository.go,lock_retry.go,gorm_logger.go,helpers.go}` and tests. **Files:** Create the same names under `backend/internal/database/`; Modify `repository.go` (`GenericRepository`), `factory.go`, `models.Repository` users in handlers.
**Produces:** `GenericRepository.ListPaged(ctx, dest, limit, offset int, filter) (total int64, err error)` for Items with a `ItemFilter` struct; lock-retry helper `retryOnLock(ctx, fn func() error) error` (retries MySQL 1213/1205 up to 3 times with backoff, honors ctx); `newGormLogger(debug bool) logger.Interface` writing through `slog.Default()` at each call, redacting values when `debug` is false, duplicate key (1062) at warn, cancelled queries at debug, `gorm.ErrRecordNotFound` never logged.

- [ ] **Step 1:** Tests: `TestRetryOnLockRetriesDeadlockOnce` (fn fails with 1213 then succeeds; fn is called exactly twice and its side effect applied once); `TestRetryOnLockStopsOnContextCancel`; `TestGormLoggerRedactsValuesWhenNotDebug`; `TestGormLoggerDuplicateKeyIsWarn`; `TestItemsListPagedTotalMatchesFilter`; `TestItemUpdateStaleVersionReturns409` (Update with outdated `Version` returns the version-mismatch error and the handler maps it to 409).
- [ ] **Step 2:** Run the new tests (FAIL).
- [ ] **Step 3:** Port the files; route `GenericRepository` writes through `retryOnLock`; Items list endpoint uses `ListPaged` (response keeps its current JSON shape plus existing `limit`/`offset` params).
- [ ] **Step 4:** `go test ./internal/database ./internal/api/handlers -race -count=1`; `database` coverage at least 44.8%; new files at least 80%.
- [ ] **Step 5:** Commit `feat(database): lock retry, ListPaged, slog GORM logger`.

### Task 9: leader.Group and ui-config

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/leader/group.go` and test; `$KSM/backend/internal/api/handlers/ui_config.go` (generic fields only). **Files:** Create `backend/internal/leader/group.go`, `group_test.go`, `backend/internal/api/handlers/ui_config.go`, `ui_config_test.go`; Modify `routes.go`.
**Produces:** `leader.Worker{Name string; Run func(ctx context.Context)}`; `leader.NewGroup(stopTimeout time.Duration, workers ...Worker) *Group`; methods `Add(Worker)`, `Names() []string`, `Running() bool`, `Start(ctx)`, `Stop() bool`, `Close(timeout) bool`. `GET /api/v1/ui-config` (public, `Cache-Control: public, max-age=300`) returns `UIConfigResponse{Title string; AuthEnabled bool}` from `APP_TITLE` (default "Application Template") and `AUTH_ENABLED`. Also add `config.AuthConfig.Enabled bool` (`AUTH_ENABLED`, default false) as a stub field used by this endpoint.

- [ ] **Step 1:** Tests: group starts all workers once, `Stop` cancels and waits, `Close` returns false when a worker ignores cancel past the timeout, a second `Start` is a no-op; `GET /api/v1/ui-config` returns 200 `{"title":"Application Template","auth_enabled":false}` without credentials and the cache header; with `AUTH_ENABLED=true` it returns `auth_enabled: true`.
- [ ] **Step 2:** Run (FAIL).
- [ ] **Step 3:** Port `group.go` unchanged; write the handler with Swagger annotations.
- [ ] **Step 4:** `go test ./internal/leader ./internal/api/... -race -count=3` (PASS).
- [ ] **Step 5:** Commit `feat: leader worker group and public ui-config`.

### Task 10: WebSocket hub

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/websocket/{hub.go,client.go,revocable.go,message.go}` and tests (without instance/deployment message types). **Files:** Modify `backend/internal/websocket/{hub.go,client.go,message.go}`, tests; Create `revocable.go`, `revocable_test.go`; Modify `handlers/websocket.go`, `mock_broadcast_sender.go`.
**Produces:** `Hub.Subscribe(c *Client, topic string)`, `Unsubscribe(c, topic)`, `BroadcastToTopic(topic string, msg []byte)`, `BroadcastToUser(userID string, msg []byte)`, `DisconnectUser(userID string) int`, `DisconnectToken(tokenID string) int`, `DisconnectAPIKey(keyID string) int`; `type Revocable interface` as in KSM; `websocket.ClientRevoker` interface with `DisconnectUser`, `DisconnectToken`, `DisconnectAPIKey`. Client messages `subscribe`/`unsubscribe` carry `{"topic": "..."}`. Existing `Broadcast` and the Items `item.*` events are unchanged.

- [ ] **Step 1:** Tests: a client subscribed to topic A receives `BroadcastToTopic("A")` and not topic B; `BroadcastToUser` reaches only that user's sockets; `DisconnectUser` closes them and returns the count; a slow client is dropped without blocking others; all existing hub and Items websocket tests still pass.
- [ ] **Step 2:** Run `go test ./internal/websocket ./internal/api/handlers -run 'Topic|User|Disconnect'` (FAIL).
- [ ] **Step 3:** Port with topics in place of KSM's `instance_id`.
- [ ] **Step 4:** `go test ./internal/websocket ./internal/api/handlers -race -count=3`; websocket coverage at least 85.7%.
- [ ] **Step 5:** Commit `feat(websocket): topics, per-user sends, revocation`.

### Task 11: Bootstrap and graceful shutdown

**Agent:** go-api-developer. **Port from:** `$KSM/backend/api/bootstrap.go` (structure and shutdown order only). **Files:** Create `backend/api/bootstrap.go`, `bootstrap_test.go`; Modify `backend/api/main.go`.
**Produces:** helpers `initDatabase(cfg) (*database.Database, error)`, `buildRouter(cfg, deps) *gin.Engine` (sets Gin mode from `cfg.GinMode()`, registers Swagger only if `EnableSwagger`), `startHTTPServer(cfg, router) *http.Server`, `shutdown(ctx, s *services)` in this order: HTTP servers, leader group, in-flight work, hub, telemetry hook (no-op until Task 32), database. `main` is under 60 lines.

- [ ] **Step 1:** Tests: `TestSwaggerRouteOnlyWhenEnabled` (404 when false, 200 when true); `TestShutdownOrder` (fake services record call order: server, leader group, hub, telemetry, db); `TestShutdownCompletesWithinTimeout` with a hung worker.
- [ ] **Step 2:** Run `go test ./api -run 'Swagger|Shutdown'` (FAIL).
- [ ] **Step 3:** Port the split and the order; wire `leader.Group` (empty for now).
- [ ] **Step 4:** `go test ./... -short -race`, then `make dev-backend` and `curl localhost:8081/health/live` returns 200.
- [ ] **Step 5:** Commit `refactor(api): bootstrap helpers and ordered graceful shutdown`.

### Task 12: WS1 gate

**Agent:** code-reviewer. Review `git diff main...HEAD` for WS1 against spec sections 5 (WS1), 7, 8. Run `go test ./... -race -count=3 -short`, `make lint gosec`, domain-term check. Fix or log findings, then commit `chore: WS1 review fixes`.

---

## WS2. Auth, RBAC and audit

### Task 13: Models and migrations

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/models/{user,refresh_token,api_key,audit_log}.go` and `$KSM/backend/internal/database/{refresh_token,api_key,audit_log}_repository.go` (+ user repo). **Files:** Modify `backend/internal/models/models.go` (remove old `User`), `validation.go`, `backend/internal/database/migrations.go`; Create `models/{user,refresh_token,api_key,audit_log}.go`, `database/{user,refresh_token,api_key,audit_log}_repository.go` with tests.
**Produces:** `User{ID string (uuid, size 36); Username, Email, PasswordHash, DisplayName, Role string; AuthProvider string; Disabled bool; CreatedAt, UpdatedAt}`; `RefreshToken`, `APIKey`, `AuditLog` as in KSM; repository interfaces `UserRepository`, `RefreshTokenRepository`, `APIKeyRepository`, `AuditLogRepository`, all methods taking `ctx` first. New migration version `20260101000001` `replace_users_and_add_auth_tables`: drop `users`, create `users`, `refresh_tokens`, `api_keys`, `audit_logs`; `Down` reverses.

- [ ] **Step 1:** Tests: SQLite-backed repo tests for create/find/update/delete and unique username; `TestMigrationRunsTwice` (second run is a no-op); `TestMigrationOnDatabaseWithOldUsersTable` (apply migrations 1-2, insert an old-shape user row, apply the new one: table has the new columns, no error); `TestUserValidateRole` (only `admin`, `devops`, `user` valid); MySQL integration `TestDatabaseAuthTables` under the `integration` tag.
- [ ] **Step 2:** Run (FAIL).
- [ ] **Step 3:** Port models, repos and migration; update `AutoMigrate` registration.
- [ ] **Step 4:** `go test ./internal/models ./internal/database -race -count=1`; `make test-backend-all` for the integration test.
- [ ] **Step 5:** Commit `feat(models): users, refresh tokens, API keys, audit log`.

### Task 14: Session store

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/sessionstore/{sessionstore.go,memory.go,mysql.go}` and tests, **without** `CLIAuthData` and the four CLI-auth methods. **Files:** Create `backend/internal/sessionstore/*.go`; add a `session_blocks` / `oidc_states` migration `20260101000002`.
**Produces:** `type SessionStore interface { BlockToken(ctx, jti string, expiresAt time.Time) error; IsTokenBlocked(ctx, jti string) (bool, error); BlockUser(ctx, userID string, until time.Time) error; IsUserBlocked(ctx, userID string, issuedAt time.Time) (bool, error); UnblockUser(ctx, userID string) error; BlockSession(ctx, sessionID string, until time.Time) error; BlockSessions(ctx, sessionIDs []string, until time.Time) error; IsSessionBlocked(ctx, sessionID string) (bool, error); SaveOIDCState(ctx, state string, data OIDCStateData, ttl time.Duration) error; ConsumeOIDCState(ctx, state string) (*OIDCStateData, error); Cleanup(ctx) error; Stop() }`; `NewMemory() SessionStore`, `NewMySQL(db *gorm.DB) SessionStore`.

- [ ] **Step 1:** Port the tests for both backends via one shared table: a blocked token reads blocked until expiry then not; a user block applies only to tokens issued before the block; `ConsumeOIDCState` returns data once then nil; `Cleanup` removes expired rows.
- [ ] **Step 2:** Run (FAIL).
- [ ] **Step 3:** Port both implementations.
- [ ] **Step 4:** `go test ./internal/sessionstore -race -count=3`; coverage at least 80%.
- [ ] **Step 5:** Commit `feat(sessionstore): token blocklist and OIDC state`.

### Task 15: Auth config and JWT middleware

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/config/config.go` (`AuthConfig`, validation), `$KSM/backend/internal/api/middleware/{auth.go,role.go,combined_auth.go}` and tests. **Files:** Modify `config.go`, `config_test.go`; Create `middleware/{auth,role,combined_auth}.go` with tests.
**Produces:** `AuthConfig{Enabled bool; JWTSecret string; JWTExpiration, AccessTokenExpiration, SessionIdleTimeout (30m), SessionMaxLifetime (12h) time.Duration; AdminUsername ("admin"), AdminPassword string; SelfRegistration bool}`; `AuthConfig.Validate() error`. Middleware: `ValidateJWT(tokenStr, secret string) (*Claims, error)`, `AuthRequiredWithOptions(opts JWTAuthOptions) gin.HandlerFunc`, `GenerateToken(userID, username, role, secret string, expiration time.Duration) (string, error)`, `RequireRole(roles ...string)`, `RequireAdmin()`, `RequireDevOps()`, `RequireUser()`, `CombinedAuth(deps APIKeyAuthDeps) gin.HandlerFunc`, context getters `GetUserIDFromContext`, `GetUsernameFromContext`, `GetRoleFromContext`, `GetJTIFromContext`, `GetSessionIDFromContext`.

- [ ] **Step 1:** Tests: with `AUTH_ENABLED=true`, `LoadConfig` fails when `JWT_SECRET` is empty or 15 characters, passes at 16; `AUTH_ENABLED=false` ignores a missing secret; `ADMIN_PASSWORD` empty is allowed (no admin created); a token signed with another secret gives 401; an expired token 401; a blocked `jti` 401 with message from `RevocationMessage`; role gates: `user` on an admin route 403, `devops` passes `RequireDevOps`, `admin` passes both.
- [ ] **Step 2:** Run (FAIL).
- [ ] **Step 3:** Port, removing stack-access and `stackctl`-specific claims.
- [ ] **Step 4:** `go test ./internal/config ./internal/api/middleware -race -count=1` (PASS).
- [ ] **Step 5:** Commit `feat(auth): JWT middleware, roles, auth config`.

### Task 16: Auth handler, identity

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/api/handlers/auth.go` lines for `NewAuthHandler`, `Login`, `Register`, `GetCurrentUser`, `EnsureAdminUser`, `dummyPasswordHash`, `login_failure.go`; tests. **Files:** Create `backend/internal/api/handlers/auth.go`, `login_failure.go`, `auth_test.go`.
**Produces:** `NewAuthHandler(userRepo models.UserRepository, cfg *config.AuthConfig, oidcCfg *config.OIDCConfig) *AuthHandler`; `Login`, `Register`, `GetCurrentUser`, `EnsureAdminUser(ctx)`; setters `SetRefreshTokenRepo`, `SetSessionStore`, `SetWebSocketRevoker`. Routes `POST /api/v1/auth/login`, `POST /api/v1/auth/register` (404 unless `SELF_REGISTRATION`), `GET /api/v1/auth/me`.

- [ ] **Step 1:** Tests: valid login returns 200 with an access token and user; wrong password and unknown user both return 401 with the same body and similar timing path (dummy hash used); repeated failures from one username are throttled (429 after the KSM threshold); a disabled user gets 401; `EnsureAdminUser` creates `admin` only when `ADMIN_PASSWORD` is set and is idempotent; `Register` is 404 by default.
- [ ] **Step 2:** Run (FAIL).
- [ ] **Step 3:** Port only these handlers; passwords hashed with bcrypt (`golang.org/x/crypto`).
- [ ] **Step 4:** `go test ./internal/api/handlers -race -count=1` (PASS).
- [ ] **Step 5:** Commit `feat(auth): login, register, me, admin bootstrap`.

### Task 17: Refresh tokens, sessions, logout

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/api/handlers/auth.go` (`Refresh`, grace reuse, `Logout`, `LogoutAll`, `CleanupExpiredTokens`, cookie helpers), `auth_recheck.go`, middleware session-activity tests. **Files:** Create `handlers/auth_refresh.go`, `auth_logout.go`, `auth_recheck.go` and tests.
**Produces:** `POST /api/v1/auth/refresh` (httpOnly cookie, rotation, reuse detection revokes the family, `SESSION_MAX_LIFETIME` cap), `POST /api/v1/auth/logout`, `POST /api/v1/auth/logout-all`; `(*AuthHandler).CleanupExpiredTokens(ctx)` for the leader worker.

- [ ] **Step 1:** Tests: refresh rotates the token and the old one fails; reuse of a consumed token inside the grace window succeeds once, outside it revokes the whole family; a session past `SESSION_MAX_LIFETIME` is refused; logout blocks the `jti` and clears the cookie; logout-all blocks all sessions of the user; a client cancel during refresh gives 499 and revokes nothing; `CleanupExpiredTokens` removes only expired rows.
- [ ] **Step 2:** Run (FAIL).
- [ ] **Step 3:** Port; all writes after the identity check use `detachedCtx(c)`.
- [ ] **Step 4:** `go test ./internal/api/handlers -race -count=3` (PASS).
- [ ] **Step 5:** Commit `feat(auth): refresh rotation, sessions, logout`.

### Task 18: OIDC

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/auth/oidc.go`, `handlers/oidc.go`, config `OIDCConfig`, tests. **Files:** Create `backend/internal/auth/oidc.go`, `oidc_test.go`, `handlers/oidc.go`, `oidc_test.go`; Modify `config.go`.
**Produces:** `OIDCConfig` (`OIDC_ENABLED`, issuer, client id/secret, redirect URL, `OIDC_ADMIN_ROLES`); `GET /api/v1/auth/oidc/login` (PKCE + state saved via `SaveOIDCState`), `GET /api/v1/auth/oidc/callback`. Disabled by default; routes return 404 when disabled. Do not port the CLI-auth flow.

- [ ] **Step 1:** Tests with a fake OIDC provider (`httptest`): login redirect carries `code_challenge` and `state`; callback with unknown or reused state gives 400; successful callback provisions a user with `auth_provider=oidc` and role from `OIDC_ADMIN_ROLES`; disabled gives 404.
- [ ] **Step 2:** Run (FAIL). **Step 3:** Port. **Step 4:** `go test ./internal/auth ./internal/api/handlers -race -count=1` (PASS).
- [ ] **Step 5:** Commit `feat(auth): optional OIDC with PKCE`.

### Task 19: API keys

**Agent:** go-api-developer. **Port from:** `handlers/api_keys.go`, `middleware/combined_auth.go` key branch and tests. **Files:** Create `handlers/api_keys.go`, `api_keys_test.go`; Modify `combined_auth.go`.
**Produces:** `POST/GET/DELETE /api/v1/api-keys` (own keys; admin can list all), `X-API-Key` accepted by `CombinedAuth`; keys stored hashed, shown once at creation, expiry honored, rate-limited verification.

- [ ] **Step 1:** Tests: created key authenticates once and the raw key is never returned again; expired and revoked keys give 401; a key cannot exceed its owner's role; deleting a key disconnects its sockets via `DisconnectAPIKey`.
- [ ] **Step 2-4:** Run (FAIL), port, run `go test ./internal/api/... -race -count=1` (PASS).
- [ ] **Step 5:** Commit `feat(auth): API keys`.

### Task 20: Audit and user admin

**Agent:** go-api-developer. **Port from:** `middleware/audit.go`, `handlers/{audit_logs,users,admin}.go` (users only), tests. **Files:** Create `middleware/audit.go`, `handlers/audit_logs.go`, `handlers/users.go` with tests.
**Produces:** `NewAuditMiddleware(repo AuditLogger) gin.HandlerFunc` (writes on a detached context with a fresh `ctxutil.WriteTimeout`; `SkipAudit(c)`); `GET /api/v1/audit-logs` (admin, filters + paging); `GET/POST/PUT/DELETE /api/v1/users`, `PUT /api/v1/users/:id/role`, `PUT /api/v1/users/:id/disabled` (admin only). Disabling a user blocks the user in the session store and calls `DisconnectUser`. The last admin cannot be demoted, disabled or deleted (409).

- [ ] **Step 1:** Tests: a mutating Items request writes one audit row with user, method, path, status; a failed audit write does not fail the request; non-admin gets 403 on `/users` and `/audit-logs`; disabling a user blocks the user and disconnects the socket; the last-admin rule.
- [ ] **Step 2-4:** Run (FAIL), port, `go test ./internal/api/... -race -count=1` (PASS).
- [ ] **Step 5:** Commit `feat(auth): audit log and user administration`.

### Task 21: Wire auth behind the flag, dual-mode tests

**Agent:** go-api-developer. **Files:** Modify `routes.go`, `bootstrap.go`, `routes_test.go`, `main_integration_test.go`; Create `backend/api/auth_modes_test.go`.
**Produces:** with `AUTH_ENABLED=true`, the `/api/v1` group (except login, register, refresh, OIDC, `ui-config`, `/health/*`) uses `CombinedAuth` + `NewAuditMiddleware`; `/ws` requires a valid token; `EnsureAdminUser` runs at start; a `refresh-token-cleanup` and a `session-store-cleanup` worker are added to the `leader.Group`. With `false`, no auth middleware is registered.

- [ ] **Step 1:** Tests (table over both modes): auth off: Items CRUD works with no header, **and with `Authorization: Bearer garbage` still returns 200** (Review Focus 1); auth on: no token 401, user token on an admin route 403, valid token 200; `/health/live` and `ui-config` open in both modes; logout then a new `/ws` upgrade with that token is refused and an already open socket is closed (Review Focus 2); startup with `AUTH_ENABLED=true` and no `JWT_SECRET` exits non-zero.
- [ ] **Step 2-4:** Run (FAIL), wire, `go test ./... -race -count=1 -short` (PASS).
- [ ] **Step 5:** Commit `feat: gate API behind AUTH_ENABLED`.

### Task 22 (WS2 gate)

**Agent:** code-reviewer. Review WS2 against spec WS2 and section 8; run `go test ./... -race -count=3 -short`, `make gosec`, domain-term check, `grep k8s.io backend/go.mod` (empty). Fix or log, commit `chore: WS2 review fixes`.

---

## WS3. Frontend, CI and AI tooling

### Task 23: Frontend dependency upgrade

**Agent:** frontend-developer. **Files:** Modify `frontend/package.json`, `package-lock.json`, `tsconfig*.json`, `vite.config.ts`, `frontend/Dockerfile`, `README.md`, any source the upgrade breaks.
**Produces:** Node 22.12+ (Dockerfile `node:26-alpine`, production `nginxinc/nginx-unprivileged:alpine`), MUI 9, Vite 8, TypeScript 6, Vitest 5, ESLint 10 config (`eslint.config.js`, `npm run lint` = `tsc --noEmit && eslint .`).

- [ ] **Step 1:** Record `npm test` (64 pass) and `tsc --noEmit` (clean) as the gate. Add `src/__tests__/theme-smoke.test.tsx` rendering `<App />` inside the router and expecting the Items heading.
- [ ] **Step 2:** Bump versions per KSM `frontend/package.json`; run tests (expect some FAIL).
- [ ] **Step 3:** Fix breakages with the smallest changes; add the ESLint config ported from KSM.
- [ ] **Step 4:** `npm run lint`, `npm test`, `npm run build`, `docker build ./frontend` all PASS with the same 64 tests plus the smoke test.
- [ ] **Step 5:** Commit `build(frontend): upgrade MUI, Vite, TypeScript, Vitest; add ESLint`.

### Task 24: Theme and shared components

**Agent:** frontend-developer. **Port from:** `$KSM/frontend/src/theme/*`, `context/ThemeContext.tsx`, `components/{ErrorBoundary,ConfirmDialog,EmptyState,LoadingState}`, `utils/apiError.ts`, `hooks/useUnsavedChanges.ts`, `pages/NotFound`, and their tests. **Files:** Create the same paths under `frontend/src/`; Modify `App.tsx`, `main.tsx`, `pages/Items/index.tsx` (use `LoadingState`, `EmptyState`, `ConfirmDialog` for delete), `routes.tsx`.
**Produces:** `ThemeProvider` with light/dark and persisted choice; `ErrorBoundary` ({children, fallback?}); `ConfirmDialog` ({open,title,message,confirmLabel,onConfirm,onCancel}); `EmptyState` ({title,description?,action?}); `LoadingState` ({label?}); `getApiErrorMessage(err: unknown): string`; `useUnsavedChanges(dirty: boolean)`; `*` route renders `NotFound`.

- [ ] **Step 1:** Port tests: ErrorBoundary shows fallback on a throwing child and logs once; ConfirmDialog calls `onConfirm`/`onCancel`; `getApiErrorMessage` handles axios error with `{error}` body, network error, and unknown; theme toggle persists to `localStorage` and survives a throwing `localStorage`; Items delete asks for confirmation.
- [ ] **Step 2-4:** Run (FAIL), port, `npm test` and `npm run lint` PASS.
- [ ] **Step 5:** Commit `feat(frontend): theme and shared components`.

### Task 25: Auth in the frontend

**Agent:** frontend-developer. **Port from:** `$KSM/frontend/src/{context/AuthContext.tsx,components/ProtectedRoute,pages/Login,pages/AuthCallback,api/refresh.ts,utils/jwt.ts}` and tests (without CLI-auth). **Files:** Create under `frontend/src/`: `context/UIConfigContext.tsx`, `context/AuthContext.tsx`, `components/ProtectedRoute/`, `pages/Login/`, `pages/AuthCallback/`, `api/refresh.ts`; Modify `api/client.ts`, `routes.tsx`, `components/Layout`.
**Produces:** `UIConfigProvider` fetches `GET /api/v1/ui-config` once (falls back to `{title:"Application Template", auth_enabled:false}` on failure); `AuthProvider`/`useAuth()` → `{user, role, login(u,p), logout(), isAuthenticated}`; `ProtectedRoute({roles?})` renders children directly when `auth_enabled` is false; `client.ts` adds `Authorization` and on 401 calls `refresh()` once (single-flight) then retries, else redirects to `/login`.

- [ ] **Step 1:** Tests: auth disabled renders protected pages with no login and sends no `Authorization`; auth enabled redirects to `/login` when anonymous; successful login stores the token in memory and shows the user; two concurrent 401s trigger one refresh; failed refresh logs out; `ProtectedRoute roles={['admin']}` blocks a `user`; Layout shows Logout only when auth is enabled.
- [ ] **Step 2-4:** Run (FAIL), port, `npm test`, `npm run lint` PASS.
- [ ] **Step 5:** Commit `feat(frontend): optional auth, login and token refresh`.

### Task 26: Admin Users page and accessibility

**Agent:** frontend-developer. **Port from:** `pages/Admin/Users`, `e2e/{a11y.ts,accessibility.spec.ts}` (generic pages only), `tsconfig.a11y.json`. **Files:** Create `frontend/src/pages/Admin/Users/`, `frontend/e2e/{a11y.ts,accessibility.spec.ts}`; Modify `routes.tsx`, Layout nav (admin only, auth only), `package.json` (`@axe-core/playwright`).
**Produces:** `/admin/users` (admin role): list, create, change role, disable/enable, delete with `ConfirmDialog`; last-admin 409 shown with the server message.

- [ ] **Step 1:** Tests: list renders users; role change calls `PUT /users/:id/role`; delete requires confirmation; 409 message is displayed; non-admin is redirected.
- [ ] **Step 2-4:** Run (FAIL), port, `npm test` PASS.
- [ ] **Step 5:** Commit `feat(frontend): admin users page and a11y checks`.

### Task 27: Playwright e2e, both modes

**Agent:** qa-engineer. **Files:** Create `frontend/e2e/{auth.spec.ts,global-setup.ts}`; Modify `playwright.config.ts`, `Makefile` (`test-e2e`, `test-e2e-auth`), existing specs only where the upgrade requires it.
**Produces:** `make test-e2e` runs the existing specs plus `accessibility.spec.ts` with auth off; `make test-e2e-auth` starts the backend with `AUTH_ENABLED=true`, `JWT_SECRET` (test value), `ADMIN_PASSWORD` (test value) and runs `auth.spec.ts`.

- [ ] **Step 1:** Write `auth.spec.ts`: anonymous visit redirects to login; wrong password shows error; admin logs in and sees Items; page reload keeps the session via refresh; logout returns to login and the back button does not show Items; non-admin cannot open `/admin/users`.
- [ ] **Step 2:** Run `make test-e2e-auth` (FAIL until wired).
- [ ] **Step 3:** Wire the config and global setup (create a non-admin user through the API).
- [ ] **Step 4:** `make test-e2e` and `make test-e2e-auth` PASS, compared with `docs/superpowers/baseline.md`.
- [ ] **Step 5:** Commit `test(e2e): cover Items open and authenticated`.

### Task 28: CI workflows

**Agent:** devops-engineer. **Port from:** `$KSM/.github/workflows/{pull-request,codeql,security-scan,docker-build,e2e}.yml`, `$KSM/.github/dependabot.yml`, `.markdownlint.yml`, `.markdownlintignore`, `.trivyignore`, `scripts/check-trivyignore.sh`. **Files:** Modify `.github/workflows/pull-request.yml`; Create `codeql.yml`, `security-scan.yml`, `docker-build.yml`, `e2e.yml`, `.github/dependabot.yml`, `.markdownlint.yml`, `.markdownlintignore`, `.trivyignore`.
**Produces:** `pull-request.yml` triggers on pull requests to `main` and on push to `main`; actions pinned to current major versions (or SHAs as KSM does); Go version from `backend/go.mod` (`go-version-file`); Node from `.nvmrc`/Dockerfile value; jobs: PR title lint, backend `go vet` + `gosec` + tests, frontend lint + build + tests, markdownlint. `e2e.yml`: one job with a MySQL service running both e2e modes. `security-scan.yml`: trivy on both built images and the `.trivyignore` expiry check. No sharding, no release-drafter.

- [ ] **Step 1:** Add `scripts/check-workflows.sh`: fails if any workflow lacks `main` in `pull_request.branches`, uses `@master`/`@v1`-`@v3` of checkout/setup actions, or references `golint`. Run (FAIL on current file).
- [ ] **Step 2:** Port and adapt the workflows.
- [ ] **Step 3:** Run `scripts/check-workflows.sh` (PASS), `actionlint` if available, and `npx markdownlint-cli2 "**/*.md"` (PASS, fixing the table-style warnings in the spec/plan by config or content).
- [ ] **Step 4:** Commit `ci: pull-request on main, security scans, e2e job, dependabot`.

### Task 29: Docs and CLAUDE.md

**Agent:** devops-engineer (docs) with `code-reviewer` reading the result. **Port from:** `$KSM/CLAUDE.md`, `backend/CLAUDE.md`, `frontend/CLAUDE.md`, `ARCHITECTURE.md`, `EXTENDING.md`, `RELEASING.md`, `docs/claude/{structure,auth-sessions,operations}.md`, `.github/copilot-instructions.md`. **Files:** Create `CLAUDE.md`, `backend/CLAUDE.md`, `frontend/CLAUDE.md`, `ARCHITECTURE.md`, `EXTENDING.md`, `RELEASING.md`, `docs/claude/{structure,auth-sessions,operations}.md`; Modify `README.md`, `WIKI.md`, `.github/copilot-instructions.md`, `.github/instructions/*.md` (point to `CLAUDE.md`), `backend/.env.example` (every new variable with a comment; dev-only secrets marked).

- [ ] **Step 1:** Add `scripts/check-env-docs.sh`: every `getEnv*("NAME"` in `backend/internal/config/*.go` must appear in `backend/.env.example` and `docs/claude/operations.md`. Run (FAIL).
- [ ] **Step 2:** Write the docs from the KSM sources, stripped of domain content; document the "Adding a new API resource" steps with `Items` as the example, the auth modes, the optional modules, and a README section "Start a new project from this template" (rename module, change title, set secrets, enable auth).
- [ ] **Step 3:** Run `scripts/check-env-docs.sh` (PASS), markdownlint (PASS), domain-term check (PASS).
- [ ] **Step 4:** Commit `docs: CLAUDE.md files, architecture and extension guides`.

### Task 30: Agents and ship-issue skill

**Agent:** devops-engineer. **Port from:** `$KSM/.claude/agents/{go-api-developer,frontend-developer,devops-engineer,qa-engineer,code-reviewer}.md`, `$KSM/.claude/skills/ship-issue/`. **Files:** Create `.claude/agents/*.md`, `.claude/skills/ship-issue/SKILL.md`, `.claude/settings.json`.

- [ ] **Step 1:** Add `scripts/check-agents.sh`: each agent file has `name` and `description` frontmatter, its `tools` list contains no `mcp__mempalace__*`, and none of `git-provider`, `helm-values` exist. Run (FAIL).
- [ ] **Step 2:** Port and strip domain and mempalace references; point agents at `CLAUDE.md` files.
- [ ] **Step 3:** Run `scripts/check-agents.sh` and the domain-term check (PASS).
- [ ] **Step 4:** Commit `chore(claude): agents and ship-issue skill`.

### Task 31 (WS3 gate)

**Agent:** code-reviewer. Review WS3; run `npm run lint && npm test && npm run build`, `make test-e2e test-e2e-auth`, all `scripts/check-*.sh`. Fix or log; commit `chore: WS3 review fixes`.

---

## WS4. Observability and ops

### Task 32: Telemetry

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/telemetry/{telemetry.go,dbmetrics.go,logs.go}`, `middleware/{metrics,auth_metrics,otel}.go`, and tests (not `business_metrics.go`, `leader_metrics.go`). **Files:** Create `backend/internal/telemetry/*.go`, `middleware/{metrics,otel}.go`; Modify `config.go` (`OtelConfig`), `bootstrap.go`, `routes.go`.
**Produces:** `telemetry.Init(cfg config.OtelConfig) (*Telemetry, error)` (no-op when `OTEL_ENABLED=false`), `(*Telemetry).Shutdown(ctx) error`, `StartDBMetrics(db *sql.DB)`, `LogHandler`; `middleware.HTTPMetrics()` registered when `OTEL_ENABLED` or `METRICS_ENABLED`; `slog` bridge never wraps the existing default handler; shutdown hook from Task 11 now flushes telemetry.

- [ ] **Step 1:** Tests: disabled `Init` returns a usable no-op; enabled with an in-memory exporter records one span and one request counter for `GET /api/v1/items`; DB pool gauges are registered; setting up logging twice does not recurse (`TestSetupOTelLoggingNoDeadlock`); log records carry trace and span ID when the context has a span.
- [ ] **Step 2-4:** Run (FAIL), port, `go test ./internal/telemetry ./internal/api/... -race -count=3` (PASS).
- [ ] **Step 5:** Commit `feat(telemetry): OpenTelemetry traces, metrics, logs`.

### Task 33: Metrics, pprof, compose OTel stack

**Agent:** devops-engineer. **Port from:** `$KSM/config/otel/*`, `$KSM/docker-compose.otel.yml`, pprof/metrics server code in `bootstrap.go`. **Files:** Create `backend/api/servers.go`, `servers_test.go`, `config/otel/*`, `docker-compose.otel.yml`; Modify `Makefile` (`dev-otel`), `.env.example`, `docs/claude/operations.md`.
**Produces:** `startMetricsServer(cfg) *http.Server` (`METRICS_ENABLED`, `METRICS_ADDR` `:9090`, serves `/metrics`); `startPprofServer(cfg) *http.Server` (`PPROF_ENABLED`, `PPROF_ADDR` `127.0.0.1:6060`, `PPROF_MUTEX_FRACTION`, `PPROF_BLOCK_RATE`); both stopped by `shutdown`. `make dev-otel` brings up collector, Prometheus, Tempo, Grafana (Grafana on :3001).

- [ ] **Step 1:** Tests: both servers are nil/not listening when disabled; pprof default address is loopback; `/metrics` exposes `http_server_request_duration`-style series after one request; shutdown closes both.
- [ ] **Step 2-3:** Run (FAIL), implement.
- [ ] **Step 4:** `go test ./api -race`; `docker compose -f docker-compose.yml -f docker-compose.otel.yml config` validates.
- [ ] **Step 5:** Commit `feat(ops): metrics and pprof servers, OTel compose stack`.

### Task 34: Replica heartbeat and MySQL lease election

**Agent:** go-api-developer. **Port from:** `$KSM/backend/internal/replica/heartbeat.go` (heartbeat only); the lease is new code. **Files:** Create `backend/internal/replica/heartbeat.go`, `models/replica_heartbeat.go`, `database/replica_heartbeat_repository.go`, `backend/internal/leader/lease.go`, `lease_test.go`; migration `20260101000003` (`replica_heartbeats`, `leader_leases`); Modify `config.go`, `bootstrap.go`.
**Produces:** `replica.NewHeartbeat(repo models.ReplicaHeartbeatRepository, id string, interval time.Duration) *Heartbeat` with `Start()`, `Stop(timeout)`, `ID()`; `leader.NewLease(db *gorm.DB, name, holder string, cfg LeaseConfig) *Lease` with `LeaseConfig{Duration (15s), RenewDeadline (10s), RetryPeriod (2s) time.Duration}`; `(*Lease).Run(ctx, onStarted func(ctx context.Context), onStopped func())` — acquires the row `leader_leases(name PK, holder, expires_at)` with an atomic `UPDATE … WHERE expires_at < now() OR holder = ?` (or insert), renews every `RetryPeriod`, and calls `onStopped` when renewal fails past `RenewDeadline`, then campaigns again. `LEADER_ELECTION_ENABLED` default false: `leader.Group` starts at once. Heartbeat enabled by `REPLICA_HEARTBEAT_ENABLED` (default false).

- [ ] **Step 1:** Tests (SQLite for unit, MySQL for race): two `Lease`s on one database — exactly one `onStarted` fires; after the holder stops renewing, the other takes over within `Duration + RetryPeriod`; when the database becomes unreachable the holder calls `onStopped` before `Duration` has elapsed (Review Focus 3); a deposed holder campaigns again and can win after the new leader stops; `Heartbeat` upserts a row each interval and deletes it on `Stop`; `TestDatabaseLeaseTwoReplicas` under the integration tag with `-race -count=3`.
- [ ] **Step 2:** Run (FAIL).
- [ ] **Step 3:** Implement; leader workers start in `onStarted` and stop in `onStopped`.
- [ ] **Step 4:** `go test ./internal/leader ./internal/replica -race -count=3` and `make test-backend-all` (PASS).
- [ ] **Step 5:** Commit `feat(ops): replica heartbeat and MySQL lease leader election`.

### Task 35: Load tests and Make targets

**Agent:** devops-engineer. **Port from:** `$KSM/loadtest/` (`backend/k6-api.js`, `k6-stress.js`, `k6-websocket.js`, `lib`), Makefile targets `test-coverage`, `test-backend-repeat`, `seed`, `loadtest`; `scripts/seed-dev-data.sh`. **Files:** Create `loadtest/**`, `scripts/seed-dev-data.sh`; Modify `Makefile`, `backend/Makefile`.
**Produces:** `make loadtest` (k6 against Items and `/ws`), `make test-coverage` (80% gate over packages listed in `backend/coverage-packages.txt`: every package added or ported by this branch), `make test-backend-repeat` (`-race -count=$(REPEAT_COUNT)` on packages changed vs `$(REPEAT_BASE)`, default `origin/main`), `make seed` (creates sample Items through the API).

- [ ] **Step 1:** Add `scripts/check-make-targets.sh` asserting each target above exists and `make -n <target>` succeeds. Run (FAIL).
- [ ] **Step 2:** Port and adapt scripts and targets (Items endpoints only).
- [ ] **Step 3:** Run `scripts/check-make-targets.sh`, `make test-coverage` (PASS with the 80% rule), `make seed` against a running dev stack.
- [ ] **Step 4:** Commit `chore(ops): k6 load tests, coverage gate, repeat and seed targets`.

### Task 36: Final verification and clean-up

**Agent:** qa-engineer, then `code-reviewer` for the whole branch. **Files:** Delete `docs/superpowers/` (spec, plan, baseline) in the last commit; Modify spec section 11 first if findings were logged.

- [ ] **Step 1:** Run the full done criteria (spec section 10): `make test lint gosec test-coverage`, `make test-e2e test-e2e-auth`, `make dev` smoke (Items open with auth off; with `AUTH_ENABLED=true JWT_SECRET=… ADMIN_PASSWORD=…` login then Items), `scripts/check-*.sh`, `grep -c k8s.io backend/go.mod` prints 0.
- [ ] **Step 2:** `code-reviewer` reviews `git diff main...HEAD` as a whole; fix findings.
- [ ] **Step 3:** Copy the logged follow-ups into the PR description, delete `docs/superpowers/`, commit `chore: remove working docs before merge`.
- [ ] **Step 4:** Push and open the PR with `gh pr create` (title `feat: backport generic foundations from k8s-stack-manager`); the description lists the four workstreams, the Review Focus items, and the baseline comparison. Do not merge.

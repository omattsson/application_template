# Backport generic foundations from k8s-stack-manager into application_template

Date: 2026-11-08
Branch: `feat/backport-ksm-foundations` (single branch, single PR)
Status: revised after review, for approval

## 1. Purpose

`application_template` is the base for new projects that have a Go API and a React front end. `k8s-stack-manager` (KSM) started from it and has since gained many production-hardened parts that have nothing to do with Kubernetes. This work brings those generic parts back so a new project starts with them instead of rebuilding them.

Success means:

- A new project forked from the template starts with hardened backend foundations, optional auth, a themed and tested front end, CI, AI tooling, and observability.
- No Kubernetes, stack, Helm-deploy or other KSM domain concept appears in the template, and the template has no `k8s.io` dependency.
- The `Items` entity stays the reference example for adding a resource.
- Everything is built test-first and lands on one branch.

## 2. Decisions

| Decision | Choice |
| --- | --- |
| Areas in scope | Backend foundations; Auth, RBAC and audit; Frontend, CI and AI tooling; Observability and ops |
| Auth default | Off by default, opt-in with `AUTH_ENABLED=true` |
| Azure Table Storage backend | Removed. MySQL only (SQLite stays for unit tests) |
| Leader election | Optional, off by default. MySQL-based lease, no `client-go` (new code, not a straight backport) |
| Replica heartbeat | Optional, off by default |
| Existing `User` model (uint ID, unused) | Replaced by a new migration that drops and recreates `users` with a UUID string ID. The old migration is not edited |
| Helm chart | Not included. Docker and Compose are the deployment story |
| Dropped from scope | Sharded e2e and `release-drafter`. CI uses one e2e job |
| Kept in scope | OIDC, API keys, k6 load tests |
| `.github` Copilot files (`copilot-instructions.md`, `instructions/`, `agents/`) | Kept and updated (Azure removed, new modules added). `CLAUDE.md` is the canonical source and the Copilot files point to it |
| Method | Port and strip: copy the generic code from KSM, remove domain references, adapt tests |
| Workflow | One branch, one git worktree, TDD per task, specialist subagents |

## 3. Out of scope

Everything specific to KSM's domain: clusters and the cluster registry, `deployer`, Helm deploys, `helm` values merge, `gitprovider`, `hooks` and custom actions, `notifier` and notification channels, quotas, TTL and expiry, cleanup policies and scheduler, `podstream` (port-forward and exec), `k8s` client code and the Kubernetes Lease election, and the stack, definition, template, instance and chart models.

Also out of scope:

- The Helm chart and its tooling (`values.schema.json`, `tools/helmschema`, `helm-*` Make targets, `helm-release` workflow).
- Sharded e2e and `release-drafter`.
- Anything tied to the author's mempalace setup (`mempalace-init-all.sh`, mempalace MCP tools in agent definitions). Ported agents must work with only the standard tools.
- The untracked `PLAN.md`, `entities.json` and `mempalace.yaml` in the template root, which are left alone.

Anything useful found during the work that is not on the lists below goes into the follow-up section at the end of this document, not into the branch.

## 4. Baseline (measured 2026-11-08 on `main` at `5b2e7ce`)

- Backend: `go build`, `go vet` and `go test ./...` pass in every package.
- Frontend: 64 Vitest tests in 9 files pass; `tsc --noEmit` is clean.
- Backend coverage per package: handlers 89.2%, middleware 91.4%, routes 100%, websocket 85.7%, health 100%, utils 91.7%, config 76.5%, schema 71.9%, database 44.8% (Azure package 51.3%, removed).
- Not yet measured: Playwright e2e. Task 0 of the plan measures it before any change.
- Versions: Go directive 1.23.0 (toolchain 1.24.3), Dockerfile `golang:1.24.3`, `node:20-alpine`, `distroless-debian11`, root `nginx:alpine`. MUI 7, Vite 6, TypeScript 5, Vitest 4.
- CI: `.github/workflows/pull-request.yml` triggers only on pull requests to `release` and `dev`, which do not exist (the repo uses `main`), so it never runs today.

## 5. Workstreams

The workstreams run in order. Each ends with a green test run and a `code-reviewer` pass before the next one builds on it.

### WS1. Backend foundations

Cleanup:

- Delete `backend/internal/database/azure/`, `azure_repository_test.go`, `backend/api/azure_test.go`, Azurite in `docker-compose.yml` and the Makefile, and `table_test.go.bak` and `lint_result.txt`.
- Remove Azure from `backend/internal/config/config.go` and `config_test.go`, `backend/internal/database/repository.go`, `backend/api/main.go`, `main_test.go`, `main_integration_test.go`, `backend/.env.example`, `go.mod` and `go.sum`.
- Remove Azure from the Copilot files: `.github/copilot-instructions.md`, `.github/instructions/{go,scalability,security}.instructions.md`, `.github/agents/{go-api-developer,devops-engineer,qa-engineer}.md`.
- Move to the Go version in KSM's `go.mod`. Update `backend/Dockerfile` to the matching Go image and a minimal runtime image, add `.dockerignore` files, and run the container as non-root. Replace `golint` with `go vet` and `gosec`, and add `scripts/gosec.sh` and the `gosec` Make target here, not in WS4.

Added or changed:

- `internal/ctxutil`: `Detach(ctx, timeout)`, `WriteTimeout` (30 s), and the guard test `TestNoUnboundedDetach`.
- Handler helpers: `handlers/errors.go` (`mapError`, 499 for a cancelled request context, no raw message for a 500) and `handlers/detach.go`. `items.go` moves to these.
- `internal/leader`, group only: `Group` runs named leader-only workers and starts them when the process is leader. In this workstream the process is always leader. The election backend arrives in WS4.
- Public `GET /api/v1/ui-config`: returns the app title and `auth_enabled`. It holds no secret. The front end reads it at start (used by WS3).
- Config: `TRUSTED_PROXIES` parsing and use, `LOG_LEVEL`, `APP_DEBUG` driving the Gin mode, `ENABLE_SWAGGER`, `RATE_LIMIT` and `LOGIN_RATE_LIMIT`, MySQL session parameters in the DSN.
- Middleware: security headers, WebSocket token redaction in logs, request ID kept, body limit kept (413).
- Database: a base repository with lock retry, `ListPaged` with filter structs for Items, and a GORM logger that writes through `slog` and redacts values when `APP_DEBUG=false`. Cursor pagination is not ported; `ListPaged` with limit and offset is the only list pattern.
- Bootstrap: split `backend/api/main.go` into `main.go` plus `bootstrap.go` helpers, with graceful shutdown in KSM's proven order (HTTP servers, workers, in-flight work, hub, telemetry, database). Telemetry and election appear as hook points that are no-ops until WS4.
- WebSocket hub: scoped subscriptions, per-user sends, and the `Revocable` interface used by WS2. The existing Items events and tests keep working.

### WS2. Auth, RBAC and audit (off by default)

Backend, all behind `AUTH_ENABLED`:

- Models and migrations: a new migration drops and recreates `users` with a UUID string ID, then creates `refresh_tokens`, `api_keys` and `audit_logs`. The tables are created whether or not auth is enabled, so the flag never needs a migration. Existing migrations are not edited or renumbered.
- Local users with a first-run admin bootstrap, JWT access tokens, rotating refresh tokens, login failure throttling.
- `internal/sessionstore`: token blocklist and OIDC state, `memory` and `mysql` implementations.
- Roles `admin`, `devops`, `user`, with `RequireAdmin()` and `RequireDevOps()`.
- `CombinedAuth` middleware (JWT and API key), checking the blocklist and disabled users.
- Optional OIDC with PKCE (`internal/auth`) and API keys with their endpoints.
- Audit middleware and audit-log endpoints. Admin user-management endpoints.
- Refresh-token cleanup runs as a leader-only worker in the WS1 `leader.Group`.
- WebSocket revocation on logout or when a user is disabled.
- `auth_enabled` in `ui-config` follows `AUTH_ENABLED`.
- KSM's `handlers/auth.go` is 1,536 lines and mixes in stack-access rules. It is split into identity, sessions and OIDC, and only those parts are ported.

Secrets and startup rules:

- With `AUTH_ENABLED=true` the backend refuses to start unless `JWT_SECRET` is set and at least 16 characters.
- There is no default admin password. `ADMIN_PASSWORD` must be set, or the admin is not created and the log says so. `SELF_REGISTRATION` defaults to false.
- Dev values in `docker-compose.yml` and `.env.example` are clearly marked as development only, and the README says production must override them.
- CORS defaults and the `*` development mode are documented as development only.

Template behavior:

- `AUTH_ENABLED=false`: Items and WebSocket behave as today.
- `AUTH_ENABLED=true`: Items routes require a valid token (401 without, 403 for a role gate), and `/health`, `ui-config` and login stay open.

### WS3. Frontend, CI and AI tooling

Frontend:

- Task 1 is the dependency upgrade, done before anything else is built: Node 22.12 or later (CI, Dockerfile, README), MUI 9, Vite 8, TypeScript 6, Vitest 5, ESLint 10. The 64 existing tests and `tsc` are the regression gate. `frontend/Dockerfile` moves to the matching Node image and an unprivileged nginx runtime.
- MUI theme (`theme/`) and `ThemeContext`. Components: `ErrorBoundary`, `ConfirmDialog`, `EmptyState`, `LoadingState`, `NotFound`. Utilities and hooks: `apiError`, `useUnsavedChanges`.
- A `ui-config` loader. Auth pieces, active only when `auth_enabled` is true: `AuthContext`, `Login`, `ProtectedRoute`, `AuthCallback`, token refresh in the API client (`refresh.ts`), and an Admin Users page.
- ESLint config, a stricter `tsconfig`, accessibility e2e helpers (`@axe-core/playwright`), and e2e coverage for Items with auth off and the login, refresh and logout flow with auth on.

CI (`.github/workflows/`):

- `pull-request` rewritten: triggers on pull requests to `main`, current pinned action versions, Go version read from `backend/go.mod`, Node version matching the Dockerfile, `go vet`, `gosec`, backend tests, frontend lint, build and tests, markdownlint.
- New: `codeql`, `security-scan` (gosec, trivy on the built images, `.trivyignore` check), `docker-build`, and one `e2e` job that starts MySQL and the stack and runs Playwright in both auth modes. `dependabot.yml`. No `release-drafter`, no sharding.
- Markdownlint config (`.markdownlint.yml`, `.markdownlintignore`).

AI tooling and docs:

- Root, `backend/` and `frontend/` `CLAUDE.md`, and `docs/claude/*` reference docs (structure, auth and sessions, operations).
- `ARCHITECTURE.md`, `EXTENDING.md` and `RELEASING.md`.
- `.claude/agents` for the template: `go-api-developer`, `frontend-developer`, `devops-engineer`, `qa-engineer`, `code-reviewer`. The KSM-only agents `git-provider`, `helm-values` and any mempalace tool references are not ported. A `ship-issue` skill adapted to those agents.
- The existing `.github` Copilot files are updated as listed in the decisions table.
- A README section: how to start a new project from the template.

### WS4. Observability and ops

- `internal/telemetry`: OpenTelemetry traces, metrics and logs (`OTEL_ENABLED`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_TRACE_SAMPLE_RATE`), DB-pool metrics, and a `slog`-to-OTel bridge that never wraps the default handler. HTTP and auth metrics middleware. The OTel, Prometheus, JWT and OIDC modules compile in even when the features are off; only the runtime behavior is off by default.
- Optional Prometheus metrics server (`METRICS_ENABLED`, `METRICS_ADDR`) and pprof server (`PPROF_ENABLED`, `PPROF_ADDR`, bound to localhost by default).
- `docker-compose.otel.yml` and `config/otel` (collector, Prometheus, Tempo, Grafana).
- k6 load tests under `loadtest/` and the matching Make targets.
- Optional `internal/replica` (heartbeat row per replica) and the leader election backend for the WS1 `leader.Group`: a lease row in MySQL with an expiry, renewed by the holder and taken over by another replica after it lapses (`LEADER_ELECTION_ENABLED`, default false: the process is always leader). A replica that loses the lease stops its workers and campaigns again. This is new code and has its own tests, including a two-replica race test.
- Make targets: `test-coverage`, `test-backend-repeat` (`-race -count=3` on changed packages), `dev-otel`, `loadtest`, `seed`. (`gosec` is in WS1.)

## 6. Process

- Branch `feat/backport-ksm-foundations` in the worktree `.claude/worktrees/feat-backport-ksm-foundations`. `.claude/worktrees/` is added to `.gitignore`.
- Task 0 of the plan re-runs the baseline in section 4, including Playwright e2e, so any later failure can be told from an existing one.
- Commits are grouped per workstream and follow conventional commits, one commit or a few small ones per task, so a very large PR stays reviewable commit by commit.
- Each task is red, green, refactor. The subagent writes failing tests, then the minimal port, then the controller runs the verification commands and reads the output before accepting the task. Agents: `go-api-developer`, `frontend-developer`, `devops-engineer`, `qa-engineer`. Independent tasks run in parallel. `code-reviewer` reviews at the end of each workstream.
- A domain-term check (clusters, helm, kubeconfig, k8s, deployer, gitprovider, quota, and `stack`/`namespace` in a Kubernetes sense) runs after each port. Hits are removed, or listed in a reviewed allowlist file when the word is used in a generic sense (for example a call stack or a CSS stack).
- The spec file stays on the branch for review and is removed in the last commit before merge, matching KSM, which ignores `docs/superpowers/`.

## 7. Testing

Backend:

- Table-driven `testify` tests with `t.Parallel()` and `tt := tt`, following the template's conventions. Each ported package brings its relevant KSM tests, adapted. A test that only exercised domain code is replaced by a template-specific one.
- Unit tests run with `-short` and no database. MySQL integration tests (`TestDatabase*`) run under `make test-backend-all`, and the CI `e2e` job provides MySQL.
- Auth is tested in both modes: with the flag off, Items behaves as before; with it on, no token gives 401, a role gate gives 403, a revoked token is refused. Startup refuses a missing or short `JWT_SECRET`.
- Guard tests carry over, such as `TestNoUnboundedDetach`.
- New packages are run with `-race -count=3` before a workstream is accepted.
- Coverage rule: every package added or ported by this branch reaches at least 80%. The existing `database`, `config` and `schema` packages must not drop, and are raised toward 80% where the branch touches them. `make test-coverage` reports the total. The 80% gate does not apply to a package this branch does not touch.

Frontend: Vitest and Testing Library for each component, context and hook. Playwright e2e for Items with auth off, and for login, refresh and logout with auth on.

## 8. Error handling rules carried into the template

- A 500 never returns a raw error message; it returns `"Internal server error"`.
- A cancelled request context returns 499 with a debug log. A deadline exceeded stays 500.
- A write whose side effects follow the commit uses a detached context with a deadline and a cancel function the caller must call. A bare `context.WithoutCancel` is not allowed.
- A repository read on the request path uses the request context.

## 9. Risks

1. **Domain leakage into ported files.** Mitigation: the domain-term check, a compile check without the domain packages, and a check that `go.mod` has no `k8s.io` module.
2. **Auth code size and coupling.** Mitigation: split `auth.go`, port only identity, sessions and OIDC, and put the leader worker group in WS1 so auth does not wait for WS4.
3. **Migrations.** Mitigation: append new versions, never renumber or edit old ones, create auth tables unconditionally. The `users` replacement is safe because nothing uses the old table.
4. **Major dependency upgrades break the current front end.** Mitigation: the upgrade is its own first task, gated by the 64 tests and `tsc`, before any new code.
5. **New leader-lease code is not a proven port.** Mitigation: it is optional and off by default, it has a two-replica race test, and it is the last item in WS4 so it can be cut without affecting the rest.
6. **Template grows too heavy for small projects.** Mitigation: auth, telemetry, pprof, metrics and leader election are all off by default and documented as modules. Compile-time weight (OTel, OIDC, JWT modules) is accepted.
7. **One very large PR.** Mitigation: small commits per task, a workstream summary in the PR description, and `code-reviewer` findings logged per workstream.
8. **Scope creep.** Mitigation: out-of-scope findings go to the follow-up section.

## 10. Done criteria

- `make test`, `make lint` and `make gosec` pass, and the Playwright e2e suite passes in both auth modes in the CI `e2e` job.
- `make dev` starts. Items works open with auth off and gated with auth on.
- The domain-term check finds nothing outside the reviewed allowlist, and `go.mod` has no `k8s.io` module.
- The coverage rule in section 7 holds.
- Docs, `CLAUDE.md` files and the Copilot files match the code. The README explains how to start a new project and states that production must override the dev secrets.
- `code-reviewer` has reviewed each workstream and its findings are fixed or logged below.

## 11. Follow-ups (filled in during the work)

- Extract the shared parts into a Go module that both repositories import, so fixes in one reach the other.
- Back-port in the other direction: KSM could drop its copies once the template is stable, and could adopt the MySQL lease instead of the Kubernetes Lease where a single database is already required.

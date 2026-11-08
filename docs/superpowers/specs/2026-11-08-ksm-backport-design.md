# Backport generic foundations from k8s-stack-manager into application_template

Date: 2026-11-08
Branch: `feat/backport-ksm-foundations` (single branch, single PR)
Status: draft for review

## 1. Purpose

`application_template` is the base for new projects that have a Go API and a React front end. `k8s-stack-manager` (KSM) started from it and has since gained many production-hardened parts that have nothing to do with Kubernetes. This work brings those generic parts back so a new project starts with them instead of rebuilding them.

Success means:

- A new project forked from the template starts with hardened backend foundations, optional auth, a themed and tested front end, CI, AI tooling, and observability.
- No Kubernetes, stack, Helm-deploy or other KSM domain concept appears in the template.
- The `Items` entity stays the reference example for adding a resource.
- Everything is built test-first and lands on one branch.

## 2. Decisions

| Decision | Choice |
|---|---|
| Areas in scope | Backend foundations; Auth, RBAC and audit; Frontend, CI and AI tooling; Observability and ops |
| Auth default | Off by default, opt-in with `AUTH_ENABLED=true` |
| Azure Table Storage backend | Removed. MySQL only (SQLite stays for unit tests) |
| Leader election and replica heartbeat | Included as an optional module, off by default |
| Helm chart | Not included. Docker and Compose are the deployment story |
| Method | Port and strip: copy the generic code from KSM, remove domain references, adapt tests |
| Workflow | One branch, one git worktree, TDD per task, specialist subagents |

## 3. Out of scope

Everything specific to KSM's domain: clusters and the cluster registry, `deployer`, Helm deploys, `helm` values merge, `gitprovider`, `hooks` and custom actions, `notifier` and notification channels, quotas, TTL and expiry, cleanup policies and scheduler, `podstream` (port-forward and exec), `k8s` client code, and the stack, definition, template, instance and chart models. Also out of scope: the Helm chart and its tooling (`values.schema.json`, `tools/helmschema`, `helm-*` Make targets, `helm-release` workflow), and the untracked `PLAN.md`, `entities.json` and `mempalace.yaml` in the template root, which are left alone.

Anything useful found during the work that is not on the lists below goes into the follow-up section at the end of this document, not into the branch.

## 4. Workstreams

The workstreams run in order. Each ends with a green test run and a `code-reviewer` pass before the next one builds on it.

### WS1. Backend foundations

Cleanup:

- Delete `backend/internal/database/azure/`, `azure_repository_test.go`, `backend/api/azure_test.go`, Azurite in `docker-compose.yml` and Makefile targets, and Azure config. Remove Azure dependencies from `go.mod`.
- Delete `table_test.go.bak` and `lint_result.txt`.
- Move to the Go version in KSM's `go.mod`. Replace `golint` in CI and Makefile with `go vet` and `gosec`.

Added or changed:

- `internal/ctxutil`: `Detach(ctx, timeout)`, `WriteTimeout` (30 s), and the guard test `TestNoUnboundedDetach`.
- Handler helpers: `handlers/errors.go` (`mapError`, 499 for a cancelled request context, no raw message for a 500) and `handlers/detach.go`. `items.go` moves to these.
- Config: `TRUSTED_PROXIES` parsing and use, `LOG_LEVEL`, `APP_DEBUG` driving the Gin mode, `ENABLE_SWAGGER`, `RATE_LIMIT` and `LOGIN_RATE_LIMIT`, MySQL session parameters in the DSN.
- Middleware: security headers, WebSocket token redaction in logs, request ID kept, body limit kept (413).
- Database: a base repository with lock retry, `ListPaged` with filter structs for Items, a GORM logger that writes through `slog` and redacts values when `APP_DEBUG=false`. Cursor pagination is not ported; `ListPaged` with limit and offset is the only list pattern.
- Bootstrap: split `backend/api/main.go` into `main.go` plus `bootstrap.go` helpers, with graceful shutdown in KSM's proven order (HTTP servers, workers, in-flight work, hub, telemetry, database).
- WebSocket hub: scoped subscriptions, per-user sends, and the `Revocable` interface used by WS2. The existing Items events and tests keep working.

### WS2. Auth, RBAC and audit (off by default)

Backend, all behind `AUTH_ENABLED`:

- Models and migrations: `User`, `RefreshToken`, `APIKey`, `AuditLog`. The tables are created whether or not auth is enabled, so the flag never needs a migration. New migration versions come after the existing ones.
- Local users with a first-run admin bootstrap, JWT access tokens, rotating refresh tokens, login failure throttling.
- `internal/sessionstore`: token blocklist and OIDC state, `memory` and `mysql` implementations.
- Roles `admin`, `devops`, `user`, with `RequireAdmin()` and `RequireDevOps()`.
- `CombinedAuth` middleware (JWT and API key), checking the blocklist and disabled users.
- Optional OIDC with PKCE (`internal/auth`).
- Audit middleware and audit-log endpoints. Admin user-management and API-key endpoints.
- WebSocket revocation on logout or when a user is disabled.
- KSM's `handlers/auth.go` is 1,536 lines and mixes in stack-access rules. It is split into identity, sessions and OIDC, and only those parts are ported.

Template behavior:

- `AUTH_ENABLED=false`: Items and WebSocket behave as today.
- `AUTH_ENABLED=true`: Items routes require a valid token (401 without, 403 for a role gate), and `/health` and login stay open.

### WS3. Frontend, CI and AI tooling

Frontend:

- MUI theme (`theme/`) and `ThemeContext`. Components: `ErrorBoundary`, `ConfirmDialog`, `EmptyState`, `LoadingState`, `NotFound`. Utilities and hooks: `apiError`, `useUnsavedChanges`.
- Auth pieces, active only when the backend reports auth enabled: `AuthContext`, `Login`, `ProtectedRoute`, `AuthCallback`, token refresh in the API client (`refresh.ts`), and an Admin Users page.
- ESLint, a stricter `tsconfig`, accessibility e2e helpers, and e2e coverage for Items with auth off and the login, refresh and logout flow with auth on.
- Dependency upgrades the ported code needs (MUI, Vite, TypeScript, Vitest), done first and verified with the existing suite.

CI (`.github/workflows/`):

- `pull-request` modernized (current action versions, Go version read from `backend/go.mod`).
- New: `codeql`, `security-scan` (gosec, trivy, `.trivyignore` check), `docker-build`, sharded `e2e`, `release-drafter`, `dependabot.yml`. Markdownlint config.

AI tooling and docs:

- Root, `backend/` and `frontend/` `CLAUDE.md`, and `docs/claude/*` reference docs (structure, auth and sessions, operations).
- `ARCHITECTURE.md`, `EXTENDING.md` and `RELEASING.md`.
- `.claude/agents` for the template: `go-api-developer`, `frontend-developer`, `devops-engineer`, `qa-engineer`, `code-reviewer`. The KSM-only agents `git-provider` and `helm-values` are not ported. A `ship-issue` skill adapted to those agents.
- A README section: how to start a new project from the template.

### WS4. Observability and ops

- `internal/telemetry`: OpenTelemetry traces, metrics and logs (`OTEL_ENABLED`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_TRACE_SAMPLE_RATE`), DB-pool metrics, and a `slog`-to-OTel bridge that never wraps the default handler. HTTP and auth metrics middleware.
- Optional Prometheus metrics server (`METRICS_ENABLED`, `METRICS_ADDR`) and pprof server (`PPROF_ENABLED`, `PPROF_ADDR`, bound to localhost by default).
- `docker-compose.otel.yml` and `config/otel` (collector, Prometheus, Tempo, Grafana).
- k6 load tests under `loadtest/` and the matching Make targets.
- Optional `internal/replica` (heartbeat) and `internal/leader` (leader election and leader-only worker group). The Kubernetes Lease implementation stays behind an interface, and the default is "no election, single replica".
- Make targets: `gosec`, `test-coverage` (80% threshold), `test-backend-repeat` (`-race -count=3` on changed packages), `dev-otel`, `loadtest`, `seed`.

## 5. Process

- Branch `feat/backport-ksm-foundations` in the worktree `.claude/worktrees/feat-backport-ksm-foundations`. `.claude/worktrees/` is added to `.gitignore`.
- Commits are grouped per workstream and follow conventional commits.
- Each task is red, green, refactor. The subagent writes failing tests, then the minimal port, then the controller runs the verification commands and reads the output before accepting the task. Agents: `go-api-developer`, `frontend-developer`, `devops-engineer`, `qa-engineer`. Independent tasks run in parallel. `code-reviewer` reviews at the end of each workstream.
- A domain-term check (clusters, helm, stack, deployer, gitprovider, k8s, kubeconfig, namespace, quota) runs after each port. A hit in code, config or docs is removed or justified.

## 6. Testing

Backend:

- Table-driven `testify` tests with `t.Parallel()` and `tt := tt`, following the template's conventions. Each ported package brings its relevant KSM tests, adapted. A test that only exercised domain code is replaced by a template-specific one.
- Unit tests run with `-short` and no database. MySQL integration tests (`TestDatabase*`) run under `make test-backend-all`.
- Auth is tested in both modes: with the flag off, Items behaves as before; with it on, no token gives 401, a role gate gives 403, a revoked token is refused.
- Guard tests carry over, such as `TestNoUnboundedDetach`.
- New packages are run with `-race -count=3` before a workstream is accepted.

Frontend: Vitest and Testing Library for each component, context and hook. Playwright e2e for Items with auth off, and for login, refresh and logout with auth on.

## 7. Error handling rules carried into the template

- A 500 never returns a raw error message; it returns `"Internal server error"`.
- A cancelled request context returns 499 with a debug log. A deadline exceeded stays 500.
- A write whose side effects follow the commit uses a detached context with a deadline and a cancel function the caller must call. A bare `context.WithoutCancel` is not allowed.
- A repository read on the request path uses the request context.

## 8. Risks

1. **Domain leakage into ported files.** Mitigation: the domain-term check and a compile check without the domain packages after each port.
2. **Auth code size and coupling.** Mitigation: split `auth.go` and port only identity, sessions and OIDC.
3. **Migrations.** Mitigation: append new versions, never renumber, create auth tables unconditionally.
4. **Dependency upgrades break the current front end.** Mitigation: upgrade first, run the full suite, then build on top.
5. **Template grows too heavy for small projects.** Mitigation: auth, telemetry, pprof, metrics and leader election are all off by default and documented as modules.
6. **Scope creep.** Mitigation: out-of-scope findings go to the follow-up section.

## 9. Done criteria

- `make test`, `make lint` and `make gosec` pass, and the Playwright e2e suite passes in both auth modes.
- `make dev` starts. Items works open with auth off and gated with auth on.
- The domain-term check finds nothing unjustified.
- Docs and `CLAUDE.md` files match the code. The README explains how to start a new project.
- `code-reviewer` has reviewed each workstream and its findings are fixed or logged below.

## 10. Follow-ups (filled in during the work)

- Extract the shared parts into a Go module that both repositories import, so fixes in one reach the other.
- Back-port in the other direction: KSM could drop its copies once the template is stable.

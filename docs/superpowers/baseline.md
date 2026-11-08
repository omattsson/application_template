# Baseline before KSM backport

Recorded on branch `feat/backport-ksm-foundations` at commit `cd639ea` (no code changes). Use this to tell existing failures from regressions.

## Backend

Commands (in `backend/`): `go build ./...`, `go vet ./...`, `go test ./... -cover -short -count=1`. All pass.

| Package | Coverage |
|---|---|
| backend/api | 0.0% |
| backend/docs | 0.0% (no tests) |
| backend/internal/api/handlers | 89.2% |
| backend/internal/api/middleware | 91.4% |
| backend/internal/api/routes | 100.0% |
| backend/internal/config | 76.5% |
| backend/internal/database | 44.8% |
| backend/internal/database/azure | 51.3% |
| backend/internal/database/azure/testhelpers | 0.0% (no tests) |
| backend/internal/database/schema | 71.9% |
| backend/internal/health | 100.0% |
| backend/internal/models | 0.0% (tests exist, no statements covered) |
| backend/internal/test | 0.0% (no tests) |
| backend/internal/websocket | 85.7% |
| backend/pkg/dberrors | 0.0% (no tests) |
| backend/pkg/utils | 91.7% |

`backend/internal/api` has no test files.

## Frontend unit tests

`npm test` (Vitest): 9 test files, **64 tests passed**, matching the spec. `npx tsc --noEmit` is clean (no output).
The run prints a stack trace from an expected error-path test, but the test passes.

## Frontend e2e (Playwright, project `chromium`, 5 specs, 18 tests)

Run with `npx playwright test` in `frontend/`. Playwright started the Vite dev server itself. The backend was reachable at `localhost:8081`.

Run 1 (empty database, fresh MySQL): 16 passed, 2 failed.

| Spec | Tests | Result |
|---|---|---|
| health.spec.ts | 3 | pass |
| home.spec.ts | 3 | pass |
| navigation.spec.ts | 5 | pass |
| websocket.spec.ts | 3 | pass |
| items.spec.ts | 4 | 2 pass, 2 FAIL |

The 2 failing tests in `items.spec.ts` depend on seed data:
- `displays the items table with data from the API`
- `renders table column headers`

With an empty database the page shows "No items found." and renders no table, so `getByRole('table')` is never found. This is an existing failure, not a regression. After one item was created through the API (`POST /api/v1/items`), `items.spec.ts` passed 4 of 4. The specs need at least one item in the database.

Expected baseline for later tasks: with at least one item present, all 18 e2e tests pass. With an empty database, the two items tests fail.

## How the e2e stack was run (deviations from the brief)

- Playwright browsers were missing. `npx playwright install chromium` fixed that.
- `docker compose up -d db backend frontend` did not work as written:
  - With the default env, the backend image builds the `production` target. That target is the Go builder stage and has no CMD, so the container exits 0 right after starting. Compose then reports "dependency backend failed to start".
  - With `GO_ENV=development`, a stale exited container named `app-backend-development` from the `k8s-stack-manager` compose project (13 days old) conflicts with the container name. It was left untouched.
- Instead, the repo's own `make test-e2e` flow was used. MySQL ran in Docker (`GO_ENV=development docker compose up -d db`). The backend was built (`go build -o tmp/main ./api/main.go`) and run natively on `:8081` with `DB_HOST=localhost`. Playwright started Vite on `:3000`.
- Cleanup: the backend process was killed and `docker compose down -v` was run. No containers, volumes or listeners from this run remain. `git status` is clean.

# File structure

Where things are. Run `ls` when a file is missing here: the tree below lists the directories and the files that matter, not every test.

## Repository root

```text
.
├── CLAUDE.md                  # Instructions for AI assistants (this documentation set starts here)
├── ARCHITECTURE.md            # How the system fits together
├── EXTENDING.md               # Adding an API resource and a page
├── RELEASING.md               # Versions and releases
├── README.md, WIKI.md         # Human getting-started docs
├── Makefile                   # Every developer and CI command (make help is not defined: read the file)
├── docker-compose.yml         # db, backend, frontend; networks backend-net and frontend-net
├── docker-compose.otel.yml    # override: collector, Prometheus, Tempo, Grafana (make dev-otel)
├── config/otel/               # collector, Prometheus, Tempo and Grafana provisioning files
├── backend/                   # Go service
├── frontend/                  # React app
├── docs/claude/               # Reference docs that CLAUDE.md links to
├── scripts/                   # Check and test scripts (see below)
├── .claude/                   # Claude Code agents (agents/), the ship-issue skill (skills/), permissions (settings.json), specs of running work (work/, git-ignored)
└── .github/                   # Workflows, templates, Copilot instructions, agents
```

## Backend (`backend/`, module `backend`)

```text
backend/
├── api/
│   ├── main.go                # config -> telemetry -> database -> services -> HTTP server -> signal -> shutdown; Swagger header
│   └── bootstrap.go           # initTelemetry, initDatabase, buildServices, buildRouter, startHTTPServer, shutdown (ordered)
├── internal/
│   ├── api/
│   │   ├── routes/routes.go   # Setup(): every route, middleware order, auth wiring, rate limiters
│   │   ├── middleware/        # RequestID, RedactWSToken, Logger, Recovery, Compression, SecurityHeaders, CORS, MaxBodySize,
│   │   │                      # JWT + API key auth (CombinedAuth), roles, audit recorder, idempotency.go, /ws token helpers,
│   │   │                      # HTTPMetrics, SpanEnrichUser, auth_metrics.go (auth outcome counters)
│   │   └── handlers/          # items.go (reference CRUD), auth*.go, oidc.go, users.go, api_keys.go,
│   │                          # audit_logs.go, websocket*.go, ui_config.go, rate_limiter.go, errors.go,
│   │                          # login_failure.go, mock_repository.go and test_schemas.go (test support)
│   ├── auth/                  # OIDC provider client (discovery, PKCE, ID token validation); authtest/ fake IdP
│   ├── config/                # LoadConfig, AuthConfig, OIDCConfig, TrustedProxies
│   ├── ctxutil/               # Detach: a bounded context that a leaving client cannot cancel
│   ├── database/              # factory, migrations, schema/ (migrator), repositories (users, refresh tokens,
│   │                          # API keys, audit log, replica heartbeats, idempotency keys, outbox events), BaseRepository (lock retry), gorm logger
│   ├── health/                # Liveness and readiness checks
│   ├── inflight/              # Tracker for work that outlives its request
│   ├── leader/                # Worker and Group: leader-only background jobs; Lease: MySQL row lease election
│   ├── models/                # Item, User, RefreshToken, APIKey, AuditLog, ReplicaHeartbeat, IdempotencyKey, OutboxEvent, validation,
│   │                          # GenericRepository (optional change hook), RetryOnLock
│   ├── logutil/               # Limiter: one log line per interval for a repeating error
│   ├── outbox/                # Poller (delivers outbox_events to the hub, gap window), ItemHook, Metrics
│   ├── replica/               # ProcessID and Heartbeat (replica_heartbeats row per process)
│   ├── sessionstore/          # Token and session blocklist, OIDC state: mysql.go, memory.go, janitor.go
│   ├── telemetry/             # Init, Shutdown, LogHandler (slog bridge), StartDBMetrics; no-op when off
│   ├── websocket/             # Hub, Client, topics, revalidation, message types
│   └── test/                  # Small test helpers
├── pkg/
│   ├── dberrors/              # ErrNotFound, ErrDuplicateKey, ErrValidation, DatabaseError, duplicate-key detection
│   └── utils/                 # Small helpers
├── config/mysql/my.cnf        # MySQL server settings of the Compose database
├── config/.env.example        # Copy of ../.env.example with DB_HOST=localhost
├── docs/                      # Generated Swagger (docs.go, swagger.json, swagger.yaml): never edit by hand
├── .env.example               # Every variable the backend reads
├── Dockerfile                 # development (air), build-prod, production (alpine, uid 65532)
└── Makefile                   # build, test, test-coverage, docs, lint
```

The old `internal/api/routes.go` is a placeholder; all routes are in `internal/api/routes/routes.go`.

## Frontend (`frontend/`)

```text
frontend/
├── src/
│   ├── main.tsx, App.tsx      # Entry, providers (theme, UI config, auth, WebSocket), Layout
│   ├── routes.tsx             # Route table; ProtectedRoute per page
│   ├── api/                   # config.ts (same-origin /api), client.ts (services), session.ts, refresh.ts, errorSummary.ts
│   ├── context/               # AuthContext, UIConfigContext, WebSocketContext, ThemeContext
│   ├── hooks/                 # useWebSocket, useUnsavedChanges
│   ├── components/            # Layout, ProtectedRoute, ErrorBoundary, ConfirmDialog, EmptyState, LoadingState
│   ├── pages/                 # Home, Health, Items, Login, AuthCallback, Admin/Users, NotFound (one directory each)
│   ├── theme/                 # MUI theme: palette, typography, component overrides
│   ├── utils/                 # jwt, apiError, returnPath, navigation
│   └── test/                  # Test harness and setup
├── e2e/                       # Playwright specs, global-setup.ts, a11y.ts (axe), authEnv.ts, skipDecision.ts
├── nginx.conf                 # Production proxy: /api (one prefix stripped), /ws, SPA fallback
├── vite.config.ts             # Dev server on 3000 with the same proxy
├── playwright.config.ts
├── Dockerfile                 # build, development (Vite), production (nginx-unprivileged, uid 101, port 8080)
└── .nvmrc                     # Node major for CI and the image
```

## Scripts and CI

| File | Purpose | Command |
|---|---|---|
| `scripts/check-domain-terms.sh` (+ `.test.sh`, `domain-terms.allowlist`) | Keeps domain terms of the project this template came from out of the tree | `scripts/check-domain-terms.sh` |
| `scripts/check-env-docs.sh` | Environment variables of the code equal the example files and `operations.md` | `make check-env-docs` |
| `scripts/check-workflows.sh` | Workflow hygiene: pinned actions, permissions, timeouts, `main` trigger | `make check-workflows` |
| `scripts/check-agents.sh` | `.claude/agents/*.md` have frontmatter, no forbidden tools or agents, and `.claude/settings.json` allows nothing that publishes | `make check-agents` |
| `scripts/check-dockerfiles.sh` | Production stage non-root, pinned images, Go version equals `go.mod` | `scripts/check-dockerfiles.sh` |
| `scripts/check-trivyignore.sh` | Every `.trivyignore` entry has a reason and an expiry within 90 days | `scripts/check-trivyignore.sh` |
| `scripts/gosec.sh` | Pinned gosec run inside `backend/` | `make gosec` |
| `scripts/e2e.sh` | MySQL in Docker, backend, Playwright | `make test-e2e`, `make test-e2e-auth` |
| `scripts/check-make-targets.sh` | The ops make targets exist, are `.PHONY` and can be planned with `make -n` and print a recipe (also without k6) | `make check-make-targets` |
| `scripts/coverage-gate.sh` | Per-package coverage gate over `backend/coverage-packages.txt` | `make test-coverage` |
| `scripts/seed-dev-data.sh` | Creates sample Items through the API of a running backend | `make seed` |
| `loadtest/` | k6 scripts (`k6-api.js`, `k6-websocket.js`, `k6-stress.js`, `lib/`) | `make loadtest`, `make loadtest-stress` |

Each `check-*.sh` takes `--self-test` (except `check-domain-terms.sh`, which has `check-domain-terms.test.sh`).

Workflows in `.github/workflows/`: `pull-request.yml` (repository checks, backend, frontend, Markdown lint), `pr-title.yml` (conventional PR title), `docker-build.yml` (both production images, not pushed), `security-scan.yml` (Trivy), `codeql.yml`, `e2e.yml` (Playwright, three passes). Details: `RELEASING.md`.

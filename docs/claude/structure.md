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
├── backend/                   # Go service
├── frontend/                  # React app
├── docs/claude/               # Reference docs that CLAUDE.md links to
├── docs/superpowers/          # Design specs and plans of the template itself
├── scripts/                   # Check and test scripts (see below)
├── .claude/                   # Claude Code agents (agents/), the ship-issue skill (skills/), permissions (settings.json)
└── .github/                   # Workflows, templates, Copilot instructions, agents
```

## Backend (`backend/`, module `backend`)

```text
backend/
├── api/
│   ├── main.go                # config -> database -> services -> HTTP server -> signal -> shutdown; Swagger header
│   └── bootstrap.go           # initDatabase, buildServices, buildRouter, startHTTPServer, shutdown (ordered)
├── internal/
│   ├── api/
│   │   ├── routes/routes.go   # Setup(): every route, middleware order, auth wiring, rate limiters
│   │   ├── middleware/        # RequestID, RedactWSToken, Logger, Recovery, SecurityHeaders, CORS, MaxBodySize,
│   │   │                      # JWT + API key auth (CombinedAuth), roles, audit recorder, /ws token helpers
│   │   └── handlers/          # items.go (reference CRUD), auth*.go, oidc.go, users.go, api_keys.go,
│   │                          # audit_logs.go, websocket*.go, ui_config.go, rate_limiter.go, errors.go,
│   │                          # login_failure.go, mock_repository.go and test_schemas.go (test support)
│   ├── auth/                  # OIDC provider client (discovery, PKCE, ID token validation); authtest/ fake IdP
│   ├── config/                # LoadConfig, AuthConfig, OIDCConfig, TrustedProxies
│   ├── ctxutil/               # Detach: a bounded context that a leaving client cannot cancel
│   ├── database/              # factory, migrations, schema/ (migrator), repositories (users, refresh tokens,
│   │                          # API keys, audit log), BaseRepository (lock retry), gorm logger
│   ├── health/                # Liveness and readiness checks
│   ├── inflight/              # Tracker for work that outlives its request
│   ├── leader/                # Worker and Group: leader-only background jobs
│   ├── models/                # Item, User, RefreshToken, APIKey, AuditLog, validation, GenericRepository, RetryOnLock
│   ├── sessionstore/          # Token and session blocklist, OIDC state: mysql.go, memory.go, janitor.go
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

Each `check-*.sh` takes `--self-test` (except `check-domain-terms.sh`, which has `check-domain-terms.test.sh`).

Workflows in `.github/workflows/`: `pull-request.yml` (repository checks, backend, frontend, Markdown lint), `pr-title.yml` (conventional PR title), `docker-build.yml` (both production images, not pushed), `security-scan.yml` (Trivy), `codeql.yml`, `e2e.yml` (Playwright, three passes). Details: `RELEASING.md`.

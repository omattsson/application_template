# Copilot Instructions

The project rules are in [`CLAUDE.md`](../CLAUDE.md) at the repository root. Read it first; this file only summarizes it so that the two never disagree. When they differ, `CLAUDE.md` wins.

| Area | Read |
|---|---|
| Architecture, commands, patterns, security and scalability rules | `CLAUDE.md` |
| Backend rules (handlers, repositories, errors, tests, migrations) | `backend/CLAUDE.md` |
| Frontend rules (components, API client, accessibility, e2e) | `frontend/CLAUDE.md` |
| Adding an API resource and a page, optional modules | `EXTENDING.md` |
| How the system fits together | `ARCHITECTURE.md` |
| File trees, authentication and sessions, every environment variable | `docs/claude/` |
| CI checks, branch protection, releases | `RELEASING.md` |

Topic files for Copilot are in `.github/instructions/`; each points to the same documents.

## What this is

A full-stack template: a Go (Gin) backend, a React (TypeScript, Vite, MUI) frontend and a MySQL database (GORM), run by Docker Compose. Items is the example resource. Authentication is optional and off by default (`AUTH_ENABLED=false`).

## Rules that are easy to break

- Never return a raw error message for a 500: the body is `"Internal server error"`. A cancelled request is 499, an oversized body is 413 (`respondBindError` in every JSON handler), a bad body is 400.
- Every repository method takes `context.Context` first. A write whose effects follow the commit uses a detached context with a deadline (`detachedCtx`, `ctxutil.Detach`).
- Never edit a database migration that exists: add a new one.
- A new list endpoint is paginated (`ListPaged`, `limit` and `offset`) with an index that covers its filter and order.
- Every environment variable is documented in `backend/.env.example`, `backend/config/.env.example` and `docs/claude/operations.md` (`make check-env-docs`).
- Do not hard-code secrets. `JWT_SECRET` and `ADMIN_PASSWORD` have no default. Database passwords in Compose and the e2e credentials are DEV ONLY.
- The web UI always calls its own origin (`/api`, `/ws`), never the backend port.
- Do not write the vocabulary of other projects into this template (`scripts/check-domain-terms.sh`).

## Commands

| Task | Command |
|---|---|
| Full stack (Docker) | `make dev` |
| Backend unit tests | `cd backend && go test ./... -short` |
| All backend tests (starts MySQL) | `make test-backend-all` |
| Frontend tests | `cd frontend && npm test` |
| E2E tests, auth off and on | `make test-e2e`, `make test-e2e-auth` |
| Swagger docs | `make docs` |
| Lint and checks | `make lint`, `make lint-docs`, `make gosec` |

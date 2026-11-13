---
applyTo: "**/*.go"
---

# Go Backend Instructions

The backend rules are in `backend/CLAUDE.md`; the repository-wide rules are in `CLAUDE.md`. This file lists what Copilot must not get wrong.

- The module is `backend` (`backend/go.mod`, Go 1.27). Private packages are in `backend/internal/`, shared ones in `backend/pkg/`.
- `models.Repository` (Items) and the other repository interfaces take `context.Context` as the first parameter of every method. Pass `c.Request.Context()` for reads; use `detachedCtx(c)` (and `defer cancel()`) for a write whose effects follow the commit.
- Handlers: bind with `ShouldBindJSON` and answer a failure with `respondBindError` (413 or 400); map repository errors with `mapError(err, "Entity")` (404, 409, 400, 499, 500). A 500 never exposes the cause: `"Internal server error"`.
- Repositories built on `BaseRepository` write through `Write` or `Tx` (deadlock retry). Never interpolate user input into SQL.
- Never use a bare `context.WithoutCancel`: use `ctxutil.Detach`.
- Tests: `testify/assert`, table-driven, `t.Parallel()` on the parent and the subtests, `tt := tt`, `MockRepository` for Items, schemas in `handlers/test_schemas.go`. Integration tests carry the `integration` build tag and run one package at a time.
- Swagger annotations on every handler (`@Summary`, `@Tags`, `@Param`, `@Success`, `@Failure`, `@Router`); regenerate with `make docs`.
- Order struct fields for memory alignment (8-byte fields first).
- Configuration is read from environment variables in `internal/config`; a new variable goes into `backend/.env.example`, `backend/config/.env.example` and `docs/claude/operations.md`.

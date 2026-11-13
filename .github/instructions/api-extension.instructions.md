---
applyTo: "**"
---

# API Extension Instructions

The step-by-step guide for adding an API resource and its page is `EXTENDING.md` ("Adding a new API resource"); the short version is in `CLAUDE.md` ("Adding a New API Resource"). Items is the reference implementation: copy its shape.

Order of work for a resource:

1. Model, `Validate()` and the repository interface in `internal/models/`.
2. GORM repository in `internal/database/` (embed `BaseRepository`; write through `Write` or `Tx`; map errors with `repoError`).
3. A new migration in `internal/database/migrations.go` (never edit an existing one).
4. Handler in `internal/api/handlers/` (`respondBindError`, `mapError`, `detachedCtx`, Swagger annotations).
5. Routes in `internal/api/routes/routes.go` on the `api` group, wiring in `routes.Deps` and `api/bootstrap.go`.
6. Audit entity type in `internal/api/middleware/audit.go` (`KnownAuditEntityTypes`, `entityTypeBySegment`).
7. `make docs`, then tests (handler, repository, routes) and the frontend service, page, route, navigation, tests and e2e spec.

Rules:

- A 500 answers `"Internal server error"`. A cancelled request is 499; an oversized body is 413.
- Validate path IDs and return 400 for a bad one. Success returns the entity; an error returns `gin.H{"error": "message"}`.
- List endpoints are paginated with `ListPaged`.

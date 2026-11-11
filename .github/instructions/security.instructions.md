---
applyTo: "**"
---

# Security Instructions

## Input Validation
All user input must be validated before processing. This project validates at two layers:

1. **Handler layer** — Gin's `ShouldBindJSON` rejects malformed payloads, then handlers check required fields explicitly before passing to the repository:
   ```go
   if err := c.ShouldBindJSON(&item); err != nil {
       c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
       return
   }
   if item.Name == "" {
       c.JSON(http.StatusBadRequest, gin.H{"error": "Name is required"})
       return
   }
   ```
2. **Model layer** — models implement the `Validator` interface (`internal/models/validation.go`). The repository calls `Validate()` automatically before Create/Update. Add validation for every new model.

## Error Handling — Never Leak Internals
The `handleDBError()` function in `handlers/items.go` maps database errors to safe HTTP responses. For `500` errors, always return `"Internal server error"` — never expose raw error messages, stack traces, or database details to clients:
```go
return http.StatusInternalServerError, "Internal server error"  // correct
return http.StatusInternalServerError, err.Error()              // only for non-DB errors
```

## CORS Configuration
CORS middleware is in `internal/api/middleware/middleware.go`. Default development config allows all origins (`*`). For production, restrict `CORS_ALLOWED_ORIGINS` in `docker-compose.yml` to specific domains. Never deploy with `Access-Control-Allow-Origin: *` in production.

## Database Credentials
- All secrets are passed via environment variables, loaded by `config.LoadConfig()` with `.env` fallback (godotenv).
- Never hardcode credentials in source files. Use `docker-compose.yml` env var substitution with defaults only for local dev.

## SQL Injection Prevention
GORM parameterizes all queries automatically. When writing raw SQL in migrations (`internal/database/migrations.go`), never interpolate user input — use parameterized queries exclusively.

## Soft Delete
The `Base` model uses `DeletedAt *time.Time` with a GORM index. Deletes are soft by default — rows are marked with a timestamp, not removed. GORM automatically filters soft-deleted records from queries. If implementing hard delete, be explicit and document why.

## Rate Limiting
The `RateLimiter` in `handlers/rate_limiter.go` provides per-IP request throttling using a sliding window. Apply it to route groups that need protection:
```go
rateLimiter := handlers.NewRateLimiter(100, time.Minute)
items.Use(rateLimiter.RateLimit())
```

## Recovery Middleware
The `Recovery()` middleware in `internal/api/middleware/middleware.go` catches panics and returns a generic 500. This is applied globally in `routes.go`. Never remove it — it prevents unhandled panics from crashing the server and potentially leaking stack traces.

## Graceful Shutdown
`api/main.go` waits for `SIGINT`/`SIGTERM`. Then `shutdown` in `api/bootstrap.go` stops the services in this order: HTTP servers, leader worker group, in-flight work, WebSocket hub, telemetry, rate limiters, database.

Shutdown timeouts:
- The HTTP step gets `SERVER_SHUTDOWN_TIMEOUT` (default 30s; 5s when set to 0).
- The audit log drain (auth on), leader, in-flight and telemetry steps each get their own budget of the same length. These budgets are detached, so a hung request does not starve the later steps.
- A server that does not finish in time is closed.
- The worst case is about 5 x `SERVER_SHUTDOWN_TIMEOUT` (HTTP, audit log drain, leader, in-flight and telemetry steps each use their full budget), so the deploy grace period must be longer: `docker-compose.yml` sets `stop_grace_period: 170s` for the default 30s, and any other deployment's stop grace period follows the same rule; raise it with the timeout.

Background work:
- Periodic work is a `leader.Worker`. Pass it to `leader.NewGroup` in `buildServices` before `workers.Start`, because `Group.Add` takes effect only at the next term.
- Work that outlives its request or worker run starts with `inflight.Tracker.Go(fn)`. Handlers get the tracker as `routerDeps.InFlight`, and workers get it in `buildServices`. `Go` returns `false` once shutdown has begun; the caller then does the work inline or drops it. Never add work to the tracker after `Wait`.
- Shutdown waits for tracked work before the database closes. A panic in tracked work is recovered and logged.
- All background work must respect cancellation.

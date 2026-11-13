---
applyTo: "**"
---

# Security Instructions

The security rules are in `CLAUDE.md` ("Security Rules") and `docs/claude/auth-sessions.md`. Summary:

- **Validation**: bind with `ShouldBindJSON` (answer a failure with `respondBindError`), check required fields in the handler, and implement `Validate()` on the model; the repository calls it before a write.
- **Errors**: a 500 answers `"Internal server error"`, never a raw error, stack trace or database detail (`mapError`).
- **SQL**: GORM parameterizes queries. Never interpolate user input into raw SQL, also not in migrations.
- **Secrets**: only through environment variables. `JWT_SECRET` and `ADMIN_PASSWORD` have no default. The Compose database password and the e2e credentials are DEV ONLY.
- **Authentication** is optional and off by default. With `AUTH_ENABLED=false` the API is open: never expose it to people you do not trust. With it on, the access token lives in memory in the browser and the refresh token is an httpOnly cookie. `SECURE_COOKIES` defaults to `true`; `false` is for plain-HTTP development on a host other than `localhost`.
- **CORS**: `CORS_ALLOWED_ORIGINS=*` (the default) is for development only. The bundled UI is same-origin and needs no entry. The CORS middleware must never allow `X-Requested-With` or send `Access-Control-Allow-Credentials`.
- **Rate limiting**: per client IP (`RATE_LIMIT`, `LOGIN_RATE_LIMIT`). The client IP comes from `X-Forwarded-For` only for peers in `TRUSTED_PROXIES`; never trust a whole network.
- **Never remove** the `SecurityHeaders`, `Recovery` or `RedactWSToken` middleware. `/ws` carries the token as `?token=`: a proxy in front must not log its query (the bundled nginx does not access-log `/ws`; its `error_log` can still show the request line).
- **Swagger** (`ENABLE_SWAGGER=true`) stays open with auth on: keep it off in production.
- **Audit**: with auth on, every successful mutating request of an authenticated user is recorded; `/auth/*` is not.
- **Hard deletes**: `Base.DeletedAt` is a plain field, not `gorm.DeletedAt`; `Delete` removes the row.

## Graceful shutdown

`api/main.go` waits for `SIGINT` or `SIGTERM`; `shutdown` in `api/bootstrap.go` stops the services in this order: HTTP server, audit recorder drain (auth on), leader worker group, in-flight work, WebSocket hub, telemetry, rate limiters, session store (auth on), database. Every step runs even when an earlier one failed.

- The HTTP step gets `SERVER_SHUTDOWN_TIMEOUT` (default 30s; 5s when set to 0). The audit, leader, in-flight and telemetry steps each get their own detached budget of the same length, so a hung request does not starve them. The worst case is about five times `SERVER_SHUTDOWN_TIMEOUT`: the stop grace period must be longer (`docker-compose.yml`: `stop_grace_period: 170s` for the default).
- Periodic work is a `leader.Worker` passed to `leader.NewGroup` before `workers.Start`. Work that outlives its request starts with `inflight.Tracker.Go` (it returns `false` once shutdown has begun). All background work respects cancellation.

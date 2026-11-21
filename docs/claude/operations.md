# Operations

How to run, configure and stop the backend. Read it before you change configuration, the shutdown order, the Compose file or anything that runs on more than one replica. The auth behaviour is in `docs/claude/auth-sessions.md`.

## Requirements

- MySQL 8.0.17 or later. Migration `20260101000002` gives `session_entries.entry_key` the collation `utf8mb4_0900_bin`, which MySQL has from 8.0.17. `docker-compose.yml` runs `mysql:8.0`.
- The backend runs the migrations at every start (`database.NewRepository`). Replicas that start together serialize on the MySQL advisory lock `schema_migrations.<database>` (`GET_LOCK`). A replica that cannot get it waits up to 5 minutes (300 s), then the start fails with "timed out waiting for the migration lock". A migration that hangs therefore holds the other replicas for up to 5 minutes. Integration tests run one package at a time (`-p 1`) for the same reason: the packages share one database.
- Sort indexes: `GET /api/v1/items?sort=` and `GET /api/v1/users?sort=` order by a whitelisted column, then by `id` in the same direction. Migration `20261115000001` adds the single-column indexes that serve them: `idx_items_name`, `idx_items_price`, `idx_items_created_at`, `idx_items_updated_at`, `idx_users_email` and `idx_users_created_at`. The other keys are covered already: `items.id` (primary key), `users.username` (`idx_users_username`) and `users.role` (`idx_users_role`). InnoDB appends the primary key to each secondary index, so `(col)` serves `ORDER BY col, id` in both directions. The migration creates an index only when it is missing and its `Down` drops only these six; no data is rewritten.
- Upgraders: migration `20260101000001` drops the old `users` table (the unused one with a numeric ID and no `password_hash` column) without copying its rows, then creates `users`, `refresh_tokens`, `api_keys` and `audit_logs`. The tables exist whether or not `AUTH_ENABLED` is true.
- Compose is a development setup. It publishes MySQL on host port `MYSQL_PORT` (3306) with the default credentials `MYSQL_PASSWORD=apppass` and `MYSQL_ROOT_PASSWORD=rootpass`. These are dev-only values: set your own, or do not publish the port, anywhere else.

## Configuration reference

`internal/config/config.go` and `oidc.go` read every variable below. `backend/.env.example` has the same list with comments, and `scripts/check-env-docs.sh` (`make check-env-docs`, part of `make lint`) fails when a variable of the code is missing here or in the example files, or when the example documents one that no code reads. Add a variable to the code, `backend/.env.example`, `backend/config/.env.example` and this table together.

An unset or empty variable has its default. A duration is a Go duration such as `15m` or `12h`. With `AUTH_ENABLED=true` a set but invalid boolean or duration of the auth and OIDC settings stops the start; with auth off they are read leniently (the default on an invalid value) because nothing uses them. `RATE_LIMIT`, `LOGIN_RATE_LIMIT` and `APP_DEBUG` are strict in both modes.

### Application

| Variable | Default | Notes |
|---|---|---|
| `ENV_FILE` | `.env` | Dotenv file to load; a missing file is fine. A variable already in the process environment wins. |
| `APP_NAME` | `backend-api` | Required non-empty. |
| `GO_ENV` | `development` | Required non-empty; nothing branches on it. Compose uses the same name for the image target. |
| `APP_TITLE` | `Application Template` | Product name in `GET /api/v1/ui-config`. |
| `APP_DEBUG` | `true` | Gin debug mode and SQL values in the log. Set `false` in production (`make prod*` does). A boolean (`true`, `false`, `1`, `0`, `t`, `f`, surrounding spaces ignored); any other value, such as `off` or `no`, stops the start instead of falling back to `true`. |
| `GIN_MODE` | not set | When set, wins over `APP_DEBUG`. |

### Authentication and sessions

| Variable | Default | Notes |
|---|---|---|
| `AUTH_ENABLED` | `false` | Strict boolean. With `false` the API is open and the auth, API key, user and audit log routes answer 404. |
| `JWT_SECRET` | none | Required with auth on, 16 or more characters (surrounding whitespace does not count). Never logged. |
| `ADMIN_USERNAME` | `admin` | The bootstrap admin, a service account (see `auth-sessions.md`). Use a non-default name. |
| `ADMIN_PASSWORD` | none | Empty: no admin is created. With auth on a set value needs 8 or more characters and at most 72 bytes, or the start fails. Used once at creation. |
| `SELF_REGISTRATION` | `false` | Anyone can create a local account with the role `user`. |
| `SECURE_COOKIES` | `true` | `__Host-` cookie names and the `Secure` attribute. Plain HTTP on a host other than `localhost` needs `false`. Strict boolean with auth on. |
| `JWT_EXPIRATION` | `24h` | Token outside a refresh session. |
| `ACCESS_TOKEN_EXPIRATION` | `15m` | Must be shorter than `SESSION_IDLE_TIMEOUT`. |
| `SESSION_IDLE_TIMEOUT` | `30m` | At most `SESSION_MAX_LIFETIME`. |
| `SESSION_MAX_LIFETIME` | `12h` | Absolute session lifetime. |
| `REFRESH_TOKEN_EXPIRATION` | `168h` | Capped by the session deadline, so at most 12 h with the defaults. |
| `REFRESH_REUSE_GRACE` | `30s` | `0` to `5m`; `0` turns the grace window off. |
| `SESSION_STORE` | `mysql` | `mysql` or `memory`. `memory` is for one replica in development. |

### OpenID Connect

Needs `AUTH_ENABLED=true`; with auth off the OIDC routes answer 404 and the settings are not validated.

| Variable | Default | Notes |
|---|---|---|
| `OIDC_ENABLED` | `false` | |
| `OIDC_ISSUER` | none | Required with OIDC on. https, no credentials, query or fragment. |
| `OIDC_CLIENT_ID` | none | Required with OIDC on. |
| `OIDC_CLIENT_SECRET` | none | Required with OIDC on. Never logged. |
| `OIDC_REDIRECT_URL` | none | Required with OIDC on. https, or http for a loopback host. Behind the bundled nginx or the Vite dev server it is `<origin>/api/api/v1/auth/oidc/callback` (both strip one `/api`). |
| `OIDC_SCOPES` | `openid,profile,email` | Must include `openid`. |
| `OIDC_ROLE_CLAIM` | `roles` | Top-level claim, a string or a list of strings; at most 200 values. |
| `OIDC_ADMIN_ROLES` | empty | Exact, case-sensitive names. Empty: nobody is an admin through OIDC. |
| `OIDC_DEVOPS_ROLES` | empty | Same for the role `devops`; an admin role wins. |
| `OIDC_LOCAL_AUTH` | `false` | `true`: every local user may still use the password login. |
| `OIDC_STATE_TTL` | `5m` | `30s` to `1h`. |
| `OIDC_FRONTEND_PATH` | `/auth/callback` | Fixed web UI path after the callback. |

### Database

| Variable | Default | Notes |
|---|---|---|
| `DB_TYPE` | `mysql` | `sqlite` only in unit tests; the production factory always builds MySQL. |
| `DB_HOST` | `localhost` | `db` inside Compose. |
| `DB_PORT` | `3306` | |
| `DB_USER` | `root` | Compose passes `appuser`. |
| `DB_PASSWORD` | empty | Compose passes `apppass`: a dev-only value. |
| `DB_NAME` | `app` | |
| `DB_MAX_OPEN_CONNS` | `25` | |
| `DB_MAX_IDLE_CONNS` | `5` | Compose passes 25. |
| `DB_CONN_MAX_LIFETIME` | `5m` | |

The DSN sets `sql_mode`, `time_zone` (UTC) and `foreign_key_checks` as driver parameters (`config.MySQLSessionParams`), so every pooled connection has them. Never set them with a `SET` statement after connecting.

### Server

| Variable | Default | Notes |
|---|---|---|
| `SERVER_HOST` | empty | All interfaces. |
| `SERVER_PORT` | `8081` | The images, the proxies and the health checks expect 8081. |
| `SERVER_READ_TIMEOUT` | `10s` | Compose passes `15s`. |
| `SERVER_WRITE_TIMEOUT` | `0s` | Off by default: a stalled client can then hold a REST connection open. A positive value is safe for WebSockets: `net/http` clears the connection deadlines when the upgrade hijacks the connection (`TestServerWriteTimeoutDoesNotEndAWebSocket`), and the socket write pump sets its own per-write deadline. Compose therefore passes `15s` for slow-client protection. |
| `SERVER_IDLE_TIMEOUT` | `30s` | |
| `SERVER_SHUTDOWN_TIMEOUT` | `30s` | Budget of each shutdown step; `0` means 5 s. See "Shutdown". |
| `TRUSTED_PROXIES` | empty | See "Client IP and proxies". |
| `RATE_LIMIT` | `100` | Requests per minute per client IP on `/api/v1`; must be positive. |
| `LOGIN_RATE_LIMIT` | `10` | Attempts per minute per client IP on each of login, register, OIDC login, OIDC callback and the password change (`PUT /auth/me/password`); must be positive. |
| `WS_MAX_CONNECTIONS_PER_USER` | `16` | Most `/ws` sockets that one user, and one API key, may have open at once (per replica: the hub is per replica); must be positive. The socket over the limit is closed with code 1008 `too many connections` right after the upgrade; closing a socket frees its slot. Sockets without an identity (`AUTH_ENABLED=false`) are not counted. One line per minute at Warn tells who was refused. A user with many tabs and devices may need a higher value. |
| `ENABLE_SWAGGER` | `false` | Swagger UI at `/swagger/index.html`. |

### Logging and CORS

| Variable | Default | Notes |
|---|---|---|
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` (or `warning`), `error`; invalid stops the start. Applies to the default `slog` logger, which writes text to stderr. |
| `LOG_FILE` | empty | Read but not used: logs go to stderr. |
| `CORS_ALLOWED_ORIGINS` | `*` | `*` or a comma-separated list. `*` is for development only. It also gates the WebSocket `Origin` header. The other `CORS_*` names in old files are not read: methods and headers are fixed in `middleware.CORS`. |

CORS always exposes `X-Total-Count` to browser scripts: `GET /api/v1/items` sends the number of items that match the filter (ignoring `limit` and `offset`) in that header and keeps a plain array as the body. With `IDEMPOTENCY_ENABLED=true` the same `Access-Control-Expose-Headers` value also lists `Idempotency-Replayed` and `Retry-After`.

### Telemetry

Off by default. With both switches off nothing is created (no exporter, goroutine or handler) and requests do no extra work. Details: "Telemetry" below.

| Variable | Default | Notes |
|---|---|---|
| `OTEL_ENABLED` | `false` | Strict boolean. Traces, metrics and logs go to the OTLP collector over gRPC, **without transport security**: use a collector on a trusted network. Spans carry the user's ID, username and role. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | `host:port`, or `http://host:port` (the scheme is dropped). `https://`, a path, a query or credentials stop the start. A host other than `localhost`, `127.0.0.1` or `::1` logs a startup warning: the export is plaintext gRPC (the usual case is a collector service on a private Compose network). Read only with `OTEL_ENABLED=true`. |
| `OTEL_SERVICE_NAME` | `backend-api` | `service.name` of every signal. Must not be empty. Read only with `OTEL_ENABLED=true`. |
| `OTEL_TRACE_SAMPLE_RATE` | `1` | New traces recorded, 0 to 1; a sampled incoming `traceparent` is always kept. A value outside 0 to 1 stops the start with `OTEL_ENABLED=true`. |
| `METRICS_ENABLED` | `false` | Strict boolean. Records `http.server.request.*`, builds the Prometheus handler (also without `OTEL_ENABLED`) and serves it as `GET /metrics` on `METRICS_ADDR`. |
| `METRICS_ADDR` | `:9090` | `host:port` of the metrics server, a port of its own (never the API port). **No authentication**: keep it on a private network. Read and validated only with `METRICS_ENABLED=true`. |

### Profiling (pprof)

Off by default. With `PPROF_ENABLED=false` no server and no profile rate is set. A heap or goroutine dump exposes secrets (tokens, keys, request data), so the defaults are loopback only.

| Variable | Default | Notes |
|---|---|---|
| `PPROF_ENABLED` | `false` | Strict boolean. Serves `net/http/pprof` (`/debug/pprof/`) on `PPROF_ADDR`, a port of its own. |
| `PPROF_ADDR` | `127.0.0.1:6060` | `host:port`. With pprof on, an address that is not loopback (`localhost`, `127.x.x.x`, `::1`) stops the start unless `PPROF_ALLOW_NON_LOOPBACK=true`. `:6060` and `0.0.0.0:6060` are not loopback. |
| `PPROF_ALLOW_NON_LOOPBACK` | `false` | Strict boolean. The explicit opt-in for a non-loopback `PPROF_ADDR` (for example in a container whose port the host publishes on `127.0.0.1`). The backend then logs a Warn at every start. |
| `PPROF_MUTEX_FRACTION` | `0` | Whole number of 0 or more: `runtime.SetMutexProfileFraction` (on average 1 in N contention events). `0` is off. |
| `PPROF_BLOCK_RATE` | `0` | Whole number of 0 or more, nanoseconds: `runtime.SetBlockProfileRate`. `0` is off; `1` records every blocking event (slow). |

The three `PPROF_*` values besides `PPROF_ENABLED` are read only with pprof on; an invalid one stops the start then.

Compose does not pass the `PPROF_*` variables to the backend, so a `PPROF_ENABLED=true` in the root `.env` has no effect. Profile a backend that runs on the host (set them in `backend/.env` or the environment, then `go tool pprof http://127.0.0.1:6060/debug/pprof/heap`), or give the container an override file of your own, for example `docker-compose.override.yml` (Compose reads it by itself; do not commit it):

```yaml
services:
  backend:
    environment:
      - PPROF_ENABLED=true
      - PPROF_ADDR=:6060
      - PPROF_ALLOW_NON_LOOPBACK=true
    ports:
      - "127.0.0.1:6060:6060"
```

### Replicas

Off by default: a single process behaves as before. Details: "Replicas" below.

| Variable | Default | Notes |
|---|---|---|
| `LEADER_ELECTION_ENABLED` | `false` | Strict boolean. With `true` the leader workers run only on the replica that holds the MySQL lease row `leader-workers` (`leader_leases`). With `false` every process starts them at once. |
| `REPLICA_HEARTBEAT_ENABLED` | `false` | Strict boolean. With `true` every process upserts its row in `replica_heartbeats` every 30 s and deletes it on a clean shutdown; the leader deletes rows older than 2 minutes. |

The lease timing is fixed in code (`leader.DefaultLeaseConfig`: 15 s lease, 10 s renew deadline, 2 s retry period), not configurable.

### Idempotency keys

Off by default: with the switch off no middleware, repository or worker exists, no query touches the table and the `Idempotency-Key` header is ignored. Details: "Idempotency keys" below.

| Variable | Default | Notes |
|---|---|---|
| `IDEMPOTENCY_ENABLED` | `false` | Strict boolean. With `true` a `POST` with an `Idempotency-Key` header runs once and a repeat replays the stored response. |
| `IDEMPOTENCY_TTL` | `24h` | How long a key and its stored response live. Read leniently while the module is off; with it on it must parse, be positive and not be shorter than `IDEMPOTENCY_LOCK_TIMEOUT`. |
| `IDEMPOTENCY_LOCK_TIMEOUT` | `60s` | How long a request in progress blocks a retry (409 with `Retry-After`) before the next request with the same key and the same request may take the key over. Set it above the longest `POST`. Read leniently while the module is off; with it on it must parse and be positive. |

### Outbox

Off by default: with the switch off no table is read or written, there is no poller and no janitor, and the Items handler broadcasts after the commit as before. Details: "Outbox" below.

| Variable | Default | Notes |
|---|---|---|
| `OUTBOX_ENABLED` | `false` | Strict boolean. With `true` the Items events are written to `outbox_events` in the transaction of the change, and every replica polls the table and delivers them to its own sockets. |
| `OUTBOX_POLL_INTERVAL` | `500ms` | How often each replica reads the new rows. Read leniently while the module is off; with it on it must parse and be positive. |
| `OUTBOX_RETENTION` | `1h` | How long a row is kept before the `outbox-cleanup` leader worker deletes it. Read leniently while the module is off; with it on it must parse, be at least 1 minute and at least 10 times `OUTBOX_POLL_INTERVAL`. |

### Background jobs

Off by default: with the switch off no table is read or written, there is no pool, no janitor, no route (404) and no health check, and no metric exists. Details: "Background jobs" below.

| Variable | Default | Notes |
|---|---|---|
| `JOBS_ENABLED` | `false` | Strict boolean. With `true` the backend builds the job repository, registry, queue, pool and the `jobs-cleanup` leader worker, adds the informational `jobs` health check and serves `GET /api/v1/jobs`. The other `JOBS_*` values are read leniently while the module is off and strictly while it is on (an unusable value stops the start). |
| `JOBS_WORKERS` | `4` | Goroutines that run jobs, per replica, 1 to 64. |
| `JOBS_POLL_INTERVAL` | `1s` | How often an idle pool claims (with up to 20% jitter), at least 100 ms. A busy pool claims again as soon as a worker is free. |
| `JOBS_VISIBILITY_TIMEOUT` | `5m` | The lease of a running job, at least 10 s. The pool extends it every third of this time while a handler runs. A job whose lease passed (its replica crashed or hung) is claimed again. |
| `JOBS_MAX_ATTEMPTS` | `5` | Claims after which a failing job is dead, 1 to 50. `jobs.WithMaxAttempts` sets it for one job. |
| `JOBS_BACKOFF_BASE` | `10s` | The delay before the second attempt, at least 1 s; it doubles with every attempt. |
| `JOBS_BACKOFF_MAX` | `1h` | The longest delay between two attempts, at least `JOBS_BACKOFF_BASE`. A hard bound: the jitter only shortens a delay, never lengthens it. |
| `JOBS_RETENTION` | `24h` | How long a succeeded job stays before `jobs-cleanup` deletes it, at least 1 minute. |
| `JOBS_DEAD_RETENTION` | `168h` | How long a dead job stays, at least `JOBS_RETENTION`. |
| `JOBS_STALE_AFTER` | `5m` | The `jobs` health check is DEGRADED when the oldest due job has waited longer than this, at least 10 s. |

### Mail and password reset

Off by default: with both switches off no mailer, readiness check, token repository, worker or route exists, and no query touches the table. Details: "Mail and password reset" below and `auth-sessions.md`.

| Variable | Default | Notes |
|---|---|---|
| `MAIL_ENABLED` | `false` | Strict boolean. With `true` the backend builds the SMTP mailer and adds the informational `mail` readiness check (a failure shows as `DEGRADED`, never as 503). It adds no route by itself: the password reset is its user. |
| `SMTP_HOST` | none | The SMTP server: a host name or IP address, without scheme or port. Required with `MAIL_ENABLED=true`. |
| `SMTP_PORT` | `587` | The SMTP port (465 with `SMTP_TLS=tls`). Read only with `MAIL_ENABLED=true`. |
| `SMTP_TLS` | `starttls` | `starttls` (the connection must upgrade, else the send fails), `tls` (implicit TLS) or `none` (clear text: a local mail catcher only; refused together with `SMTP_USERNAME`). |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | none | SMTP authentication (PLAIN, LOGIN or the best the server offers, over TLS only). Both or neither. The password is never logged. |
| `MAIL_FROM` | none | The sender address, optionally with a display name. Required with `MAIL_ENABLED=true`. |
| `SMTP_TIMEOUT` | `10s` | Connect, TLS and the whole send of one mail. Positive, at most 5m. Read leniently while the module is off. |
| `PASSWORD_RESET_ENABLED` | `false` | Strict boolean. With `true` the two routes `POST /api/v1/auth/password-reset` and `POST /api/v1/auth/password-reset/confirm` exist and `/ui-config` has `password_reset_enabled: true`. Needs `AUTH_ENABLED=true`, `MAIL_ENABLED=true` and the password login (`OIDC_ENABLED=false` or `OIDC_LOCAL_AUTH=true`); otherwise the start fails. |
| `PASSWORD_RESET_URL` | none | The base URL of the web UI in the link: `<PASSWORD_RESET_URL>/reset-password#token=...`. An `http` or `https` URL without a query, a fragment or user info; at most 512 characters. Required with `PASSWORD_RESET_ENABLED=true`. |
| `PASSWORD_RESET_TOKEN_TTL` | `1h` | How long a link works. From 5m to 24h. Read leniently while the module is off. |
| `PASSWORD_RESET_MIN_INTERVAL` | `60s` | The least time between two mails to one account (a request inside it sends nothing, and answers the same). Zero to 24h. Read leniently while the module is off. |

### Response compression

On by default: the backend gzips compressible responses for clients that ask for it. Details: "Response compression" below.

| Variable | Default | Notes |
|---|---|---|
| `COMPRESSION_ENABLED` | `true` | Strict boolean. With `false` no middleware is mounted and every response is unchanged. |
| `COMPRESSION_MIN_SIZE` | `1024` | The smallest response body in bytes (uncompressed) that is compressed. Read leniently while the module is off; with it on it must be a positive integer of at most 1048576 (1 MiB): the middleware holds up to that many bytes of a response back to decide. |

### Compose and Makefile variables

These are read by `docker-compose.yml`, the Makefile or the scripts, not by the backend.

| Variable | Default | Notes |
|---|---|---|
| `GO_ENV`, `NODE_ENV` | `production` | Image targets (`development` or `production`). `make dev*` sets development, `make prod*` production. |
| `BACKEND_PORT`, `FRONTEND_PORT` | 8081, 3000 | Host ports (`make prod*`: 8080 and 80). |
| `FRONTEND_CONTAINER_PORT` | `8080` | The unprivileged nginx of the production image. `make dev*` sets `3000` for the Vite dev server (set it too when running `NODE_ENV=development docker compose` by hand). |
| `MYSQL_PORT`, `MYSQL_DATABASE`, `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_ROOT_PASSWORD` | 3306, `app`, `appuser`, `apppass`, `rootpass` | Dev-only credentials. The database port is published on `127.0.0.1` only (`127.0.0.1:${MYSQL_PORT}:3306`): a backend or test run on the Compose host connects to `127.0.0.1:${MYSQL_PORT}`, the backend container uses `db:3306` on `backend-net`. |
| `ALLOW_DEFAULT_CREDENTIALS` | unset | Read by `make prod`, `make prod-backend` and `make prod-frontend` (`scripts/check-prod-credentials.sh`): they refuse to start while `MYSQL_PASSWORD` or `MYSQL_ROOT_PASSWORD` is unset or still the placeholder (`apppass`, `rootpass`), looking at the environment, the `.env` file and the defaults as Compose resolves them. `ALLOW_DEFAULT_CREDENTIALS=1` overrides it with a warning (a throwaway local check of the production images). A database volume that already exists keeps the password it was created with. |
| `GRAFANA_ADMIN_PASSWORD` | `devonly-change-me` | Grafana admin password of `docker-compose.otel.yml`. A dev-only default: set your own. Read at the first start of an empty `grafana_data` volume. |
| `MAILPIT_UI_PORT` | `8025` | Host port of the Mailpit web UI of `docker-compose.mail.yml` (`make dev-mail`), published on `127.0.0.1` only. |
| `GRAFANA_PORT`, `PROMETHEUS_PORT`, `TEMPO_PORT`, `OTLP_GRPC_PORT`, `OTLP_HTTP_PORT` | 3001, 9090, 3200, 4317, 4318 | Host ports of the observability setup, all published on `127.0.0.1` only. |
| `FRONTEND_PROXY_IP`, `FRONTEND_NET_SUBNET` | `172.30.80.10`, `172.30.80.0/24` | Fixed proxy address on `frontend-net` and its subnet; change both together. |
| `VITE_API_URL` | `http://localhost:8081` | Target of the Vite dev proxy; Compose sets `http://backend:8081`. |

`docker-compose.yml` also passes thirty-three backend variables documented above, with an empty default that keeps the backend's own default (their parsers treat an empty value as unset): `LEADER_ELECTION_ENABLED` (`false`), `REPLICA_HEARTBEAT_ENABLED` (`false`), `WS_MAX_CONNECTIONS_PER_USER` (`16`), `IDEMPOTENCY_ENABLED` (`false`), `IDEMPOTENCY_TTL` (`24h`), `IDEMPOTENCY_LOCK_TIMEOUT` (`60s`), `OUTBOX_ENABLED` (`false`), `OUTBOX_POLL_INTERVAL` (`500ms`), `OUTBOX_RETENTION` (`1h`), the ten job variables (`JOBS_ENABLED`, `JOBS_WORKERS`, `JOBS_POLL_INTERVAL`, `JOBS_VISIBILITY_TIMEOUT`, `JOBS_MAX_ATTEMPTS`, `JOBS_BACKOFF_BASE`, `JOBS_BACKOFF_MAX`, `JOBS_RETENTION`, `JOBS_DEAD_RETENTION`, `JOBS_STALE_AFTER`; off by default), `COMPRESSION_ENABLED` (`true`), `COMPRESSION_MIN_SIZE` (`1024`) and the twelve mail and password reset variables (`MAIL_ENABLED`, `SMTP_HOST`, `SMTP_PORT`, `SMTP_TLS`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `MAIL_FROM`, `SMTP_TIMEOUT`, `PASSWORD_RESET_ENABLED`, `PASSWORD_RESET_URL`, `PASSWORD_RESET_TOKEN_TTL`, `PASSWORD_RESET_MIN_INTERVAL`; all off or unset by default). It does not pass the `PPROF_*` variables: profiling needs an override file of your own (see "Profiling (pprof)"), and `docker-compose.otel.yml` sets the telemetry and metrics variables. The backend healthcheck (`/health/live`, every 30 s) has `start_period: 120s`, so a cold development compile (`go run` with an empty module cache) is not counted as failing; the frontend waits for it (`depends_on: service_healthy`).

`docker-compose.yml` also passes `APP_ENV`, `PORT`, `API_VERSION`, `API_PREFIX`, `CORS_ALLOWED_METHODS`, `CORS_ALLOWED_HEADERS` and `CORS_MAX_AGE` to the backend. The backend reads none of them (it reads `GO_ENV`, `SERVER_PORT` and `CORS_ALLOWED_ORIGINS`), so they have no effect. Set the listening port with `SERVER_PORT`, never `PORT`. The development frontend does not read a `PORT` either: the Vite server is fixed to port 3000.

Two checkouts, or another project, collide on the fixed host ports 3000, 8081 and 3306 and on the subnet `172.30.80.0/24`. Change the ports with the variables above and the subnet with `FRONTEND_NET_SUBNET` and `FRONTEND_PROXY_IP`. `make test-e2e` uses its own Compose project and MySQL on host port 13306 (`E2E_MYSQL_PORT`), and the backend and the Vite server on the ports `E2E_BACKEND_PORT` and `E2E_FRONTEND_PORT` (8081 and 3000): a port in use is an error.

## Client IP and proxies

- The per-IP rate limits and the audit entries use `c.ClientIP()`. Only a peer in `TRUSTED_PROXIES` may set the client IP with `X-Forwarded-For` or `X-Real-IP`; otherwise it is the TCP peer. Behind a proxy with an empty list every request has the proxy's address, so all users share one rate-limit bucket.
- Compose sets `TRUSTED_PROXIES=${TRUSTED_PROXIES:-${FRONTEND_PROXY_IP:-172.30.80.10}}`. The `:-` fallback means an empty value in the environment is replaced by the proxy address: the list cannot be emptied through Compose. Edit the Compose file to run with no trusted proxy.
- Trust proxy addresses, never a whole network: on some Docker setups the network gateway is the source of traffic from the host, and a trusted gateway lets a client pick its IP.
- `SecurityHeaders` sends `Strict-Transport-Security` when the connection is TLS or the first `X-Forwarded-Proto` value is `https`. The backend reads that header from any client, trusted proxy or not, so a client that talks to the backend directly can make its own response carry HSTS (self-affecting only). Terminate TLS at a proxy and let it set the header.
- The bundled nginx sends `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: strict-origin-when-cross-origin` and `Permissions-Policy: camera=(), microphone=(), geolocation=()` on every answer of the SPA (`add_header ... always`, same values as `SecurityHeaders`). They are set at server level, and nginx drops server-level `add_header` lines in a location that has its own, so no location may add one (`nginxConf.test.ts` checks it). On `/api/` and `/ws` nginx hides the backend's copies (`proxy_hide_header`), so an answer carries each header once. There is no `Content-Security-Policy` yet: it needs its own review (inline styles, the `/ws` socket). A proxy of your own in front needs the same headers, plus HSTS.
- The bundled nginx sets `Cache-Control` too, from a `map $uri` and one server-level `add_header` (no location has its own `add_header`, so the security headers above stay): `no-cache` for `index.html`, the SPA fallback and every other non-hashed file, `public, max-age=31536000, immutable` for the content-hashed `/assets/` files, nothing on `/api/` and `/ws` (the backend decides). Without it a browser can keep an old `index.html` after a redeploy; it asks for hashed files that are gone and, with `nosniff`, refuses the fallback `index.html` served for the missing script: a blank page. A missing `/assets/` file is a real 404. Error answers (404, 502, 504) carry the security headers but no cache header. A proxy or CDN of your own in front must not override these.
- The bundled nginx (`frontend/nginx.conf`) and the Vite dev server both strip one `/api` prefix: the browser calls `/api/api/v1/...` and the backend sees `/api/v1/...`. The IdP redirect URI therefore is `<origin>/api/api/v1/auth/oidc/callback`.
- `/ws` carries the access token as `?token=` (a browser WebSocket cannot send headers). The backend removes it before its own logging (`RedactWSToken`), and the bundled nginx has `access_log off` for `/ws`. Any other proxy in front must not log the query of `/ws` either. The bundled nginx also sets `error_log /dev/stderr crit` for `/ws`: an upstream error would otherwise write the request line, token included, so only crit and worse is logged there, and upstream failures stay visible through the `/api/` location. A proxy of your own needs the same for `/ws`. The token is a short-lived access token (15 minutes by default); the sockets close when it expires.

## Rate limiting

A sliding window per client IP, in memory, per replica. `/api/v1` gets `RATE_LIMIT`; `POST /auth/login`, `POST /auth/register`, `GET /auth/oidc/login`, `GET /auth/oidc/callback` and `PUT /auth/me/password` each get their own `LOGIN_RATE_LIMIT` budget on top, and with `PASSWORD_RESET_ENABLED=true` so do `POST /auth/password-reset` and `POST /auth/password-reset/confirm` (one budget each). `POST /auth/refresh` has no dedicated limiter: only `RATE_LIMIT` applies, because several tabs and page loads refresh often. `/ws` uses `RATE_LIMIT` with auth on and no limiter with auth off (connections are long-lived). `/health/*` is not limited. A limited request gets 429 `rate limit exceeded`.

The username login throttle is separate and in memory per replica: 5 failed logins for one username in 15 minutes lock that username for 15 minutes (429 with `Retry-After`). With N replicas an attacker gets up to N times 5 guesses. The lock also stops the owner, which is a denial of service on the bootstrap admin: set a non-default `ADMIN_USERNAME` and a strong `ADMIN_PASSWORD`. The lock disappears at the next restart of the replica. Details: `auth-sessions.md`.

## Mail and password reset

Both modules are optional and off by default. `MAIL_ENABLED=true` builds the SMTP mailer (`internal/mail`: one connection per mail, no pool, `Auto-Submitted: auto-generated`, plain text, the recipient and subject checked for header injection) and registers the readiness check `mail`. `PASSWORD_RESET_ENABLED=true` adds the token repository, the two public routes, the `password-reset-cleanup` leader worker (hourly, batches of 1000, at most 100 batches per run; deletes tokens that expired or were used more than 24 h ago) and two rate limit budgets. The flow, the token and the security rules are in `auth-sessions.md` ("Password reset").

- **Readiness.** With `MAIL_ENABLED=true` the backend registers the `mail` check as **informational** (`HealthChecker.AddInformationalCheck`): an unreachable SMTP server shows in `/health/ready` as `"mail": {"status": "DEGRADED", "message": "mail server unavailable"}`, but the overall status stays `UP` and the answer stays 200, so a mail outage never takes a replica out of a load balancer (the API works without mail; a reset request still answers 202 and sends nothing). The database check and every other `AddCheck` check still gate readiness (503). The probe (`SMTPMailer.Ping`) does what a send does before the message: connect, EHLO, STARTTLS (per `SMTP_TLS`), **authentication when `SMTP_USERNAME` is set**, quit; it sends no message. It runs at most once per 30 s per replica (the result, error included, is cached), is bounded by 5 s, is shared by concurrent callers and runs on a detached context, so readiness never waits on the SMTP server and a flood of probes never reaches it. Cost: one SMTP login per replica every 30 s while the endpoint is polled (about 2 per minute per replica); a provider that rate-limits or locks on repeated logins needs a longer-lived account or fewer replicas probing, and nothing in the cache can lower it below that (there is no variable for it). The public answer names no host or error (the fixed text `mail server unavailable`); the detail goes to the log (one Warn line per interval, the rest at Debug). An older balancer rule that treated any non-`UP` check as down needs `status`, not `checks`.
- **Logs of a failed send.** A failed mail never logs the recipient address, the token or the server's answer (many servers echo the address, and go-mail's `SendError` prints it as "affected recipient(s)"). The line `Failed to send a password reset mail` holds the user id, the request id and the classification of `mail.Describe`: `kind` (`timeout`, `canceled`, `invalid_message`, `smtp` or `other`), `stage` (`connect`, `send` or `close`) and, for a refused delivery, the go-mail `reason`, the SMTP code `smtp_code` and `temporary`. One Error line per interval, the rest at Debug. The health probe's failure line (Warn) is the place for connect, TLS and authentication detail: the probe carries no recipient.
- **Sending.** The request answers 202 first; the lookup, the token insert and the mail run in tracked work (`inflight.Tracker.Go`) that shutdown waits for (step 4). At most 16 run at once (a request over the bound is dropped without a trace in the answer), and each mail is bounded by `SMTP_TIMEOUT` plus a margin. There is no retry: the user asks again after `PASSWORD_RESET_MIN_INTERVAL`. A mail failure is logged once per interval at Error (the rest at Debug) with the user id, never the address or the token.
- **Replicas.** The minimum interval and the single use of a token are decided by MySQL (a conditional `UPDATE`, a locking read in one transaction; every time is read from the database clock), so they hold across replicas. The rate limiters are per replica, like every other.
- **Local development.** `docker-compose.mail.yml` adds a Mailpit catcher (SMTP on the Compose network at `mailpit:1025`, web UI on `127.0.0.1:${MAILPIT_UI_PORT:-8025}` only) and sets the mail variables for the backend. Use it with `make dev-mail`. `make prod*` never starts it and nothing in the production files points at it. The catcher accepts any mail without authentication and shows every message to whoever reaches its web UI: never expose it.

## Shutdown

`SIGINT` or `SIGTERM` starts `shutdown` (`backend/api/bootstrap.go`). Every step runs even when an earlier step failed:

0. Metrics and pprof servers (only the ones that run): stopped first, together within a fixed 2 s (`auxShutdownTimeout`), and closed when they are still busy (a pprof profile can run for 30 s). Prometheus sees the target go away at the start of the drain.
1. HTTP server: no new requests, wait for the open ones (own budget, `SERVER_SHUTDOWN_TIMEOUT`, which starts after step 0 ends). A server that does not finish is closed.
2. Audit recorder (auth on): no new entries, wait for the writes of finished requests. Own budget.
3. Leader worker group: stop the background workers. Own budget.
4. In-flight work (`inflight.Tracker`): work that outlives its request, such as the lookup and the mail of a password reset request (a send in progress is bounded by `SMTP_TIMEOUT` plus 5 s). Own budget.
5. Job pool (`JOBS_ENABLED=true`): stop claiming at once, give the running jobs a fixed 5 s to finish, then cancel their contexts and release the unfinished ones (back to `pending`, the attempt not counted) within a fixed 2 s more (`jobsStopTimeout`, 7 s in all, on a detached context). It runs after the HTTP server and the in-flight work (no request enqueues or starts anything new) and before the outbox poller (an event that a running job writes is still delivered), the hub (a job may send to it) and the database close (a job writes to it). The workers belong to the pool, not to `inflight.Tracker` (whose wait runs before this step): they are stopped by their own step. A handler that ignores its cancel is logged, its result is dropped by the fence and the step goes on.
6. Outbox poller (`OUTBOX_ENABLED=true`): stop the poll loop, within a fixed 2 s (`outboxStopTimeout`). It runs after the HTTP server, the in-flight work and the job pool (nothing new is written by this process) and before the hub and the database close (it reads the table and sends to the hub).
7. WebSocket hub: close the client connections and end the revalidation loop.
8. Telemetry flush (`Telemetry.Shutdown`: spans, metrics and logs that are still buffered, sent or dropped within the budget). Own budget.
9. Rate limiters: stop their cleanup goroutines.
10. Session store (auth on): stop its cleanup goroutine, after everything that reads it.
11. Replica heartbeat (`REPLICA_HEARTBEAT_ENABLED=true`): stop it and delete this process's row, within a fixed 2 s (`heartbeatStopTimeout`).
12. Database: close the pool last.

With `LEADER_ELECTION_ENABLED=true` step 3 first stops the workers (the group can no longer start), then ends the election, which releases the lease (`expires_at` in the past where this process is the holder) so the next replica takes over at its next attempt; all of it within the step's budget. When a worker does not stop within the budget (it ignores its context), the election is NOT ended and the lease is NOT released: this process keeps renewing it while the database is open, so no other replica starts while that worker runs. After step 12 closes the database the renewals fail, and the lease expires 15 s after the last one, after the process has exited. When the workers stopped but the election itself does not end within the rest of the budget, the lease expires on its own after 15 s. The leader step never waits longer than its budget, so the worst case below is unchanged.

Steps 1, 2, 3, 4 and 8 each get their own budget of the length that was left on the shutdown context when shutdown began (at least 100 ms), detached from it, so a hung request cannot starve a later step and the 2 s of step 0 do not shorten the HTTP drain. That is five budgeted steps, so the worst case is about five times `SERVER_SHUTDOWN_TIMEOUT` plus the 2 s of step 0, the 7 s of step 5, the 2 s of step 6 and the 2 s of step 11 (163 s with the default 30 s). The stop grace period of the orchestrator must be longer: `docker-compose.yml` sets `stop_grace_period: 170s` for the default 30 s, which still covers it with a margin of 7 s (the job pool added 7 s to the former 156 s; a deployment that raises `SERVER_SHUTDOWN_TIMEOUT` must raise the grace period by five times the increase). Raise both together. Work that outlives its request starts with `inflight.Tracker.Go`; a periodic job is a `leader.Worker`.

The API-key last-used update runs in a goroutine that shutdown does not wait for.

## Telemetry

`telemetry.Init` (`internal/telemetry`) runs in `main` before the database, the hub and the router, so they see the providers. It installs them as the global OpenTelemetry providers.

- `OTEL_ENABLED=true`: spans (batched), metrics (every 60 s) and `slog` records go to the collector. The sampler keeps the decision of an incoming `traceparent` and applies `OTEL_TRACE_SAMPLE_RATE` to new traces. The `slog` bridge adds the trace and span ID of the context to a record that is logged with a context (`slog.InfoContext`); the local stderr output is unchanged and still follows `LOG_LEVEL`, which also filters what goes to the collector. An unreachable collector does not stop the start: the data is dropped and the SDK reports the export errors.
- `METRICS_ENABLED=true` or `OTEL_ENABLED=true`: `middleware.HTTPMetrics` records `http.server.request.duration` (seconds) and `http.server.request.total` with `http.request.method` (the nine standard methods; any other value is `_OTHER`), `http.route` (the route pattern; `<unmatched>` for an unknown path) and `http.response.status_code`. `otelgin` only traces: its own request metrics are switched off, so `http.server.request.duration` has one bucket layout. `METRICS_ENABLED=true` also builds `Telemetry.MetricsHandler`, a Prometheus text handler over its own registry (OTel instruments plus the Go runtime and process collectors). `main` serves it on `METRICS_ADDR` (`startMetricsServer`, `backend/api/servers.go`). The series names are the OTel to Prometheus names of the exporter: `http_server_request_duration_seconds_{bucket,sum,count}` and `http_server_request_total`, with the labels `http_request_method`, `http_route` and `http_response_status_code`, plus `db_pool_*`, `go_*` and `process_*`.
- With either switch on, the connection pool is exported as `db.pool.*` (max open, open, in use, idle, wait count and duration, closed because of the idle or lifetime limit).
- With `OTEL_ENABLED=true` every request has a server span (`otelgin`, after `RedactWSToken`, so a `/ws` token is never recorded) and, with auth on, the span carries `user.id`, `user.name` and `user.role`.
- Auth counters (all `status` outcomes are fixed sets): `auth.login.total` (`method`, `status`, and `reason` for a failed local login), `auth.token.refresh.total`, `auth.jwt.total` (`success_unchecked` is a request that was let through because the session store lookup failed with fail-open), `auth.apikey.total`.
- WebSocket counters: `ws.revalidation.escalation.total` (identities closed because they stayed unverified for several revalidation runs in a row, fail closed), `ws.revalidation.failure.total` (`lookup`: failed lookups of a revalidation run) and `ws.broadcast.dropped.total` (broadcast channel full).

Repeated errors do not flood the log. A lookup that fails on every request (the session store or the database is down) logs one Error line per minute per middleware, with `suppressed` counting the lines that were left out; the others go to Debug. The WebSocket revalidation does the same, and so does a full broadcast channel. The counters above count every event, so an outage stays visible in the metrics. Alert on `auth.jwt.total{status="success_unchecked"}`, `auth.jwt.total{status="error"}` and `ws.revalidation.escalation.total`.

## Metrics and pprof servers

Both run on ports of their own, started by `main` (`backend/api/servers.go`) and stopped first by `shutdown`. Neither is ever on the API router.

- Metrics: `METRICS_ENABLED=true` serves `GET /metrics` (only that path) on `METRICS_ADDR` (`:9090`). The endpoint has no authentication and lists the route patterns, status codes and runtime figures. Keep the port on a private network (a Compose network or an internal service of the platform): publish it to the host on `127.0.0.1` at most, and never to the internet. A `METRICS_ADDR` that is not loopback (the default `:9090` is not) logs an Info line at start that says so; it is not a Warn because the Compose setup uses the default on its private network.
- pprof: `PPROF_ENABLED=true` serves `/debug/pprof/` (`profile`, `trace`, `heap`, `goroutine`, `mutex`, `block`, `cmdline`, `symbol`) on `PPROF_ADDR`, loopback by default. The mutex and block profiles stay empty until `PPROF_MUTEX_FRACTION` and `PPROF_BLOCK_RATE` are set. Leave it off in production; when you need it, reach the loopback port with a port-forward or an SSH tunnel and turn it off again. `LoadConfig` refuses a non-loopback `PPROF_ADDR` unless `PPROF_ALLOW_NON_LOOPBACK=true`, and `startPprofServer` checks it again.
- A port that cannot be bound is logged at Error and the API starts without that server (it is a diagnostic, not worth a process that does not start). Check the log after a start with either switch on.
- Profile from the host with `go tool pprof http://127.0.0.1:6060/debug/pprof/heap`. `docker-compose.yml` does not publish the pprof port; to profile a container, run it with `PPROF_ENABLED=true PPROF_ADDR=0.0.0.0:6060 PPROF_ALLOW_NON_LOOPBACK=true` and publish the port as `127.0.0.1:6060:6060` in a local override file.

## Observability setup (development only)

`make dev-otel` runs `docker compose -f docker-compose.yml -f docker-compose.otel.yml up --build`: the application plus an OpenTelemetry Collector, Prometheus, Tempo and Grafana (`config/otel/`). The overlay sets `OTEL_ENABLED=true`, `OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317`, `METRICS_ENABLED=true` and `METRICS_ADDR=:9090` on the backend.

| Service | Host address | Role |
|---|---|---|
| Grafana | `http://localhost:3001` | Login `admin` with `GRAFANA_ADMIN_PASSWORD` (default `devonly-change-me`, dev only). Anonymous access is off. Dashboards "API overview" and "Go runtime"; datasources Prometheus and Tempo. |
| Prometheus | `http://localhost:9090` | Scrapes the backend at `backend:9090/metrics` (job `backend`), the collector, Tempo and Grafana every 15 s. |
| Tempo | `http://localhost:3200` | Stores the traces that the collector forwards; Grafana reads it. |
| Collector | `localhost:4317` (gRPC), `localhost:4318` (HTTP) | Receives OTLP; traces go to Tempo, logs and OTLP metrics to the collector's own log (`debug` exporter), so the application metrics exist once, from the scrape. |

- **Dev only.** The backend sends OTLP over plain gRPC to the collector service name on the private Compose network, so it logs the plaintext warning at start (the collector host is not loopback). Do not copy the endpoint, the Grafana password or the unauthenticated collector to another environment: use a collector on a trusted network and your own credentials there.
- Every host port is published on `127.0.0.1` only. The backend's `/metrics` port and pprof are not published; pprof stays off.
- Every image has a pinned tag (`otel/opentelemetry-collector-contrib:0.120.0`, `grafana/tempo:2.7.2`, `prom/prometheus:v3.3.0`, `grafana/grafana:12.3.1`). Bump them on purpose.
- The Tempo, Prometheus and Grafana data live in named volumes. `docker compose -f docker-compose.yml -f docker-compose.otel.yml down -v` removes them, and a changed `GRAFANA_ADMIN_PASSWORD` needs that or a reset inside Grafana.

## Replicas

- Shared through MySQL: all data, the token blocklist and OIDC login states (`SESSION_STORE=mysql`), the refresh-token families and the audit log. A logout on one replica is seen by all others at their next request. A WebSocket on another replica closes at the next revalidation pass (every `websocket.DefaultRevalidateInterval`) or at token expiry.
- Per replica, in memory: the rate limiters, the username login throttle, the WebSocket hub and its connections. WebSocket broadcasts reach only the sockets of the replica that sent them, except the Items events with the outbox module on: every replica delivers those to its own sockets ("Outbox").
- `SESSION_STORE=memory` is for one replica: a restart forgets every revocation and a second replica does not see any.
- Leader workers (`refresh-token-cleanup`, hourly, `session-store-cleanup`, every 15 minutes, with the heartbeat on `replica-heartbeat-cleanup`, every 15 minutes, with the idempotency module on `idempotency-cleanup`, hourly, with the outbox module on `outbox-cleanup`, every minute, and with the password reset module on `password-reset-cleanup`, hourly) run in a `leader.Group`. With `LEADER_ELECTION_ENABLED=false` (the default) each process starts them itself; they only delete expired rows, so several replicas running them is safe. With `LEADER_ELECTION_ENABLED=true` they run on one replica at a time (below). A new worker must still be safe to run twice for a short time: see "Failure semantics".
- `EnsureAdminUser` is safe when replicas start together: it never changes an existing user and treats a duplicate key as success.

### Leader election

`leader.Lease` (`backend/internal/leader/lease.go`) elects the leader on one row of `leader_leases`:

| Column | Type | Meaning |
|---|---|---|
| `name` | `varchar(64)`, primary key | The lease name; the bootstrap uses `leader-workers`. |
| `holder` | `varchar(253)` | The process ID of the holder: host name plus `-` and 8 random hex characters (`replica.ProcessID`), new for every process start. No secret. |
| `expires_at` | `datetime(3)` | When the lease ends, in the database server's UTC clock. |

- **Acquire and renew** are the same atomic statement, every 2 s: `UPDATE leader_leases SET holder = me, expires_at = UTC_TIMESTAMP(3) + 15 s WHERE name = ? AND (holder = me OR expires_at < UTC_TIMESTAMP(3))`. When no row matched, the holder is read back (MySQL counts changed rows, so a renewal in the same millisecond reports 0) and, without a row, inserted; the loser of a duplicate-key race is a follower.
- **Time source: the database clock.** The expiry is written and compared with `UTC_TIMESTAMP(3)` in the same statement, so the clocks of the replicas never decide who may take the lease, and no Go time value crosses the driver (its `loc` and `parseTime` settings do not matter). The holder measures its own deadline with its monotonic clock from the moment it sent its last successful renewal; the database evaluated that renewal later, so the holder's deadline always comes before the stored expiry. All replicas must use the same MySQL server (the primary).
- **Takeover.** A replica that crashed is replaced about 15 s plus 2 s after its last renewal. A replica that shuts down releases the lease, and the next one takes over within 2 s; if its workers did not stop in time it keeps the lease until it exits (see "Shutdown").

### Failure semantics

- The holder stops acting as leader (cancels the workers' context and calls `Group.Stop`) when no renewal succeeded for 10 s: database errors, timeouts or a lost connection. Every statement has a timeout that ends at that deadline, so a database that does not answer cannot keep it leader. Its lease is then still valid for at least 5 s, which is the time the workers have to stop before another replica can start: a worker must return within that after its context is cancelled.
- A renewal that finds another holder in the row (the lease expired and was taken) stops the leader work at once. Every replica that is not leader keeps campaigning, so a deposed or recovered replica can lead again later.
- The lease trusts the wall clock of the database server: a step forward of that clock by more than `Duration - RenewDeadline` (5 s) can expire a lease while its holder still acts as leader, so two holders can overlap. Run NTP on the database server in slewing mode (gradual correction), not stepping.
- The lease is not a fencing token: a process that was paused (long GC pause, frozen VM) for longer than the lease can finish a write that it started before the pause while another replica already leads. Keep leader work idempotent and keep state that must survive a leader change in the database.
- When the database is unreachable for every replica, no replica leads until it is back: the leader work pauses rather than running twice.

### Replica heartbeat

With `REPLICA_HEARTBEAT_ENABLED=true` every process (leader or not) upserts its row in `replica_heartbeats` (`id` `varchar(253)` primary key, the process ID; `last_seen` `datetime(3)`, the database server's UTC clock, indexed) every 30 s, and deletes it on a clean shutdown (step 11). A failed write is logged once until the next success and does not stop the process. A row older than 2 minutes (`replica.StaleAfter`) belongs to a process that ended without a clean shutdown; the `replica-heartbeat-cleanup` leader worker deletes those rows. The table lists the running processes, for example `SELECT id, last_seen FROM replica_heartbeats ORDER BY last_seen DESC`.

## Outbox

With `OUTBOX_ENABLED=true` the Items events (`item.created`, `item.updated`, `item.deleted`) go through the table `outbox_events` instead of a direct broadcast after the commit. The payloads are the ones of the direct broadcast, so the SPA is unchanged.

**Write.** The Items repository (`models.GenericRepository`, built by `database.NewRepository` with `outbox.ItemHook`) runs the change and the insert of the event row in one transaction, inside the lock retry (`models.RetryOnLock`). A rolled back or failed change (validation error, version mismatch, duplicate, not found) leaves no row. The handler does not broadcast when the module is on, so each socket gets one copy. The row has `id` (BIGINT auto increment, the delivery order), `event_type`, `message` (the final JSON `{"type": ..., "payload": ...}`, so every replica sends identical bytes) and `created_at` (`DATETIME(3)`, the database clock, indexed for the janitor). The table exists after the migrations whether or not the module is on; with it off nothing reads or writes it.

**Delivery.** Every replica runs one poller (`outbox.Poller`, not a leader worker): every `OUTBOX_POLL_INTERVAL` it reads the rows with `id` greater than its cursor in id order, in batches of 100, and calls `Broadcast` on the hub of its own process. The cursor is in memory and starts at the highest id at process start, so a restart does not replay old events. A failed poll is retried on the next tick and logged once per minute at Error (`logutil.Limiter`).

**Pace.** The hub drops a broadcast that does not fit its 256 slot queue and disconnects a client whose own 256 slot send queue is full, and the poller's cursor moves on after each send, so a burst (a catch-up after a pause, a backlog of more than 256 events) would lose events silently. The batch is therefore 100, under half of the queue, and before every event the poller asks the hub for its backlog (`Hub.BroadcastBacklog`: the fullest of the broadcast queue and the client send queues) and waits, one millisecond at a time, while it is at least 128. A hub that does not drain (a stalled socket) is waited for at most 2 s once per poll (`outbox.DefaultBackpressureTimeout`): after that the rest of the poll sends without waiting, the hub may drop a message or disconnect that slow socket as for any broadcast, and one Warn per minute is logged. The wait is for the fullest queue, so one slow socket whose queue stays at half or more delays delivery to every socket on that replica by up to 2 s per poll until the hub disconnects it. Cancelling the poll during a wait leaves the cursor before the event that was not sent, so the next poll sends it.

**Gap window.** Auto increment ids can commit out of order: a transaction that took id 5 may commit after the one that took id 6. The poller remembers the ids below its cursor that it has not seen and looks them up on every poll for the gap window (the larger of 10 s and four poll intervals), then delivers them when they appear (out of id order, but each event once). At most 10 000 ids are tracked; after a bigger jump only the newest ones are. The limit: a transaction that stays open longer than the gap window, between taking its id and committing, can be skipped by every replica. Keep transactions on Items short.

**Guarantees.** A committed event is delivered by every running replica, also when the replica that wrote it died before it delivered. A healthy socket receives a backlog larger than the hub queue in full and in order (the pace above); the limit is a socket that stops reading for longer than the 2 s wait: the hub disconnects it, and the SPA refetches when it reconnects. Delivery is at most once per socket: a socket that connects after the event, or while its replica was down or the poll failing for longer than the retention, never gets it, and there is no replay for a reconnecting client (the SPA refetches on reconnect). There is no sent flag: each replica keeps its own cursor, so a replica that was down delivers nothing of what it missed. Order within one item is the id order (the row lock of the item serialises its writers).

**Cleanup.** The leader worker `outbox-cleanup` (every minute, only with the module on) deletes the rows older than `OUTBOX_RETENTION` in batches of 1000, at most 100 batches per run, oldest first. It is safe on several replicas. Keep the retention above the longest time a replica may be paused: the minimum is 1 minute and ten poll intervals.

**Metrics** (only with `METRICS_ENABLED` or `OTEL_ENABLED`; with both off nothing is registered and the poller makes no statistics query; no instrument has a label): `outbox.events.delivered` (counter, events sent to this replica's hub), `outbox.delivery.age` (gauge, seconds between the commit of the newest delivered event and its delivery, by the database clock; it keeps the last value while the outbox is idle) and `outbox.cursor.lag` (gauge, ids between the cursor and the table maximum, measured with one `MAX(id)` query at the end of every poll, also a failed one; a poll whose query failed keeps the last value).

**Start check.** `OUTBOX_ENABLED=true` with a repository that does not write the events (no change hook) stops the start: a silent start would drop every event.

## Background jobs

With `JOBS_ENABLED=true` a feature can run work in the background: a handler enqueues a job into the MySQL table `jobs` (`jobs.Queue.Enqueue`), and a pool of workers on every replica runs the handler that the application registered for the job's type (`registerJobs` in `backend/api/jobs.go`). The template registers no handler and enqueues nothing itself; the walkthrough is in `EXTENDING.md`, "Adding a background job". Moving the password reset mail onto the queue is out of scope (its timing rules stay).

**Table.** `jobs` (migration `20261121000001`, always created, also with the module off): `id`, `type` (100), `payload` (at most 64 KiB, JSON), `status` (`pending`, `running`, `succeeded`, `dead`), `attempts`, `max_attempts`, `run_at`, `locked_by` (the replica ID and the worker number), `locked_until`, `last_error` (cut to 2000 characters), `created_at`, `updated_at`, `finished_at`. Every time column is `DATETIME(6)` and is written and compared with the database clock (`UTC_TIMESTAMP(6)`): replicas with skewed clocks agree on what is due and what has expired. The indexes cover the claim (`status`, `run_at`, `id`), the cleanup (`status`, `finished_at`) and the admin list.

**Claim.** The pool of a process claims only the types that its registry holds (no handler, no claim; a job of a type that no replica of this version handles waits for a replica that does, which suits a rolling deploy). One transaction runs `SELECT ... FOR UPDATE SKIP LOCKED` twice, first the `running` jobs whose `locked_until` passed, then the `pending` jobs whose `run_at` passed, each in `run_at, id` order, up to the number of free workers; an `UPDATE` then sets `running`, `attempts + 1`, `locked_by` and `locked_until`. The claim transaction runs at `READ COMMITTED`, so rows that it scans but skips (another type, a backlog nobody handles) stay unlocked. Replicas never block each other and never take the same row. An idle pool polls every `JOBS_POLL_INTERVAL` (with jitter, so replicas do not poll in step); a failing claim backs off up to 30 s and its log line is rate limited.

**Outcome.** A handler that returns nil makes the job `succeeded`. An error makes it `pending` again after a delay, or `dead` when `attempts` reached `max_attempts`. `jobs.Permanent(err)` makes it `dead` at once. A panic is recovered and counts as a failure (the call trace is logged, the panic value is never stored, because it can hold payload data). `last_error` holds the error text of the last failure, cut to 2000 characters; no log line carries the payload.

**Backoff.** The delay before attempt n+1 is `min(JOBS_BACKOFF_BASE * 2^(n-1), JOBS_BACKOFF_MAX)`, shortened by a random share of up to 20%. The jitter only shortens, so `JOBS_BACKOFF_MAX` is a hard bound.

**Lease and fencing.** While a handler runs, the pool extends `locked_until` every third of `JOBS_VISIBILITY_TIMEOUT`. A job whose lease passed (the replica crashed, hung or lost the database) is claimed again by another worker; when its `attempts` already reached `max_attempts` it becomes `dead` with the error `lease expired after N attempts` instead of running again. `Extend`, `Complete`, `Fail` and `Release` update `WHERE id = ? AND locked_by = ? AND status = 'running'`: zero rows means the job was taken over, so the worker drops its result and cancels the context of the handler.

**Delivery is at least once.** A crash after a handler's side effect and before `Complete`, or a job that outlives its lease because the process hung, runs the handler again, also at the same time on another replica. A handler must be idempotent (use a key from the payload for the side effect). There is no deduplication or unique key: two `Enqueue` calls make two jobs. `attempts` counts claims, so a crash counts as an attempt; a job that shutdown interrupted is released without counting.

**Cleanup.** The leader worker `jobs-cleanup` (every minute, only with the module on) deletes `succeeded` rows older than `JOBS_RETENTION` and `dead` rows older than `JOBS_DEAD_RETENTION` in batches of 1000, at most 100 batches per status and run, oldest first; it returns within 5 s of a cancel and is safe on several replicas. `pending` and `running` rows are never deleted.

**Shutdown.** The pool is stopped by its own step (step 5, "Shutdown" above): no new claim, a fixed 5 s for the running jobs, then a cancel and a release within 2 s more. The workers are stopped by that step, after the in-flight work and before the hub and the database close, not by `inflight.Tracker`. A handler must honour its context: a handler that ignores the cancel keeps its goroutine until the process exits, its job is released anyway and its late result is dropped by the fence.

**Admin list.** `GET /api/v1/jobs` (only with the module on; 404 otherwise) returns `{data, total, limit, offset}`: `limit` (default 25, at most 100), `offset` (at most 100000), `status`, `type`, `sort` (`created_at`, `run_at`, `id`; default `id`) and `order` (`asc`, `desc`; default `desc`, newest first); a bad value is 400. A job shows its type, status, attempts, schedule, worker and `last_error`, never the payload. With auth on it needs the admin role (`RequireAdmin()`), with auth off it is open like every other route. It is a read, so it writes no audit entry.

**Health.** The informational check `jobs` (never gates readiness) reads the snapshot that the pool samples every 30 s, so a readiness call makes no query. It is DEGRADED when the pool is not running, or when the oldest due job has waited longer than `JOBS_STALE_AFTER` (also a job type that no replica handles). The message holds counts (pending, due, running, dead) and never a payload or a job error. Before the first sample it does not judge the age.

**Metrics** (only with `METRICS_ENABLED` or `OTEL_ENABLED`; with both off nothing is registered): `jobs.processed` (counter, attributes `type` and `outcome` of `succeeded`, `retried`, `dead` and `released`), `jobs.duration` (histogram, seconds a handler ran, attribute `type`), `jobs.depth` (gauge, rows per `status`) and `jobs.oldest_due.age` (gauge, seconds). The `type` is a name that the code registered, never user input. The depth comes from the 30 s sample; nothing queries per scrape.

**Limits.** At most `JOBS_WORKERS` handlers run at once per replica, so a fleet runs `JOBS_WORKERS` times the replicas. A payload is at most 64 KiB. Keep a handler's runtime within what the shutdown allows (5 s) or make it resumable: a longer job is cancelled and released at shutdown and runs again from the start. No priorities, no rate limit per type, no recurring schedule (use a `leader.Worker`), no retry, cancel or delete through the API.

## Response compression

`middleware.Compression` (standard library `compress/gzip`, level 5, writers pooled) gzips a response when the client sends `Accept-Encoding` with `gzip` and a q-value above 0 (`gzip;q=0` refuses it; `*` does not count as gzip). It is mounted by `routes.Setup` right after `Recovery` and before `SecurityHeaders`, CORS and the handlers, only with `COMPRESSION_ENABLED=true`. Nothing else changes with it off.

A response is compressed only when all of these hold:

- the method is not `HEAD`, the status allows a body (not 1xx, 204, 304) and is not 206 (no `Content-Range`);
- the handler set no `Content-Encoding` and no `Cache-Control: no-transform`;
- the content type is on the allowlist: `text/*`, `application/json` and `+json`, `application/xml` and `+xml`, `application/javascript`, `image/svg+xml` (so images, archives and other binary formats are never touched). A response without a `Content-Type` is sniffed from its first bytes (`http.DetectContentType`) and compressed only when the sniffed type is on the list;
- the body reaches `COMPRESSION_MIN_SIZE`: the declared `Content-Length` when there is one, otherwise the bytes written (held back until the size is known). A smaller body is sent as it was, with its length. A response that is flushed (a stream) starts compressing at the first flush.

A compressed response has `Content-Encoding: gzip`, no `Content-Length` and a weak `ETag` (`W/`). `Vary: Accept-Encoding` is added to every response of a compressible type, compressed or not, and to a `HEAD` answer, so a cache never serves one variant for the other. The large JSON answers (Items and user lists, `/swagger/doc.json`) are the ones that benefit.

**What it does not touch.**

- `/ws`: a request for the path `/ws` or with `Upgrade: websocket` is never wrapped, so the upgrade (`Hijack`) works as before. The hub writes its own frames.
- Request bodies: they are never decompressed. A request with `Content-Encoding: gzip` reaches the handler as it is (and a JSON bind fails with 400). `MaxBodySize` (1 MB) and the 413 answers are unchanged.
- The metrics and pprof servers (own ports).

**Interplay.** After a panic in the chain before the compressed stream started, the writer is put back and `Recovery` answers a plain 500 `Internal server error` (only this case gives a clean 500). After the stream started the status (usually 200) and the headers are already sent: the stream is left without its gzip trailer, so the client's gzip reader fails with an unexpected EOF and cannot take the cut response for a complete one, and the 500 body is dropped (it would be taken for gzip data). `Logger` and the metrics, outside, record the status that was already sent for such a request (not 500), so the only trace of the panic is the Error log of `Recovery`. For any other request they see the real status. The audit recorder and the idempotency module sit inside: they see the uncompressed bytes, so the stored response of an `Idempotency-Key` is the plain one and a replay is compressed per the `Accept-Encoding` of the replay request.

**The bundled nginx does not compress.** `frontend/nginx.conf` has no `gzip` directive and the stock `nginxinc/nginx-unprivileged:alpine` image has it commented out, so neither `/api` nor the SPA files are compressed by the proxy. The backend compresses the API answers; nginx passes them on (a `Content-Encoding: gzip` answer from the backend is proxied as it is). Compressing the static files of the SPA is a separate change in `frontend/nginx.conf`. A proxy of your own that also compresses must not compress an answer that already has a `Content-Encoding`.

**BREACH.** Compressing a response that mixes a secret with text an attacker can choose can leak the secret through the compressed size. No response of this template is exploitable this way:

- The login, refresh and token answers have a fixed shape and are under the 1024-byte default, so they are not compressed. That argument holds for the default only: with `COMPRESSION_MIN_SIZE=1` they would be compressed.
- `CreateAPIKeyResponse` echoes the `name` that the caller chose next to `raw_key`. It is not exploitable: the route needs an `Authorization` header, which a page of another site cannot make the browser send, so an attacker cannot force requests with chosen names under the victim's credentials. `/api-keys` and `/auth/*` are also on the idempotency skip list, so the secret is never stored or replayed.

Before you add an endpoint that echoes request input next to a secret (a CSRF token, an access token), keep it under the minimum or turn the module off.

**Cost.** Each response of a compressible type that reaches the minimum costs CPU. The writers are pooled (about 1 MiB of state each), and a body under `COMPRESSION_MIN_SIZE` is not compressed. Raise the minimum or set `COMPRESSION_ENABLED=false` when the backend is CPU bound and a proxy in front compresses instead.

## Idempotency keys

With `IDEMPOTENCY_ENABLED=true` a `POST` on `/api/v1` is safe to retry. A client sends `Idempotency-Key: <key>` (1 to 255 bytes of printable ASCII, white space around it is removed; an empty, longer or non-ASCII key is 400). A request without the header, and every method other than `POST`, passes through unchanged.

**Rules** (`middleware.Idempotency`, table `idempotency_keys`, primary key `(scope, idempotency_key)`):

- The scope is the authenticated user ID (JWT and API key of one user share it) with auth on, and one shared scope with auth off. Two users may use the same key.
- The first request claims the key (an insert), runs the handler and stores the status, the `Content-Type` and the body of the response (nothing else: no other header). The response is sent after it is stored, so a client that retries after it saw the answer always gets a replay.
- The same key with the same user, method, path, query and body replays the stored response (same status, content type and body) with `Idempotency-Replayed: true`. The handler does not run: no second change, no second audit entry (`SkipAudit`), no second WebSocket event.
- The same key with another method, path, query or body answers 422. The same key while the first request still runs answers 409 with `Retry-After` (the seconds until the lock can be taken over).
- A stored response is replayed as it was, also a 4xx. A 5xx, a 499 (client left), a handler panic, a response over 256 KiB and a flushed (streamed) response release the key (the row is deleted; the large response is still sent), so the client can retry and the handler runs again. 2xx, 3xx and 4xx are stored.
- A request that runs longer than `IDEMPOTENCY_LOCK_TIMEOUT` may lose its key to a retry with the same request: the retry takes the key over (one winner, decided by one `UPDATE`) and runs the handler again. Only the owner of the row (`lock_id`) may store a response or release the key, so the slow request cannot overwrite its successor; its own response is still sent. Set the timeout above the longest `POST`. The same takeover frees the key of a crashed process.
- The middleware runs before the route-level role gates (`RequireAdmin`, `RequireRole`). A 4xx answer is stored like any other, so a user who was refused (403) and is promoted later gets the stored 403 for that key until the TTL ends: use a new key. A replay does not run the gates again.
- With auth off every client shares one scope, and any client can add rows. The per-IP `RATE_LIMIT`, the TTL and the 256 KiB response cap bound the table; size `IDEMPOTENCY_TTL` accordingly.
- A key is not protected when the handler committed but no response was stored: a 5xx answer (key released), a 2xx or 3xx over 256 KiB, a flushed response, or a failed store (the key stays locked until the lock timeout). A retry then runs the handler again.
- A replay returns the status, `Content-Type` and body only. Other headers that a handler set (for example `Location` or `Cache-Control`) are not stored.
- The claim, the store of the response and the release run on a detached context with a deadline (`ctxutil.Detach`), never the request context. A failing claim answers 500 `Internal server error` and never runs the handler unprotected; a failing store of the response is logged (once per minute at Error) and the response is sent anyway, and the key then times out like a crashed one.
- `POST /api/v1/auth/*` and `POST /api/v1/api-keys` are never touched: a replayed login would replay tokens, and the response of an API key create holds the raw key, which is shown once and never stored.
- CORS (only with the module on): `Idempotency-Key` is an allowed request header, `Idempotency-Replayed` and `Retry-After` are exposed to browser scripts (next to `X-Total-Count`, which is always exposed).

**Several replicas.** MySQL is the arbiter, no state is kept in a process: a claim is one insert on the primary key, a takeover one conditional `UPDATE`, and every time (`locked_at`, `expires_at`) is written and compared with the database clock (`UTC_TIMESTAMP(3)`). Any replica answers a replay or a 409 for a key that another replica runs.

**Cleanup.** The leader worker `idempotency-cleanup` (hourly, only with the module on) deletes the rows past `expires_at` in batches of 1000 (the index `idx_idempotency_keys_expires_at`) and loops until a batch is short. A request that finds an expired row takes it over, so the replay window never outlives the TTL even before the worker ran. The table exists after the migrations whether or not the module is on, and turning the module off leaves its rows (they are not read).

**Stored data.** The body of a response is stored unencrypted for the TTL. A `POST` whose response holds a secret belongs on the skip list (`idempotencySkipPrefixes` in `routes.go`).

## Health

`GET /health/live` always answers 200 with the uptime. `GET /health/ready` runs the registered checks (the database ping) and answers 503 when a gating check fails; an informational check (`AddInformationalCheck`, the `mail` check) that fails shows as `DEGRADED` and leaves the status and the 200 unchanged. `GET /health` is the old plain check (`{"status":"ok"}`). The three are public and not rate limited. Register a check for every new external dependency with `healthChecker.AddCheck`, or with `AddInformationalCheck` when the service works without it. The Compose backend check is `wget -q -O /dev/null http://localhost:8081/health/live` (the Alpine image has `wget`, not `curl`).

## Logging

Structured text on stderr through `slog`, at `LOG_LEVEL`. With `OTEL_ENABLED=true` the records also go to the OTLP collector (see "Telemetry"). Failed logins log `reason`, the capped username, `client_ip` and `request_id` at Warn, never the password or hash. `RequestID` accepts a client `X-Request-ID` of 1 to 64 characters `[A-Za-z0-9._-]` and returns it; anything else is replaced. GORM errors and slow queries (200 ms or more) go through `slog`; with `APP_DEBUG=false` the SQL values are replaced with `?`. Keep `APP_DEBUG=false` in production.

## Audit log

With auth on, every successful `POST`, `PUT`, `PATCH` or `DELETE` of an authenticated user writes an entry (action, entity, user, route, status, client IP, request ID), after the response, on a context the client cannot cancel (30 s). Failed requests, reads, unauthenticated requests and everything under `/auth/` are not recorded, with one exception: the self-service password change (`PUT /auth/me/password`, action `change_password`, entity type `user`, entity ID the caller) has an entry in `auditRoutes` and is recorded. There is no audit entry for login, logout or refresh. An admin resets a password through `PUT /users/:id`, which is audited too. At most 64 writes run at once; an entry over that bound is dropped with an Error log. `GET /api/v1/audit-logs` (admin session) pages with `limit` and `offset`, or with `next_cursor`; a cursor page has `total: -1`. There is no audit log page in the web UI.

## Swagger

With `ENABLE_SWAGGER=true` the UI is at `/swagger/index.html` and stays open even with `AUTH_ENABLED=true`: the spec lists every route. Keep it off in production. Regenerate the spec with `make docs`.

## Coverage, repeated tests, sample data and load tests

None of the variables in this section is read by the backend: they are Make and script variables, set on the command line (`make loadtest LOADTEST_VUS=20`) or in the environment.

### Coverage gate

`make test-coverage` runs the unit tests (`-short`, no database) with `-coverpkg` over the packages of `backend/coverage-packages.txt` and fails when any listed package has less than `COVERAGE_MIN` percent (default 80) on its own; an average would hide a weak package. `-coverpkg` credits a package for what any test binary runs, so the `api` main package is measured through its bootstrap tests (its `main()` is not testable). The list is the set of gated packages; the file says what is left out on purpose (the generated `docs`). Add a package to the list in the change that adds it. Reports: `backend/coverage/coverage.html` and `coverage-summary.txt`. The logic is in `scripts/coverage-gate.sh`.

| Variable | Default | Notes |
|---|---|---|
| `COVERAGE_MIN` | `80` | Minimum percent per package for `make test-coverage`. |

### Repeated tests

`make test-backend-repeat` runs `go test -short -race -count=$(REPEAT_COUNT)` on the backend packages whose `.go` files changed against `REPEAT_BASE`, to find a flaky test before CI does. It compares the working tree (and untracked files) with the merge base of `REPEAT_BASE` and `HEAD`. Without a changed package it says so and succeeds. If `origin/main` does not exist (a clone without that remote) it uses the local `main`; any other unknown ref is an error.

| Variable | Default | Notes |
|---|---|---|
| `REPEAT_COUNT` | `3` | Positive integer. |
| `REPEAT_BASE` | `origin/main` | Any ref, for example `main` or a commit. |

### Sample data

`make seed` runs `scripts/seed-dev-data.sh` against a running backend (`make dev` or `make dev-backend`) and creates `SEED_COUNT` Items named `Sample Item 01`, `Sample Item 02`, and so on. It only creates: an Item whose exact name exists is skipped, so a second run changes nothing. A `429` is retried (the default `RATE_LIMIT` is 100 requests per minute).

| Variable | Default | Notes |
|---|---|---|
| `API_URL` | `http://localhost:8081` | Base URL of the API. |
| `SEED_COUNT` | `10` | Number of sample Items. |
| `API_TOKEN` | none | Access token, for a backend with `AUTH_ENABLED=true`. |
| `ADMIN_USERNAME`, `ADMIN_PASSWORD` | `admin`, none | With auth on and no `API_TOKEN`, the script logs in with them. Give the password in the environment, never in a file of the repository. |

With auth off no credential is needed.

### Load tests

`make loadtest` runs two k6 scripts against a **running** backend with the `k6` binary on your machine (install: <https://grafana.com/docs/k6/latest/set-up/install-k6/>; the scripts use only the standard k6 modules). There is no Docker variant: one tool, one way to run it. `make -n loadtest` works without k6; only running it needs the binary (`K6=/path/to/k6` picks another one).

- `loadtest/k6-api.js`: two scenarios for `LOADTEST_DURATION`: `browse` (`LOADTEST_VUS` users list, filter and read Items) and `crud` (half as many users create, read, update and delete an Item). `SCENARIO=browse` or `SCENARIO=crud` runs one.
- `loadtest/k6-websocket.js`: `LOADTEST_VUS` clients open `/ws` and subscribe to `items` while one writer creates and deletes Items, so every listener receives the broadcasts.
- `loadtest/k6-stress.js` (`make loadtest-stress`): opt-in. It ramps up to `LOADTEST_STRESS_VUS` users in steps of `LOADTEST_STRESS_STEP` with 80% reads and 20% create-and-delete, with thresholds three times looser. It is never part of `make loadtest`.

Thresholds make k6, and so `make`, exit non-zero on a regression: error rate, check pass rate and p95/p99 latency of every request (the WebSocket run: connection error rate, p95 connect time and at least one broadcast received). The Items the tests create are named `k6-loadtest-...` and deleted again; the teardown removes any a failed iteration left.

Start the backend with a high rate limit, or the default 100 requests per minute per client IP stops the run at once (setup reports a `429`): `RATE_LIMIT=1000000 make dev-backend`. Safety: the scripts refuse a target that is not `localhost`, `127.x.x.x` or `[::1]` unless `LOADTEST_ALLOW_REMOTE=1`, so a stray `API_URL` cannot load a shared backend. They never write to anything but Items.

| Variable | Default | Notes |
|---|---|---|
| `API_URL` | `http://localhost:8081` | Target backend. |
| `LOADTEST_ALLOW_REMOTE` | unset | `1` allows a target that is not local. Use it only for a backend you own. |
| `LOADTEST_VUS`, `LOADTEST_DURATION` | `10`, `30s` | Virtual users and length of the steady phase. |
| `LOADTEST_STRESS_VUS`, `LOADTEST_STRESS_STEP` | `100`, `30s` | Peak and step length of the stress profile. |
| `LOADTEST_P95_MS`, `LOADTEST_P99_MS` | `500`, `1500` | Latency thresholds in milliseconds. |
| `LOADTEST_ERROR_RATE` | `0.01` | Maximum error rate, 0 to 1. |
| `K6` | `k6` | The k6 binary. |
| `API_TOKEN`, `API_KEY`, `ADMIN_USERNAME`, `ADMIN_PASSWORD` | none, none, `admin`, none | For `AUTH_ENABLED=true`, one of the three credentials: an access token (it expires after `ACCESS_TOKEN_EXPIRATION`, 15 m by default), an API key (`X-API-Key`), or a login in `setup()` once for all users. `/ws` takes the token as `?token=` and the key as a header. Pass them in the environment, not on the command line. |

`make check-make-targets` (part of `make lint`, run by CI) fails when one of these targets is missing, is not `.PHONY` or cannot be planned with `make -n`.

## End-to-end tests

`make test-e2e` (auth off) and `make test-e2e-auth` (auth on) run `scripts/e2e.sh`: MySQL in its own Compose project, a natively built backend, the Vite dev server and Playwright (Chromium). The run sets `RATE_LIMIT=10000` and `LOGIN_RATE_LIMIT=1000` for the backend, because every request comes from 127.0.0.1.

`make test-e2e-auth` also tests the password reset by mail. It starts Mailpit in the same Compose project (`docker-compose.mail.yml` plus `docker-compose.mail-e2e.yml`, which publishes the SMTP port, because the e2e backend runs on the host, not in Compose), waits for `/readyz`, clears the inbox, and runs the backend with `MAIL_ENABLED=true`, `SMTP_HOST=127.0.0.1`, `SMTP_PORT=$E2E_MAILPIT_SMTP_PORT`, `SMTP_TLS=none` (no credentials), `MAIL_FROM`, `PASSWORD_RESET_ENABLED=true`, `PASSWORD_RESET_URL=http://localhost:$E2E_FRONTEND_PORT` (the origin of Playwright's `baseURL`) and `PASSWORD_RESET_MIN_INTERVAL=1s`. Mailpit goes down with the rest. `make test-e2e` (auth off) starts no Mailpit and leaves both modules off (the reset routes answer 404). The spec `e2e/password-reset*.spec.ts` skips itself when `MAILPIT_URL` is unset.

| Variable | Meaning |
|---|---|
| `E2E_AUTH` | `1` when the backend runs with `AUTH_ENABLED=true` (`make test-e2e-auth` sets it). `global-setup.ts` fails the run when this and the backend disagree. `auth.spec.ts` skips itself without it. |
| `A11Y_REQUIRE_AUTH` | `1` makes the accessibility spec fail, instead of skip, a page whose precondition is missing (Login, Admin Users). `make test-e2e-auth` sets it, so those pages are really checked. |
| `E2E_SECURE_COOKIES` | `false` (default) runs the backend with `SECURE_COOKIES=false` (cookie `refresh_token`); `true` checks the production cookie (`__Host-refresh_token`, `Secure`) in Chromium on `http://localhost`. CI runs both. |
| `E2E_ADMIN_USERNAME`, `E2E_ADMIN_PASSWORD`, `E2E_JWT_SECRET` | Test values of the throwaway backend, set by the Makefile. They are not defaults of the application and must never be used anywhere else. |
| `E2E_RETRIES` | Playwright retries; the default is 2 on CI and 0 otherwise. |
| `E2E_MYSQL_PORT`, `E2E_BACKEND_PORT`, `E2E_FRONTEND_PORT` | Host ports of the run (13306, 8081, 3000). A port in use is an error. |
| `E2E_MAILPIT_SMTP_PORT`, `E2E_MAILPIT_UI_PORT` | Host ports of Mailpit in `make test-e2e-auth`, both on 127.0.0.1 (11025 for SMTP, 18025 for the web UI and the API; not 8025, so `make dev-mail` can run beside it). A port in use is an error. |
| `MAILPIT_URL` | Set for Playwright in auth mode only: `http://127.0.0.1:$E2E_MAILPIT_UI_PORT` (default `http://127.0.0.1:18025`). The Mailpit API lives under it: `GET /api/v1/messages`, `GET /api/v1/message/<ID>` (field `Text`), `DELETE /api/v1/messages`. Unset in auth-off runs: the password reset spec skips itself. |
| `E2E_RESET_USERNAME`, `E2E_RESET_EMAIL`, `E2E_RESET_PASSWORD` | The seeded user of the password reset spec: `e2e-reset-user`, `e2e-reset-user@example.test`, and a random password per run (32 hex characters). Set for Playwright in auth mode only. |

`scripts/e2e.sh` (not `global-setup.ts`) creates the account `e2e-reset-user` for the password reset spec in auth mode, through `POST /api/v1/users` after the administrator signs in: role `user`, a local account with an email address, DEV ONLY. The bootstrap administrator is a service account and is not eligible for a reset, and `e2e-user` has no email. The run starts with an empty database, so the account is new each time; the spec may change its password (the password reset is the point), so use it only in that spec, and a test that needs a user who signs in uses `e2e-user`. The backend's minimum interval of one second between two mails to one account means a spec that requests twice waits one second in between.

`global-setup.ts` creates a non-admin account (`e2e-user`, random password per run) so that a test of a wrong password never counts toward the throttle of the administrator (5 failures lock a name, see `auth-sessions.md`). Keep every new failed-login test on that account, and do not raise `--repeat-each` beyond what the throttle allows. In CI, `e2e.yml` runs three passes: auth off, auth on, and auth on with `E2E_SECURE_COOKIES=true`.

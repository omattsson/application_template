# Operations

How to run, configure and stop the backend. Read it before you change configuration, the shutdown order, the Compose file or anything that runs on more than one replica. The auth behaviour is in `docs/claude/auth-sessions.md`.

## Requirements

- MySQL 8.0.17 or later. Migration `20260101000002` gives `session_entries.entry_key` the collation `utf8mb4_0900_bin`, which MySQL has from 8.0.17. `docker-compose.yml` runs `mysql:8.0`.
- The backend runs the migrations at every start (`database.NewRepository`). Replicas that start together serialize on the MySQL advisory lock `schema_migrations.<database>` (`GET_LOCK`). A replica that cannot get it waits up to 5 minutes (300 s), then the start fails with "timed out waiting for the migration lock". A migration that hangs therefore holds the other replicas for up to 5 minutes. Integration tests run one package at a time (`-p 1`) for the same reason: the packages share one database.
- Upgraders: migration `20260101000001` drops the old `users` table (the unused one with a numeric ID and no `password_hash` column) without copying its rows, then creates `users`, `refresh_tokens`, `api_keys` and `audit_logs`. The tables exist whether or not `AUTH_ENABLED` is true.
- Compose is a development setup. It publishes MySQL on host port `MYSQL_PORT` (3306) with the default credentials `MYSQL_PASSWORD=apppass` and `MYSQL_ROOT_PASSWORD=rootpass`. These are dev-only values: set your own, or do not publish the port, anywhere else.

## Configuration reference

`internal/config/config.go` and `oidc.go` read every variable below. `backend/.env.example` has the same list with comments, and `scripts/check-env-docs.sh` (`make check-env-docs`, part of `make lint`) fails when a variable of the code is missing here or in the example files, or when the example documents one that no code reads. Add a variable to the code, `backend/.env.example`, `backend/config/.env.example` and this table together.

An unset or empty variable has its default. A duration is a Go duration such as `15m` or `12h`. With `AUTH_ENABLED=true` a set but invalid boolean or duration of the auth and OIDC settings stops the start; with auth off they are read leniently (the default on an invalid value) because nothing uses them. `RATE_LIMIT` and `LOGIN_RATE_LIMIT` are strict in both modes.

### Application

| Variable | Default | Notes |
|---|---|---|
| `ENV_FILE` | `.env` | Dotenv file to load; a missing file is fine. A variable already in the process environment wins. |
| `APP_NAME` | `backend-api` | Required non-empty. |
| `GO_ENV` | `development` | Required non-empty; nothing branches on it. Compose uses the same name for the image target. |
| `APP_TITLE` | `Application Template` | Product name in `GET /api/v1/ui-config`. |
| `APP_DEBUG` | `true` | Gin debug mode and SQL values in the log. Set `false` in production (`make prod*` does). |
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
| `LOGIN_RATE_LIMIT` | `10` | Attempts per minute per client IP on each of login, register, OIDC login and OIDC callback; must be positive. |
| `ENABLE_SWAGGER` | `false` | Swagger UI at `/swagger/index.html`. |

### Logging and CORS

| Variable | Default | Notes |
|---|---|---|
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` (or `warning`), `error`; invalid stops the start. Applies to the default `slog` logger, which writes text to stderr. |
| `LOG_FILE` | empty | Read but not used: logs go to stderr. |
| `CORS_ALLOWED_ORIGINS` | `*` | `*` or a comma-separated list. `*` is for development only. It also gates the WebSocket `Origin` header. The other `CORS_*` names in old files are not read: methods and headers are fixed in `middleware.CORS`. |

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

### Replicas

Off by default: a single process behaves as before. Details: "Replicas" below.

| Variable | Default | Notes |
|---|---|---|
| `LEADER_ELECTION_ENABLED` | `false` | Strict boolean. With `true` the leader workers run only on the replica that holds the MySQL lease row `leader-workers` (`leader_leases`). With `false` every process starts them at once. |
| `REPLICA_HEARTBEAT_ENABLED` | `false` | Strict boolean. With `true` every process upserts its row in `replica_heartbeats` every 30 s and deletes it on a clean shutdown; the leader deletes rows older than 2 minutes. |

The lease timing is fixed in code (`leader.DefaultLeaseConfig`: 15 s lease, 10 s renew deadline, 2 s retry period), not configurable.

### Compose and Makefile variables

These are read by `docker-compose.yml`, the Makefile or the scripts, not by the backend.

| Variable | Default | Notes |
|---|---|---|
| `GO_ENV`, `NODE_ENV` | `production` | Image targets (`development` or `production`). `make dev*` sets development, `make prod*` production. |
| `BACKEND_PORT`, `FRONTEND_PORT` | 8081, 3000 | Host ports (`make prod*`: 8080 and 80). |
| `FRONTEND_CONTAINER_PORT` | `8080` | The unprivileged nginx of the production image. `make dev*` sets `3000` for the Vite dev server (set it too when running `NODE_ENV=development docker compose` by hand). |
| `MYSQL_PORT`, `MYSQL_DATABASE`, `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_ROOT_PASSWORD` | 3306, `app`, `appuser`, `apppass`, `rootpass` | Dev-only credentials. |
| `GRAFANA_ADMIN_PASSWORD` | `devonly-change-me` | Grafana admin password of `docker-compose.otel.yml`. A dev-only default: set your own. Read at the first start of an empty `grafana_data` volume. |
| `GRAFANA_PORT`, `PROMETHEUS_PORT`, `TEMPO_PORT`, `OTLP_GRPC_PORT`, `OTLP_HTTP_PORT` | 3001, 9090, 3200, 4317, 4318 | Host ports of the observability setup, all published on `127.0.0.1` only. |
| `FRONTEND_PROXY_IP`, `FRONTEND_NET_SUBNET` | `172.30.80.10`, `172.30.80.0/24` | Fixed proxy address on `frontend-net` and its subnet; change both together. |
| `VITE_API_URL` | `http://localhost:8081` | Target of the Vite dev proxy; Compose sets `http://backend:8081`. |

`docker-compose.yml` also passes `APP_ENV`, `PORT`, `API_VERSION`, `API_PREFIX`, `CORS_ALLOWED_METHODS`, `CORS_ALLOWED_HEADERS` and `CORS_MAX_AGE` to the backend. The backend reads none of them (it reads `GO_ENV`, `SERVER_PORT` and `CORS_ALLOWED_ORIGINS`), so they have no effect. Set the listening port with `SERVER_PORT`, never `PORT`. The development frontend does not read a `PORT` either: the Vite server is fixed to port 3000.

Two checkouts, or another project, collide on the fixed host ports 3000, 8081 and 3306 and on the subnet `172.30.80.0/24`. Change the ports with the variables above and the subnet with `FRONTEND_NET_SUBNET` and `FRONTEND_PROXY_IP`. `make test-e2e` uses its own Compose project and MySQL on host port 13306 (`E2E_MYSQL_PORT`), and the backend and the Vite server on the ports `E2E_BACKEND_PORT` and `E2E_FRONTEND_PORT` (8081 and 3000): a port in use is an error.

## Client IP and proxies

- The per-IP rate limits and the audit entries use `c.ClientIP()`. Only a peer in `TRUSTED_PROXIES` may set the client IP with `X-Forwarded-For` or `X-Real-IP`; otherwise it is the TCP peer. Behind a proxy with an empty list every request has the proxy's address, so all users share one rate-limit bucket.
- Compose sets `TRUSTED_PROXIES=${TRUSTED_PROXIES:-${FRONTEND_PROXY_IP:-172.30.80.10}}`. The `:-` fallback means an empty value in the environment is replaced by the proxy address: the list cannot be emptied through Compose. Edit the Compose file to run with no trusted proxy.
- Trust proxy addresses, never a whole network: on some Docker setups the network gateway is the source of traffic from the host, and a trusted gateway lets a client pick its IP.
- `SecurityHeaders` sends `Strict-Transport-Security` when the connection is TLS or the first `X-Forwarded-Proto` value is `https`. The backend reads that header from any client, trusted proxy or not, so a client that talks to the backend directly can make its own response carry HSTS (self-affecting only). Terminate TLS at a proxy and let it set the header.
- The bundled nginx (`frontend/nginx.conf`) and the Vite dev server both strip one `/api` prefix: the browser calls `/api/api/v1/...` and the backend sees `/api/v1/...`. The IdP redirect URI therefore is `<origin>/api/api/v1/auth/oidc/callback`.
- `/ws` carries the access token as `?token=` (a browser WebSocket cannot send headers). The backend removes it before its own logging (`RedactWSToken`), and the bundled nginx has `access_log off` for `/ws`. Any other proxy in front must not log the query of `/ws` either. The bundled nginx also sets `error_log /dev/stderr crit` for `/ws`: an upstream error would otherwise write the request line, token included, so only crit and worse is logged there, and upstream failures stay visible through the `/api/` location. A proxy of your own needs the same for `/ws`. The token is a short-lived access token (15 minutes by default); the sockets close when it expires.

## Rate limiting

A sliding window per client IP, in memory, per replica. `/api/v1` gets `RATE_LIMIT`; `POST /auth/login`, `POST /auth/register`, `GET /auth/oidc/login` and `GET /auth/oidc/callback` each get their own `LOGIN_RATE_LIMIT` budget on top. `POST /auth/refresh` has no dedicated limiter: only `RATE_LIMIT` applies, because several tabs and page loads refresh often. `/ws` uses `RATE_LIMIT` with auth on and no limiter with auth off (connections are long-lived). `/health/*` is not limited. A limited request gets 429 `rate limit exceeded`.

The username login throttle is separate and in memory per replica: 5 failed logins for one username in 15 minutes lock that username for 15 minutes (429 with `Retry-After`). With N replicas an attacker gets up to N times 5 guesses. The lock also stops the owner, which is a denial of service on the bootstrap admin: set a non-default `ADMIN_USERNAME` and a strong `ADMIN_PASSWORD`. The lock disappears at the next restart of the replica. Details: `auth-sessions.md`.

## Shutdown

`SIGINT` or `SIGTERM` starts `shutdown` (`backend/api/bootstrap.go`). Every step runs even when an earlier step failed:

0. Metrics and pprof servers (only the ones that run): stopped first, together within a fixed 2 s (`auxShutdownTimeout`), and closed when they are still busy (a pprof profile can run for 30 s). Prometheus sees the target go away at the start of the drain.
1. HTTP server: no new requests, wait for the open ones (own budget, `SERVER_SHUTDOWN_TIMEOUT`, which starts after step 0 ends). A server that does not finish is closed.
2. Audit recorder (auth on): no new entries, wait for the writes of finished requests. Own budget.
3. Leader worker group: stop the background workers. Own budget.
4. In-flight work (`inflight.Tracker`): work that outlives its request. Own budget.
5. WebSocket hub: close the client connections and end the revalidation loop.
6. Telemetry flush (`Telemetry.Shutdown`: spans, metrics and logs that are still buffered, sent or dropped within the budget). Own budget.
7. Rate limiters: stop their cleanup goroutines.
8. Session store (auth on): stop its cleanup goroutine, after everything that reads it.
9. Replica heartbeat (`REPLICA_HEARTBEAT_ENABLED=true`): stop it and delete this process's row, within a fixed 2 s (`heartbeatStopTimeout`).
10. Database: close the pool last.

With `LEADER_ELECTION_ENABLED=true` step 3 first stops the workers (the group can no longer start), then ends the election, which releases the lease (`expires_at` in the past where this process is the holder) so the next replica takes over at its next attempt; all of it within the step's budget. When a worker does not stop within the budget (it ignores its context), the election is NOT ended and the lease is NOT released: this process keeps renewing it while the database is open, so no other replica starts while that worker runs. After step 10 closes the database the renewals fail, and the lease expires 15 s after the last one, after the process has exited. When the workers stopped but the election itself does not end within the rest of the budget, the lease expires on its own after 15 s. The leader step never waits longer than its budget, so the worst case below is unchanged.

Steps 1, 2, 3, 4 and 6 each get their own budget of the length that was left on the shutdown context when shutdown began (at least 100 ms), detached from it, so a hung request cannot starve a later step and the 2 s of step 0 do not shorten the HTTP drain. That is five budgeted steps, so the worst case is about five times `SERVER_SHUTDOWN_TIMEOUT` plus the 2 s of step 0 and the 2 s of step 9 (154 s with the default 30 s). The stop grace period of the orchestrator must be longer: `docker-compose.yml` sets `stop_grace_period: 170s` for the default 30 s, which still covers it. Raise both together. Work that outlives its request starts with `inflight.Tracker.Go`; a periodic job is a `leader.Worker`.

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
- Per replica, in memory: the rate limiters, the username login throttle, the WebSocket hub and its connections. WebSocket broadcasts reach only the sockets of the replica that sent them.
- `SESSION_STORE=memory` is for one replica: a restart forgets every revocation and a second replica does not see any.
- Leader workers (`refresh-token-cleanup`, hourly, `session-store-cleanup`, every 15 minutes, and with the heartbeat on `replica-heartbeat-cleanup`, every 15 minutes) run in a `leader.Group`. With `LEADER_ELECTION_ENABLED=false` (the default) each process starts them itself; they only delete expired rows, so several replicas running them is safe. With `LEADER_ELECTION_ENABLED=true` they run on one replica at a time (below). A new worker must still be safe to run twice for a short time: see "Failure semantics".
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

With `REPLICA_HEARTBEAT_ENABLED=true` every process (leader or not) upserts its row in `replica_heartbeats` (`id` `varchar(253)` primary key, the process ID; `last_seen` `datetime(3)`, the database server's UTC clock, indexed) every 30 s, and deletes it on a clean shutdown (step 9). A failed write is logged once until the next success and does not stop the process. A row older than 2 minutes (`replica.StaleAfter`) belongs to a process that ended without a clean shutdown; the `replica-heartbeat-cleanup` leader worker deletes those rows. The table lists the running processes, for example `SELECT id, last_seen FROM replica_heartbeats ORDER BY last_seen DESC`.

## Health

`GET /health/live` always answers 200 with the uptime. `GET /health/ready` runs the registered checks (the database ping) and answers 503 when one fails. `GET /health` is the old plain check (`{"status":"ok"}`). The three are public and not rate limited. Register a check for every new external dependency with `healthChecker.AddCheck`. The Compose backend check is `wget -q -O /dev/null http://localhost:8081/health/live` (the Alpine image has `wget`, not `curl`).

## Logging

Structured text on stderr through `slog`, at `LOG_LEVEL`. With `OTEL_ENABLED=true` the records also go to the OTLP collector (see "Telemetry"). Failed logins log `reason`, the capped username, `client_ip` and `request_id` at Warn, never the password or hash. `RequestID` accepts a client `X-Request-ID` of 1 to 64 characters `[A-Za-z0-9._-]` and returns it; anything else is replaced. GORM errors and slow queries (200 ms or more) go through `slog`; with `APP_DEBUG=false` the SQL values are replaced with `?`. Keep `APP_DEBUG=false` in production.

## Audit log

With auth on, every successful `POST`, `PUT`, `PATCH` or `DELETE` of an authenticated user writes an entry (action, entity, user, route, status, client IP, request ID), after the response, on a context the client cannot cancel (30 s). Failed requests, reads, unauthenticated requests and everything under `/auth/` are not recorded: there is no audit entry for login, logout or a password change (there is no self-service password change; an admin resets it through `PUT /users/:id`, which is audited). At most 64 writes run at once; an entry over that bound is dropped with an Error log. `GET /api/v1/audit-logs` (admin session) pages with `limit` and `offset`, or with `next_cursor`; a cursor page has `total: -1`. There is no audit log page in the web UI.

## Swagger

With `ENABLE_SWAGGER=true` the UI is at `/swagger/index.html` and stays open even with `AUTH_ENABLED=true`: the spec lists every route. Keep it off in production. Regenerate the spec with `make docs`.

## Coverage, repeated tests, sample data and load tests

None of the variables in this section is read by the backend: they are Make and script variables, set on the command line (`make loadtest LOADTEST_VUS=20`) or in the environment.

### Coverage gate

`make test-coverage` runs the unit tests (`-short`, no database) with `-coverpkg` over the packages of `backend/coverage-packages.txt` and fails when any listed package has less than `COVERAGE_MIN` percent (default 80) on its own; an average would hide a weak package. `-coverpkg` credits a package for what any test binary runs, so the `api` main package is measured through its bootstrap tests (its `main()` is not testable). The list holds every package with non-test Go code added or changed since the backport began; the file says what is left out (the generated `docs`) and how it was derived. Add a package to the list in the change that adds it. Reports: `backend/coverage/coverage.html` and `coverage-summary.txt`. The logic is in `scripts/coverage-gate.sh`.

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

| Variable | Meaning |
|---|---|
| `E2E_AUTH` | `1` when the backend runs with `AUTH_ENABLED=true` (`make test-e2e-auth` sets it). `global-setup.ts` fails the run when this and the backend disagree. `auth.spec.ts` skips itself without it. |
| `A11Y_REQUIRE_AUTH` | `1` makes the accessibility spec fail, instead of skip, a page whose precondition is missing (Login, Admin Users). `make test-e2e-auth` sets it, so those pages are really checked. |
| `E2E_SECURE_COOKIES` | `false` (default) runs the backend with `SECURE_COOKIES=false` (cookie `refresh_token`); `true` checks the production cookie (`__Host-refresh_token`, `Secure`) in Chromium on `http://localhost`. CI runs both. |
| `E2E_ADMIN_USERNAME`, `E2E_ADMIN_PASSWORD`, `E2E_JWT_SECRET` | Test values of the throwaway backend, set by the Makefile. They are not defaults of the application and must never be used anywhere else. |
| `E2E_RETRIES` | Playwright retries; the default is 2 on CI and 0 otherwise. |
| `E2E_MYSQL_PORT`, `E2E_BACKEND_PORT`, `E2E_FRONTEND_PORT` | Host ports of the run (13306, 8081, 3000). A port in use is an error. |

`global-setup.ts` creates a non-admin account (`e2e-user`, random password per run) so that a test of a wrong password never counts toward the throttle of the administrator (5 failures lock a name, see `auth-sessions.md`). Keep every new failed-login test on that account, and do not raise `--repeat-each` beyond what the throttle allows. In CI, `e2e.yml` runs three passes: auth off, auth on, and auth on with `E2E_SECURE_COOKIES=true`.

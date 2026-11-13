---
applyTo: "**"
---

# Scalability Instructions

Details: `docs/claude/operations.md`. Rules: `CLAUDE.md` ("Scalability Rules").

- **Connection pool**: `DB_MAX_OPEN_CONNS` (25), `DB_MAX_IDLE_CONNS` (5), `DB_CONN_MAX_LIFETIME` (5m), applied in `internal/database/factory.go`. MySQL has its own `max_connections` (`backend/config/mysql/my.cnf`).
- **Replicas**: all data and revocations are shared through MySQL. The rate limiters, the username login throttle and the WebSocket hub are per replica and in memory. A background job must be safe on several replicas (there is no leader election yet).
- **Migrations** run at every start under a MySQL advisory lock that a replica waits for up to 5 minutes.
- **Pagination**: every list endpoint uses `ListPaged` with `limit` and `offset`, selects only the columns it needs and has an index that covers its filter and order (a new migration adds it).
- **Optimistic locking**: `Item` has `Version`; a stale update is 409. Other models use targeted or conditional writes.
- **Health**: register every external dependency with `healthChecker.AddCheck`. `/health/live` and `/health/ready` serve orchestrators.
- **Background work**: a periodic job is a `leader.Worker`; work that outlives its request starts with `inflight.Tracker.Go`.
- **Shutdown**: HTTP drain, audit drain, leader group, in-flight work, hub, telemetry, rate limiters, session store, database. The steps after HTTP have their own budgets, so the stop grace period must exceed five times `SERVER_SHUTDOWN_TIMEOUT` (Compose: 170 s).
- **Networks**: `backend-net` (db and backend) and `frontend-net` (backend and frontend). The frontend container must not reach the database.
- **Server timeouts**: `SERVER_READ_TIMEOUT` (10s), `SERVER_WRITE_TIMEOUT` (0, off: a server write timeout would end idle WebSockets), `SERVER_IDLE_TIMEOUT` (30s).

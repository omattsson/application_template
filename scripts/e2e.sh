#!/usr/bin/env bash
# Run the Playwright e2e suite against a fresh MySQL (Docker) and the backend
# built from this checkout. `make test-e2e` and `make test-e2e-auth` call it.
#
#   scripts/e2e.sh off  [spec files...]   backend with auth off
#   scripts/e2e.sh auth [spec files...]   backend with AUTH_ENABLED=true
#
# Steps: MySQL in Docker under its own Compose project (E2E_COMPOSE_PROJECT,
# own volumes, host port E2E_MYSQL_PORT), the backend natively on
# E2E_BACKEND_PORT, Playwright (which starts the Vite dev server on
# E2E_FRONTEND_PORT, proxying /api and /ws to the backend). On exit, also on a
# failure or Ctrl-C, the backend is stopped and the Compose project is removed
# with its volumes, so every run starts with an empty database. Nothing else
# is stopped: a port in use is an error, never a process to kill.
#
# Auth mode needs E2E_JWT_SECRET and E2E_ADMIN_PASSWORD (the Makefile passes
# test values; the backend has no default for either). E2E_SECURE_COOKIES
# (default false) is passed as SECURE_COOKIES.
#
# Auth mode also tests the password reset by mail: Mailpit (the same Compose
# project, docker-compose.mail.yml plus docker-compose.mail-e2e.yml, SMTP on
# host port E2E_MAILPIT_SMTP_PORT, web UI and API on E2E_MAILPIT_UI_PORT, both
# on 127.0.0.1) catches the mails, the backend runs with MAIL_ENABLED and
# PASSWORD_RESET_ENABLED, and a regular local user with an email address
# (E2E_RESET_USERNAME, E2E_RESET_EMAIL, a random password per run) is created
# through the API. Playwright gets MAILPIT_URL and E2E_RESET_PASSWORD. Auth off
# starts no Mailpit and leaves the module off.
#
# Logs: backend/tmp/e2e-backend-<mode>.log, Playwright report in
# frontend/playwright-report-<mode>.
set -euo pipefail

mode=${1:-}
case "$mode" in
  off | auth) shift ;;
  *)
    echo "usage: $0 off|auth [spec files...]" >&2
    exit 2
    ;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
project=${E2E_COMPOSE_PROJECT:-app-template-e2e}
mysql_port=${E2E_MYSQL_PORT:-13306}
backend_port=${E2E_BACKEND_PORT:-8081}
frontend_port=${E2E_FRONTEND_PORT:-3000}
db_name=${MYSQL_DATABASE:-app}
db_user=${MYSQL_USER:-appuser}
db_password=${MYSQL_PASSWORD:-apppass}
backend_bin="$root/backend/tmp/e2e-main"
backend_log="$root/backend/tmp/e2e-backend-$mode.log"
backend_pid=""
mailpit_smtp_port=${E2E_MAILPIT_SMTP_PORT:-11025}
mailpit_ui_port=${E2E_MAILPIT_UI_PORT:-18025}
mailpit_url="http://127.0.0.1:$mailpit_ui_port"
# The seeded user of the password reset specs. DEV ONLY: the e2e backend only.
reset_username=${E2E_RESET_USERNAME:-e2e-reset-user}
reset_email=${E2E_RESET_EMAIL:-e2e-reset-user@example.test}
compose_files=(-f "$root/docker-compose.yml")
if [ "$mode" = auth ]; then
  compose_files+=(-f "$root/docker-compose.mail.yml" -f "$root/docker-compose.mail-e2e.yml")
fi

# The shell environment wins over a developer's root .env, so Compose and the
# backend below use the same database name, user and password.
compose() {
  MYSQL_PORT=$mysql_port MYSQL_DATABASE=$db_name MYSQL_USER=$db_user MYSQL_PASSWORD=$db_password \
    MAILPIT_UI_PORT=$mailpit_ui_port E2E_MAILPIT_SMTP_PORT=$mailpit_smtp_port \
    docker compose -p "$project" "${compose_files[@]}" "$@"
}

port_in_use() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [ -n "$backend_pid" ] && kill -0 "$backend_pid" 2>/dev/null; then
    echo "Stopping backend (pid $backend_pid)..."
    kill "$backend_pid" 2>/dev/null || true
    for _ in $(seq 1 30); do
      kill -0 "$backend_pid" 2>/dev/null || break
      sleep 1
    done
    kill -9 "$backend_pid" 2>/dev/null || true
  fi
  echo "Removing the e2e database (Compose project $project)..."
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  exit "$status"
}

ports=("$mysql_port" "$backend_port" "$frontend_port")
if [ "$mode" = auth ]; then
  ports+=("$mailpit_smtp_port" "$mailpit_ui_port")
fi
for port in "${ports[@]}"; do
  if port_in_use "$port"; then
    echo "ERROR: port $port is in use. Stop that process or set E2E_MYSQL_PORT, E2E_BACKEND_PORT, E2E_FRONTEND_PORT, E2E_MAILPIT_SMTP_PORT or E2E_MAILPIT_UI_PORT." >&2
    lsof -nP -iTCP:"$port" -sTCP:LISTEN >&2 || true
    exit 1
  fi
done

backend_env=(
  # The e2e run never reads a developer's backend/.env.
  "ENV_FILE=$root/backend/tmp/e2e-no-env-file"
  GO_ENV=test APP_DEBUG=false LOG_LEVEL=info
  DB_HOST=127.0.0.1 "DB_PORT=$mysql_port" "DB_NAME=$db_name" "DB_USER=$db_user" "DB_PASSWORD=$db_password"
  SERVER_HOST=127.0.0.1 "SERVER_PORT=$backend_port"
  # Every page, setup call and sign-in of the suite comes from 127.0.0.1 (the
  # backend trusts no proxy here), so the production limits (100 requests and
  # 10 logins per minute per IP) would throttle the run. Test values only.
  RATE_LIMIT=10000 LOGIN_RATE_LIMIT=1000
)
# 127.0.0.1, not localhost: the backend listens on IPv4 only, and Node may
# resolve localhost to ::1 first.
playwright_env=("E2E_BACKEND_URL=http://127.0.0.1:$backend_port" "VITE_API_URL=http://127.0.0.1:$backend_port"
  "E2E_FRONTEND_PORT=$frontend_port" E2E_REUSE_SERVER=0
  "PLAYWRIGHT_HTML_OUTPUT_DIR=playwright-report-$mode")

if [ "$mode" = auth ]; then
  : "${E2E_JWT_SECRET:?E2E_JWT_SECRET is required in auth mode (make test-e2e-auth sets a test value)}"
  : "${E2E_ADMIN_PASSWORD:?E2E_ADMIN_PASSWORD is required in auth mode (make test-e2e-auth sets a test value)}"
  admin_username=${E2E_ADMIN_USERNAME:-admin}
  secure_cookies=${E2E_SECURE_COOKIES:-false}
  backend_env+=(AUTH_ENABLED=true "JWT_SECRET=$E2E_JWT_SECRET" "ADMIN_USERNAME=$admin_username"
    "ADMIN_PASSWORD=$E2E_ADMIN_PASSWORD" SELF_REGISTRATION=false OIDC_ENABLED=false
    "SECURE_COOKIES=$secure_cookies" SESSION_STORE=mysql)
  # Password reset by mail. Mailpit takes clear text SMTP without credentials.
  # The link opens the dev server at the origin Playwright uses (baseURL). The
  # one-second minimum interval keeps two requests of a spec from being throttled.
  backend_env+=(MAIL_ENABLED=true SMTP_HOST=127.0.0.1 "SMTP_PORT=$mailpit_smtp_port" SMTP_TLS=none
    "MAIL_FROM=App <noreply@example.test>" PASSWORD_RESET_ENABLED=true
    "PASSWORD_RESET_URL=http://localhost:$frontend_port" PASSWORD_RESET_MIN_INTERVAL=1s)
  reset_password=$(openssl rand -hex 16)
  # A11Y_REQUIRE_AUTH=1: the Login and Admin Users accessibility checks must
  # run (a missing precondition fails instead of skipping).
  playwright_env+=(E2E_AUTH=1 A11Y_REQUIRE_AUTH=1 "ADMIN_USERNAME=$admin_username"
    "ADMIN_PASSWORD=$E2E_ADMIN_PASSWORD" "E2E_SECURE_COOKIES=$secure_cookies"
    "MAILPIT_URL=$mailpit_url" "E2E_RESET_USERNAME=$reset_username" "E2E_RESET_EMAIL=$reset_email"
    "E2E_RESET_PASSWORD=$reset_password")
else
  backend_env+=(AUTH_ENABLED=false)
fi

# Create the regular local user of the password reset specs (role user, with an
# email address; the bootstrap administrator is a service account and is not
# eligible for a reset). Signs in as the administrator, creates the user, signs
# out. The database is fresh, so a 409 is an error.
seed_reset_user() {
  local api="http://127.0.0.1:$backend_port/api/v1" login token code
  login=$(curl -sf -X POST "$api/auth/login" -H 'X-Requested-With: XMLHttpRequest' \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$admin_username\",\"password\":\"$E2E_ADMIN_PASSWORD\"}") || {
    echo "ERROR: could not sign in as $admin_username to seed $reset_username." >&2
    exit 1
  }
  token=$(printf '%s' "$login" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  if [ -z "$token" ]; then
    echo "ERROR: the login answer of $admin_username has no token." >&2
    exit 1
  fi
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$api/users" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$reset_username\",\"password\":\"$reset_password\",\"display_name\":\"E2E Reset User\",\"email\":\"$reset_email\",\"role\":\"user\"}")
  if [ "$code" != 201 ]; then
    echo "ERROR: creating $reset_username answered $code, want 201." >&2
    exit 1
  fi
  curl -s -o /dev/null -X POST "$api/auth/logout" -H "Authorization: Bearer $token" \
    -H 'X-Requested-With: XMLHttpRequest' -H 'Content-Type: application/json' -d '{}' || true
}

trap cleanup EXIT INT TERM

echo "Starting MySQL (Compose project $project, host port $mysql_port)..."
compose down -v --remove-orphans >/dev/null 2>&1 || true
compose up -d db
echo "Waiting for MySQL to accept TCP connections..."
# TCP, not the socket: during its first start the image runs a temporary
# server without networking, which a socket ping would already reach.
for i in $(seq 1 90); do
  if compose exec -T db mysql --protocol=TCP -h127.0.0.1 "-u$db_user" "-p$db_password" -e 'SELECT 1' "$db_name" >/dev/null 2>&1; then
    break
  fi
  if [ "$i" = 90 ]; then
    echo "ERROR: MySQL did not start in 180s." >&2
    compose logs db | tail -30 >&2
    exit 1
  fi
  sleep 2
done

if [ "$mode" = auth ]; then
  echo "Starting Mailpit (SMTP 127.0.0.1:$mailpit_smtp_port, web UI and API $mailpit_url)..."
  compose up -d mailpit
  for i in $(seq 1 60); do
    if curl -sf "$mailpit_url/readyz" >/dev/null 2>&1; then
      break
    fi
    if [ "$i" = 60 ]; then
      echo "ERROR: Mailpit did not become ready in 60s." >&2
      compose logs mailpit | tail -30 >&2
      exit 1
    fi
    sleep 1
  done
  # A fresh container is empty already; this keeps a reused one honest.
  curl -sf -X DELETE "$mailpit_url/api/v1/messages" >/dev/null
fi

echo "Building the backend..."
mkdir -p "$root/backend/tmp"
(cd "$root/backend" && go build -o "$backend_bin" ./api)

echo "Starting the backend ($mode) on port $backend_port (log: $backend_log)..."
# exec: $! is the backend itself, so cleanup stops the right process.
(cd "$root/backend" && exec env "${backend_env[@]}" "$backend_bin" >"$backend_log" 2>&1) &
backend_pid=$!
for i in $(seq 1 60); do
  if curl -sf "http://127.0.0.1:$backend_port/health/ready" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$backend_pid" 2>/dev/null || [ "$i" = 60 ]; then
    echo "ERROR: the backend did not become ready. Last lines of $backend_log:" >&2
    tail -30 "$backend_log" >&2
    exit 1
  fi
  sleep 1
done

if [ "$mode" = auth ]; then
  seed_reset_user
fi

echo "Backend is ready, running Playwright ($mode): ${*:-all specs}"
cd "$root/frontend"
env "${playwright_env[@]}" npx playwright test "$@"

#!/usr/bin/env bash
# Seeds a running backend with sample Items through its API.
# Needs a running backend (make dev or make dev-backend).
#
# Usage: scripts/seed-dev-data.sh        (make seed)
#
# Environment (script variables, not backend settings):
#   API_URL         backend base URL (default http://localhost:8081)
#   SEED_COUNT      number of sample items (default 10)
#   API_TOKEN       a JWT access token, for a backend with AUTH_ENABLED=true
#   ADMIN_USERNAME  with ADMIN_PASSWORD: log in for the token (default admin)
#   ADMIN_PASSWORD  no default; the script never prints it
# With auth off (the default) no credential is needed.
#
# Safe to run again: the items are named "Sample Item NN"; an item whose exact
# name exists is left alone (and so is its price), so a second run creates
# nothing. The script only creates items: it never updates or deletes anything.
# It talks to the URL you give it, so point it at a backend you want data in.
#
# Exit codes: 0 done, 1 an API call failed, 2 usage or configuration error.
set -euo pipefail

API_URL=${API_URL:-http://localhost:8081}
API_URL=${API_URL%/}
SEED_COUNT=${SEED_COUNT:-10}

case "$SEED_COUNT" in
  '' | *[!0-9]* | 0)
    echo "ERROR: SEED_COUNT must be a positive integer, got '$SEED_COUNT'" >&2
    exit 2
    ;;
esac
if [ "$#" -gt 0 ]; then
  echo "usage: scripts/seed-dev-data.sh (configure it with API_URL, SEED_COUNT, API_TOKEN, ADMIN_USERNAME, ADMIN_PASSWORD)" >&2
  exit 2
fi
command -v curl >/dev/null 2>&1 || {
  echo "ERROR: curl is required" >&2
  exit 2
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
token=${API_TOKEN:-}

# api METHOD PATH [BODY]: prints the body, leaves the status in $tmp/status.
# The credential goes in a curl config file read from stdin, so it is never on
# a command line (visible in ps).
api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS --max-time 30 -X "$method" "$API_URL$path" -o "$tmp/body" -w '%{http_code}' -K -)
  local cfg=''
  [ -n "$token" ] && cfg="header = \"Authorization: Bearer $token\""
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  # A 429 (RATE_LIMIT is 100 requests per minute per IP by default) is retried
  # after 5, 10, ... 25 s, up to 6 attempts.
  local status attempt
  for attempt in 1 2 3 4 5 6; do
    rm -f "$tmp/body"
    status=$(printf '%s\n' "$cfg" | curl "${args[@]}") || status=000
    [ "$status" = 429 ] && [ "$attempt" -lt 6 ] || break
    echo "rate limited on $method $path: retrying in $((attempt * 5))s" >&2
    sleep $((attempt * 5))
  done
  echo "$status" >"$tmp/status"
  [ -f "$tmp/body" ] && cat "$tmp/body"
  return 0
}
status() { cat "$tmp/status"; }

# Is the backend there? Is auth on?
cfg_body=$(api GET /api/v1/ui-config)
if [ "$(status)" != 200 ]; then
  echo "ERROR: no backend at $API_URL (GET /api/v1/ui-config answered $(status)). Start it with make dev, or set API_URL." >&2
  exit 1
fi
auth_enabled=false
case "$cfg_body" in *'"auth_enabled":true'*) auth_enabled=true ;; esac

if [ "$auth_enabled" = true ] && [ -z "$token" ]; then
  if [ -z "${ADMIN_PASSWORD:-}" ]; then
    echo "ERROR: the backend has AUTH_ENABLED=true. Set API_TOKEN, or ADMIN_PASSWORD (and ADMIN_USERNAME if it is not admin)." >&2
    exit 2
  fi
  # X-Requested-With is the CSRF header the login needs (any value). The JSON
  # is built here from the two variables; a quote or backslash in either is
  # escaped.
  json_escape() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }
  login_body=$(printf '{"username":"%s","password":"%s"}' \
    "$(json_escape "${ADMIN_USERNAME:-admin}")" "$(json_escape "$ADMIN_PASSWORD")")
  login_resp=$(curl -sS --max-time 30 -X POST "$API_URL/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-Requested-With: seed' \
    -d @- -o "$tmp/login" -w '%{http_code}' <<<"$login_body") || login_resp=000
  if [ "$login_resp" != 200 ]; then
    echo "ERROR: login failed (status $login_resp)" >&2
    exit 1
  fi
  token=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$tmp/login")
  if [ -z "$token" ]; then
    echo "ERROR: the login answer holds no token" >&2
    exit 1
  fi
fi

created=0
existing=0
failed=0
for i in $(seq 1 "$SEED_COUNT"); do
  name=$(printf 'Sample Item %02d' "$i")
  # Names have no URL-special character except the space.
  found=$(api GET "/api/v1/items?name_exact=${name// /%20}&limit=1")
  case "$(status)" in
    200) ;;
    *)
      echo "FAIL: looking up '$name' answered $(status)" >&2
      failed=$((failed + 1))
      continue
      ;;
  esac
  if [ "$found" != '[]' ]; then
    existing=$((existing + 1))
    continue
  fi
  price="$((i * 5)).99"
  api POST /api/v1/items "{\"name\":\"$name\",\"price\":$price}" >/dev/null
  if [ "$(status)" = 201 ]; then
    created=$((created + 1))
  else
    echo "FAIL: creating '$name' answered $(status)" >&2
    failed=$((failed + 1))
  fi
done

echo "Seed done: $created created, $existing already present, $failed failed ($API_URL)"
[ "$failed" -eq 0 ]

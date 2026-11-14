#!/usr/bin/env bash
# Refuses to start the production Compose targets with the placeholder
# database credentials of docker-compose.yml (MYSQL_PASSWORD=apppass,
# MYSQL_ROOT_PASSWORD=rootpass), which are published knowledge. The values are
# read from "docker compose config", so they are the ones Compose will use:
# the environment, the .env file next to docker-compose.yml, or the default.
#
# Set your own values in the environment or in .env, or, knowingly (a throwaway
# local check of the production images), ALLOW_DEFAULT_CREDENTIALS=1.
#
# Usage:
#   scripts/check-prod-credentials.sh              check the Compose configuration
#   scripts/check-prod-credentials.sh --self-test  run against fixture configurations
# Override for the self-test: CHECK_PROD_CREDENTIALS_CONFIG (a file holding the
# output of "docker compose config", used instead of running it).
# Exit codes: 0 allowed, 1 refused, 2 usage or Compose error.
set -euo pipefail

DEFAULT_PASSWORD=apppass
DEFAULT_ROOT_PASSWORD=rootpass

# value_of KEY CONFIG prints the value of an environment entry (KEY: value) of
# the rendered Compose configuration, without YAML quotes.
value_of() {
  printf '%s\n' "$2" |
    sed -n "s/^ *$1: *//p" |
    head -n 1 |
    sed -e "s/^'\(.*\)'\$/\1/" -e 's/^"\(.*\)"$/\1/'
}

# check CONFIG returns 0 when no default credential is in use.
check() {
  local config=$1 password root bad=0
  password=$(value_of MYSQL_PASSWORD "$config")
  root=$(value_of MYSQL_ROOT_PASSWORD "$config")
  if [ -z "$password" ] || [ "$password" = "$DEFAULT_PASSWORD" ]; then
    echo "ERROR: MYSQL_PASSWORD is unset or the placeholder '$DEFAULT_PASSWORD'." >&2
    bad=1
  fi
  if [ -z "$root" ] || [ "$root" = "$DEFAULT_ROOT_PASSWORD" ]; then
    echo "ERROR: MYSQL_ROOT_PASSWORD is unset or the placeholder '$DEFAULT_ROOT_PASSWORD'." >&2
    bad=1
  fi
  return "$bad"
}

refuse_message() {
  cat >&2 <<'MSG'

Refusing to start the production services with the placeholder database
credentials from docker-compose.yml: they are public. Set your own values in
the environment or in a .env file next to docker-compose.yml, for example
  MYSQL_PASSWORD=<long random value>
  MYSQL_ROOT_PASSWORD=<another long random value>
(an existing database volume keeps the password it was created with: change
it in MySQL as well, or start with an empty volume).
Throwaway local check of the production images only: ALLOW_DEFAULT_CREDENTIALS=1 make prod
MSG
}

self_test() {
  local failures=0 name expect config rc
  t() { # name expected-rc config
    name=$1 expect=$2 config=$3
    rc=0
    check "$config" >/dev/null 2>&1 || rc=$?
    if [ "$rc" -eq "$expect" ]; then
      echo "PASS: $name"
    else
      echo "FAIL: $name (rc=$rc, want $expect)" >&2
      failures=$((failures + 1))
    fi
  }
  t "both defaults are refused" 1 $'      MYSQL_PASSWORD: apppass\n      MYSQL_ROOT_PASSWORD: rootpass'
  t "default password alone is refused" 1 $'      MYSQL_PASSWORD: apppass\n      MYSQL_ROOT_PASSWORD: s3cret-root'
  t "default root password alone is refused" 1 $'      MYSQL_PASSWORD: s3cret\n      MYSQL_ROOT_PASSWORD: rootpass'
  t "quoted default is refused" 1 $'      MYSQL_PASSWORD: "apppass"\n      MYSQL_ROOT_PASSWORD: \'rootpass\''
  t "missing values are refused" 1 $'      MYSQL_USER: appuser'
  t "own values are allowed" 0 $'      MYSQL_PASSWORD: s3cret\n      MYSQL_ROOT_PASSWORD: s3cret-root'
  t "a value with YAML quoting is allowed" 0 $'      MYSQL_PASSWORD: \'a: b "c"\'\n      MYSQL_ROOT_PASSWORD: s3cret-root'
  t "a command line mentioning the variable is not a value" 1 $'        - mysqladmin ping --password=$$MYSQL_PASSWORD\n      MYSQL_ROOT_PASSWORD: s3cret-root'
  if [ "$failures" -gt 0 ]; then
    echo "check-prod-credentials: $failures self-test failure(s)" >&2
    return 1
  fi
  echo "check-prod-credentials: self-test ok"
}

main() {
  if [ "${1:-}" = "--self-test" ]; then
    self_test
    return
  fi
  if [ $# -gt 0 ]; then
    echo "usage: $0 [--self-test]" >&2
    return 2
  fi
  if [ "${ALLOW_DEFAULT_CREDENTIALS:-}" = "1" ]; then
    echo "WARNING: ALLOW_DEFAULT_CREDENTIALS=1: the production services may use the placeholder database credentials." >&2
    return 0
  fi
  local config
  if [ -n "${CHECK_PROD_CREDENTIALS_CONFIG:-}" ]; then
    config=$(cat "$CHECK_PROD_CREDENTIALS_CONFIG")
  else
    cd "$(dirname "${BASH_SOURCE[0]}")/.."
    if ! config=$(docker compose config 2>&1); then
      printf '%s\n' "$config" >&2
      echo "ERROR: 'docker compose config' failed, cannot check the database credentials." >&2
      return 2
    fi
  fi
  if ! check "$config"; then
    refuse_message
    return 1
  fi
}

main "$@"

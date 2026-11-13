#!/usr/bin/env bash
# scripts/check-env-docs.sh
# Keeps the environment variable documentation in step with the code.
#
# Every environment variable that the backend config package reads must be
# documented, and nothing may be documented that the code does not read:
#
#   1. Names are every string literal in backend/internal/config/*.go (test
#      files excluded) that is a bare upper-case identifier, for example
#      "JWT_SECRET". That covers getEnv*, parseEnv*Strict and os.Getenv calls
#      and the variable names held in tables (the auth duration table), so a
#      new accessor cannot hide a variable.
#   2. Each name must appear in backend/.env.example as NAME= or # NAME=.
#   3. Each name must appear in backend/config/.env.example the same way, and
#      the two example files must be equal except for the DB_HOST line.
#   4. Each name must appear in docs/claude/operations.md (as a whole word).
#   5. Every NAME= or # NAME= line of backend/.env.example must be a name the
#      code reads (a variable that no code reads is dead documentation).
#
# EXCLUDED lists upper-case literals that are not environment variables. Each
# entry needs a reason; the list is empty because the config package has no
# such literal today. Format: "NAME|reason".
EXCLUDED=(
)
#
# Usage:
#   scripts/check-env-docs.sh              check the repository
#   scripts/check-env-docs.sh --self-test  run the built-in fixture tests
# Override for tests: CONFIG_DIR, ENV_EXAMPLE, ENV_EXAMPLE_COPY, OPS_DOC.
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# is_excluded NAME -> 0 if the name is in EXCLUDED.
is_excluded() {
  local entry
  for entry in "${EXCLUDED[@]+"${EXCLUDED[@]}"}"; do
    [ "${entry%%|*}" = "$1" ] && return 0
  done
  return 1
}

# code_names CONFIG_DIR -> sorted unique variable names, one per line.
code_names() {
  local dir="$1" f name
  for f in "$dir"/*.go; do
    [ -f "$f" ] || continue
    case "$f" in *_test.go) continue ;; esac
    grep -oE '"[A-Z][A-Z0-9_]+"' "$f" | tr -d '"'
  done | sort -u | while IFS= read -r name; do
    is_excluded "$name" || printf '%s\n' "$name"
  done
}

# example_has FILE NAME -> 0 if FILE has a line NAME= or # NAME=.
example_has() {
  grep -qE "^[[:space:]]*#?[[:space:]]*$2=" "$1"
}

# run_checks CONFIG_DIR ENV_EXAMPLE ENV_EXAMPLE_COPY OPS_DOC -> prints one line
# per problem; returns 1 if any.
run_checks() {
  local dir="$1" example="$2" copy="$3" ops="$4" rc=0 name names f

  for f in "$example" "$copy" "$ops"; do
    if [ ! -f "$f" ]; then
      echo "$f: file not found"
      return 1
    fi
  done

  names="$(code_names "$dir")"
  if [ -z "$names" ]; then
    echo "$dir: no environment variable names found (wrong directory?)"
    return 1
  fi

  while IFS= read -r name; do
    example_has "$example" "$name" || { echo "$example: $name is read by the config package but not documented"; rc=1; }
    example_has "$copy" "$name" || { echo "$copy: $name is read by the config package but not documented"; rc=1; }
    grep -qw -- "$name" "$ops" || { echo "$ops: $name is read by the config package but not documented"; rc=1; }
  done <<EOF
$names
EOF

  # Rule 3: the copy equals the main example apart from DB_HOST.
  if ! diff <(grep -v '^DB_HOST=' "$example") <(grep -v '^DB_HOST=' "$copy") >/dev/null; then
    echo "$copy: differs from $example (only the DB_HOST line may differ)"
    rc=1
  fi

  # Rule 5: no documented variable that the code does not read.
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    if ! printf '%s\n' "$names" | grep -qx -- "$name"; then
      echo "$example: $name is documented but no config code reads it"
      rc=1
    fi
  done < <(sed -nE 's/^[[:space:]]*#?[[:space:]]*([A-Z][A-Z0-9_]+)=.*/\1/p' "$example" | sort -u)

  return "$rc"
}

self_test() {
  local tmp failed=0
  tmp="$(mktemp -d)" || return 1

  # mk_fixture NAME: a passing fixture under $tmp/NAME.
  mk_fixture() {
    local d="$tmp/$1"
    mkdir -p "$d/config"
    cat >"$d/config/config.go" <<'GO'
package config

func load() {
	a := getEnv("APP_NAME", "x")
	h := getEnv("DB_HOST", "localhost")
	b := parseEnvBoolStrict("AUTH_ENABLED", false)
	table := []struct{ key string }{{"JWT_EXPIRATION"}}
	_ = os.Getenv("GIN_MODE")
}
GO
    printf 'package config\n\nfunc TestX() { _ = "ONLY_IN_TEST" }\n' >"$d/config/config_test.go"
    printf 'APP_NAME=x\n# AUTH_ENABLED=false\n# JWT_EXPIRATION=24h\n  # GIN_MODE=release\nDB_HOST=db\n' >"$d/example"
    printf 'APP_NAME=x\n# AUTH_ENABLED=false\n# JWT_EXPIRATION=24h\n  # GIN_MODE=release\nDB_HOST=localhost\n' >"$d/copy"
    # The backticks are literal (a name in code formatting counts).
    # shellcheck disable=SC2016
    printf 'APP_NAME AUTH_ENABLED `JWT_EXPIRATION` GIN_MODE DB_HOST\n' >"$d/ops.md"
  }

  # expect NAME pass|fail
  expect() {
    local d="$tmp/$1" rc out
    out="$(run_checks "$d/config" "$d/example" "$d/copy" "$d/ops.md" 2>&1)"
    rc=$?
    if { [ "$2" = pass ] && [ "$rc" -eq 0 ]; } || { [ "$2" = fail ] && [ "$rc" -eq 1 ]; }; then
      echo "PASS: $1 ($2)"
      # Show the problem lines of a fixture that must fail (fixture paths cut).
      [ "$2" = fail ] && printf '%s\n' "$out" | sed "s|$tmp/||; s|^|      |"
    else
      echo "FAIL: $1 expected $2, got rc=$rc"
      [ -n "$out" ] && echo "$out"
      failed=$((failed + 1))
    fi
  }

  mk_fixture good
  expect good pass

  mk_fixture missing-in-example
  sed -i.bak '/APP_NAME/d' "$tmp/missing-in-example/example"
  expect missing-in-example fail

  mk_fixture missing-in-copy
  sed -i.bak '/AUTH_ENABLED/d' "$tmp/missing-in-copy/copy"
  expect missing-in-copy fail

  mk_fixture missing-in-ops
  printf 'APP_NAME AUTH_ENABLED GIN_MODE DB_HOST\n' >"$tmp/missing-in-ops/ops.md"
  expect missing-in-ops fail

  # APP_NAME_EXTRA in the doc must not count for APP_NAME (whole words only).
  mk_fixture substring-in-ops
  printf 'APP_NAME_EXTRA AUTH_ENABLED JWT_EXPIRATION GIN_MODE DB_HOST\n' >"$tmp/substring-in-ops/ops.md"
  expect substring-in-ops fail

  # A variable in a table (not a call) is found too.
  mk_fixture table-var
  sed -i.bak '/JWT_EXPIRATION/d' "$tmp/table-var/example" "$tmp/table-var/copy"
  expect table-var fail

  mk_fixture dead-var
  printf '# API_PREFIX=/api\n' | tee -a "$tmp/dead-var/example" >>"$tmp/dead-var/copy"
  expect dead-var fail

  mk_fixture copy-drift
  printf 'EXTRA_COMMENT_ONLY text\n' >>"$tmp/copy-drift/copy"
  expect copy-drift fail

  # A difference in the DB_HOST line alone is fine (good fixture already has it).
  # A name only in a test file is not a variable of the code.
  mk_fixture test-file-literal
  expect test-file-literal pass

  # EXCLUDED entries are honoured: a literal that is no variable.
  mk_fixture excluded
  printf 'package config\n\nvar kind = "NOT_AN_ENV"\n' >"$tmp/excluded/config/other.go"
  EXCLUDED=("NOT_AN_ENV|fixture: a label, not a variable")
  expect excluded pass
  EXCLUDED=()
  expect excluded fail

  mk_fixture no-names
  rm "$tmp/no-names/config/config.go"
  expect no-names fail

  rm -rf "$tmp"
  if [ "$failed" -ne 0 ]; then
    echo "$failed self-test(s) failed"
    return 1
  fi
  echo "self-test OK"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

if [ "$#" -gt 0 ]; then
  echo "usage: scripts/check-env-docs.sh [--self-test]" >&2
  exit 2
fi

if run_checks "${CONFIG_DIR:-$REPO_ROOT/backend/internal/config}" \
  "${ENV_EXAMPLE:-$REPO_ROOT/backend/.env.example}" \
  "${ENV_EXAMPLE_COPY:-$REPO_ROOT/backend/config/.env.example}" \
  "${OPS_DOC:-$REPO_ROOT/docs/claude/operations.md}"; then
  echo "env docs OK"
else
  exit 1
fi

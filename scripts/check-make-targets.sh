#!/usr/bin/env bash
# Checks that the ops targets of the Makefile exist and can be planned.
#
# For each target of TARGETS (root Makefile) this script asserts that
#   1. the target is defined (make -qp lists a rule for it),
#   2. it is declared .PHONY,
#   3. "make -n <target>" succeeds (it prints the recipe and runs nothing), and
#   4. that output is not empty: a target with no recipe (for example only a
#      ".PHONY: seed" line, or a bare "seed:" rule) makes make say "Nothing to
#      be done", which counts as empty.
# It also asserts that "make -n loadtest" succeeds when no k6 binary exists
# (the check for k6 belongs in the recipe, never in the parse phase) and that
# backend/Makefile defines test-coverage.
#
# Usage:
#   scripts/check-make-targets.sh              check the repository
#   scripts/check-make-targets.sh --self-test  run the checker against fixture
#                                              Makefiles (one good, several broken)
# Override for the self-test: MAKE_TARGETS_ROOT (a directory with Makefile and
# backend/Makefile; default: the repository).
# Exit codes: 0 all checks pass, 1 at least one check failed (each failure is
# printed as "FAIL: ..."), 2 usage error.
set -euo pipefail

TARGETS="loadtest loadtest-stress test-coverage test-backend-repeat seed"

check_repo() {
  local root=$1 failures=0 target rules phony backend_rules out
  fail() {
    echo "FAIL: $*" >&2
    failures=$((failures + 1))
  }
  cd "$root"

  # make -qp prints the rule database without running anything. A target that
  # has a rule appears at the start of a line, followed by a colon.
  rules=$(make -qp 2>/dev/null || true)
  phony=$(printf '%s\n' "$rules" | sed -n 's/^\.PHONY: //p' | tr ' ' '\n')

  for target in $TARGETS; do
    if ! printf '%s\n' "$rules" | grep -Eq "^${target}:"; then
      fail "make target '$target' is not defined in the Makefile"
      continue
    fi
    if ! printf '%s\n' "$phony" | grep -qx "$target"; then
      fail "make target '$target' is not listed in .PHONY"
    fi
    if ! out=$(make -n "$target" 2>&1); then
      fail "'make -n $target' failed:"
      printf '%s\n' "$out" >&2
    elif ! plan_has_content "$out"; then
      fail "'make -n $target' prints no recipe (the target has no commands)"
    fi
  done

  # No k6 on the machine must not break the plan of the load test.
  if ! out=$(make -n loadtest K6=/nonexistent/k6 2>&1); then
    fail "'make -n loadtest' fails when k6 is not installed (check for k6 in the recipe only):"
    printf '%s\n' "$out" >&2
  fi

  # The backend Makefile has its own coverage target (the root target calls it).
  backend_rules=$(make -C backend -qp 2>/dev/null || true)
  if ! printf '%s\n' "$backend_rules" | grep -Eq '^test-coverage:'; then
    fail "backend/Makefile does not define test-coverage"
  fi

  if [ "$failures" -gt 0 ]; then
    echo "check-make-targets: $failures check(s) failed" >&2
    return 1
  fi
  echo "check-make-targets: all targets present and plannable ($TARGETS)"
}

# plan_has_content succeeds when the output of "make -n" holds a line that is
# not one of make's "nothing to do" messages.
plan_has_content() {
  printf '%s\n' "$1" |
    grep -Ev "^make(\[[0-9]+\])?: (Nothing to be done for|.* is up to date)" |
    grep -q '[^[:space:]]'
}

# write_fixture DIR [omit TARGET | empty TARGET | phony-only TARGET | k6-parse | no-backend-coverage]
# writes DIR/Makefile and DIR/backend/Makefile. The default fixture is good:
# every target has a recipe and is .PHONY.
write_fixture() {
  local dir=$1 mode=${2:-} broken=${3:-} t
  mkdir -p "$dir/backend"
  {
    echo ".PHONY: $TARGETS"
    for t in $TARGETS; do
      if [ "$mode" = omit ] && [ "$broken" = "$t" ]; then
        continue
      elif [ "$mode" = empty ] && [ "$broken" = "$t" ]; then
        echo "$t:"
      elif [ "$mode" = phony-only ] && [ "$broken" = "$t" ]; then
        continue
      else
        printf '%s:\n\t@echo run %s\n' "$t" "$t"
      fi
    done
    if [ "$mode" = k6-parse ]; then
      # k6 looked up while make reads the Makefile: make -n would fail.
      echo 'K6 ?= k6'
      # shellcheck disable=SC2016  # make syntax, not a shell expansion
      echo '$(if $(shell command -v $(K6)),,$(error k6 not found))'
    fi
  } >"$dir/Makefile"
  if [ "$mode" = no-backend-coverage ]; then
    printf 'build:\n\t@echo build\n' >"$dir/backend/Makefile"
  else
    printf '.PHONY: test-coverage\ntest-coverage:\n\t@echo cover\n' >"$dir/backend/Makefile"
  fi
}

self_test() {
  local failed=0 name want dir rc args
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT

  # name|expected exit code|write_fixture arguments
  while IFS='|' read -r name want args; do
    [ -n "$name" ] || continue
    dir="$tmp/$name"
    # args is split on purpose: it is "mode target".
    # shellcheck disable=SC2086
    write_fixture "$dir" $args
    rc=0
    MAKE_TARGETS_ROOT=$dir bash "$0" >"$tmp/$name.out" 2>&1 || rc=$?
    if [ "$rc" = "$want" ]; then
      echo "PASS: $name (exit $rc)"
    else
      echo "FAIL: $name: want exit $want, got $rc" >&2
      sed 's/^/      /' "$tmp/$name.out" >&2
      failed=$((failed + 1))
    fi
  done <<'EOF'
good|0|
missing-target|1|omit seed
empty-recipe|1|empty seed
phony-only-no-rule|1|phony-only seed
k6-checked-at-parse-time|1|k6-parse
no-backend-coverage|1|no-backend-coverage
EOF

  if [ "$failed" -gt 0 ]; then
    echo "$failed self-test(s) failed" >&2
    return 1
  fi
  echo "self-test OK"
}

case "${1:-}" in
  --self-test)
    self_test
    ;;
  '')
    check_repo "${MAKE_TARGETS_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
    ;;
  *)
    echo "usage: scripts/check-make-targets.sh [--self-test]" >&2
    exit 2
    ;;
esac

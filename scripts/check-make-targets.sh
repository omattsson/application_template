#!/usr/bin/env bash
# Checks that the ops targets of the Makefile exist and can be planned.
#
# For each target of TARGETS (root Makefile) this script asserts that
#   1. the target is defined (make -qp lists a rule for it),
#   2. it is declared .PHONY, and
#   3. "make -n <target>" succeeds (it prints the recipe and runs nothing).
# It also asserts that "make -n loadtest" succeeds when no k6 binary exists
# (the check for k6 belongs in the recipe, never in the parse phase) and that
# backend/Makefile defines test-coverage.
#
# Usage: scripts/check-make-targets.sh
#   Takes no arguments. Exit codes: 0 all checks pass, 1 at least one check
#   failed (each failure is printed as "FAIL: ..."), 2 usage error.
set -euo pipefail

if [ "$#" -gt 0 ]; then
  echo "usage: scripts/check-make-targets.sh (no arguments)" >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

TARGETS="loadtest loadtest-stress test-coverage test-backend-repeat seed"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

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
  exit 1
fi
echo "check-make-targets: all targets present and plannable ($TARGETS)"

#!/usr/bin/env bash
# Per-package coverage gate. "make test-coverage" (backend/Makefile) calls it
# after "go test -coverpkg=<listed packages> -coverprofile=<profile> ./...".
#
# Usage: scripts/coverage-gate.sh PROFILE PACKAGES_FILE [MIN_PERCENT]
#   PROFILE         Go coverage profile (go test -coverprofile)
#   PACKAGES_FILE   one package per line, relative to backend/ (./api,
#                   ./internal/leader); blank lines and # comments are ignored
#   MIN_PERCENT     minimum statement coverage per package, default 80
#
# Every listed package must reach MIN_PERCENT on its own (the gate is not an
# average). Coverage counts a statement as covered when any test binary of the
# run executed it (-coverpkg), so a package is also credited for what the tests
# of other packages exercise, for example the api main package through its
# bootstrap tests. A listed package that has no statements in the profile is an
# error (a typo, or a package that no test run measured).
#
# Exit codes: 0 every package passes, 1 a package is below the minimum or has
# no statements, 2 usage or input error.
set -euo pipefail

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "usage: scripts/coverage-gate.sh PROFILE PACKAGES_FILE [MIN_PERCENT]" >&2
  exit 2
fi
profile=$1
packages_file=$2
min=${3:-80}

case "$min" in
  '' | *[!0-9.]*)
    echo "ERROR: MIN_PERCENT must be a number, got '$min'" >&2
    exit 2
    ;;
esac
for f in "$profile" "$packages_file"; do
  if [ ! -r "$f" ]; then
    echo "ERROR: cannot read $f" >&2
    exit 2
  fi
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
module=$(cd "$root/backend" && go list -m)

pkgs=$(sed -e 's/#.*//' -e 's/[[:space:]]//g' "$packages_file" | grep . || true)
if [ -z "$pkgs" ]; then
  echo "ERROR: $packages_file lists no package" >&2
  exit 2
fi

# Profile lines are "file:start.col,end.col numStatements count". The same
# block can appear once per test binary: count it once, covered when any
# binary ran it.
stats=$(awk '
  /^mode:/ { next }
  {
    file = $1; sub(/:.*/, "", file); dir = file; sub(/\/[^\/]*$/, "", dir)
    key = $1; stmts[key] = $2; where[key] = dir
    if ($3 > 0) hit[key] = 1
  }
  END {
    for (k in stmts) { total[where[k]] += stmts[k]; if (k in hit) covered[where[k]] += stmts[k] }
    for (d in total) printf "%s %d %d\n", d, covered[d] + 0, total[d]
  }
' "$profile")

failed=0
printf '%-45s %9s %9s %8s\n' package covered statements percent
while IFS= read -r pkg; do
  dir=${pkg#./}
  [ "$dir" = "." ] && dir=""
  import_path=$module${dir:+/$dir}
  line=$(printf '%s\n' "$stats" | awk -v p="$import_path" '$1 == p { print }')
  if [ -z "$line" ]; then
    printf '%-45s %s\n' "$pkg" "FAIL: no statements in the profile (typo, or no test run measured it)"
    failed=1
    continue
  fi
  read -r _ cov tot <<<"$line"
  pct=$(awk -v c="$cov" -v t="$tot" 'BEGIN { printf "%.1f", 100 * c / t }')
  if awk -v p="$pct" -v m="$min" 'BEGIN { exit !(p + 0 < m + 0) }'; then
    printf '%-45s %9s %9s %7s%%  FAIL (minimum %s%%)\n' "$pkg" "$cov" "$tot" "$pct" "$min"
    failed=1
  else
    printf '%-45s %9s %9s %7s%%\n' "$pkg" "$cov" "$tot" "$pct"
  fi
done <<<"$pkgs"

if [ "$failed" -ne 0 ]; then
  echo "Coverage gate FAILED: every listed package needs at least ${min}%." >&2
  exit 1
fi
echo "Coverage gate passed: every listed package has at least ${min}%."

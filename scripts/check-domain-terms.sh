#!/usr/bin/env bash
# Domain-term check: fails when a Kubernetes/stack/Helm/Azure/mempalace concept
# leaks into this template. Scans files tracked by git (git grep), matching each
# forbidden term case-insensitively as a whole word.
#
# Usage: scripts/check-domain-terms.sh
#   Takes no arguments; any argument is rejected (exit 2 with a usage line).
#   There is deliberately no --self-test flag: the self-test is the separate
#   script scripts/check-domain-terms.test.sh.
#   Prints "file:line:term" for each hit that is not allowlisted.
#   Exit codes: 0 no hits, 1 at least one hit, 2 usage/configuration/scan error
#   (bad allowlist, not a git repository, git grep failure). Errors never pass.
#
# Environment:
#   DOMAIN_TERMS_ROOT  Git repository to scan. Defaults to the repository that
#                      contains this script. The allowlist is read from
#                      $DOMAIN_TERMS_ROOT/scripts/domain-terms.allowlist.
#                      (The self-test scripts/check-domain-terms.test.sh sets this
#                      to temporary fixture repositories.)
#
# Allowlist format, one entry per line ("#" starts a comment, blank lines ok):
#   path-glob|term|reason
# An entry allows one term in every file matching the glob (bash case pattern,
# "*" also matches "/"). Rules, enforced on load (violations exit 2):
#   - only the generic words stack, namespace and cluster may be allowlisted;
#     domain terms (kubeconfig, helm, azure, ...) must be removed, never listed;
#   - the reason must not be empty;
#   - the glob must not be a catch-all made only of "*" and "/" (e.g. *, **, **/*);
#   - every line must have exactly the three fields.
#
# Never scanned: docs/superpowers/**, .superpowers/**, the allowlist itself,
# lockfiles (package-lock.json, go.sum) and the two check scripts (they list the
# terms).

set -u

if [ "$#" -gt 0 ]; then
  echo "usage: scripts/check-domain-terms.sh (no arguments; set DOMAIN_TERMS_ROOT to scan another repo)" >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${DOMAIN_TERMS_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}"
ALLOWLIST="$ROOT/scripts/domain-terms.allowlist"

# term|regex (regex used with grep -E -w; "." in k8s.io must be literal)
TERMS="kubeconfig|kubeconfig
k8s.io|k8s\\.io
kubernetes|kubernetes
helm|helm
deployer|deployer
gitprovider|gitprovider
quota|quota
azure|azure
azurite|azurite
mempalace|mempalace
cluster|cluster
namespace|namespace
stack|stack"

if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "error: $ROOT is not a git repository" >&2
  exit 2
fi

# Load and validate allowlist entries as "glob|term" (reason is documentation).
ALLOW_ENTRIES=""
if [ -f "$ALLOWLIST" ]; then
  lineno=0
  while IFS= read -r line || [ -n "$line" ]; do
    lineno=$((lineno + 1))
    case "$line" in
      '' | '#'*) continue ;;
    esac
    where="scripts/domain-terms.allowlist:$lineno"
    seps="${line//[^|]/}"
    if [ "${#seps}" -lt 2 ]; then
      echo "error: $where: malformed entry (expected path-glob|term|reason): $line" >&2
      exit 2
    fi
    glob="${line%%|*}"
    rest="${line#*|}"
    term="${rest%%|*}"
    reason="${rest#*|}"
    term="$(printf '%s' "$term" | tr '[:upper:]' '[:lower:]')"
    case "$term" in
      stack | namespace | cluster) ;;
      *)
        echo "error: $where: term '$term' cannot be allowlisted; only stack, namespace and cluster may be (remove the term from the template instead)" >&2
        exit 2
        ;;
    esac
    if [ -z "${reason//[[:space:]]/}" ]; then
      echo "error: $where: empty reason; every allowlist entry needs a reason" >&2
      exit 2
    fi
    if [ -z "$glob" ] || [ -z "${glob//[*\/]/}" ]; then
      echo "error: $where: glob '$glob' is too broad or empty; name the specific file(s)" >&2
      exit 2
    fi
    ALLOW_ENTRIES="${ALLOW_ENTRIES}${glob}|${term}
"
  done <"$ALLOWLIST"
fi

HITS_FILE="$(mktemp)"
ERR_FILE="$(mktemp)"
trap 'rm -f "$HITS_FILE" "$ERR_FILE"' EXIT

# Collect raw hits as "path:line:term" for every term.
while IFS='|' read -r term regex; do
  [ -n "$term" ] || continue
  out="$(git -C "$ROOT" grep -I -n -i -w -E -e "$regex" -- . \
    ':(exclude,glob)docs/superpowers/**' \
    ':(exclude,glob).superpowers/**' \
    ':(exclude)scripts/domain-terms.allowlist' \
    ':(exclude)scripts/check-domain-terms.sh' \
    ':(exclude)scripts/check-domain-terms.test.sh' \
    ':(exclude,glob)**/package-lock.json' \
    ':(exclude,glob)**/go.sum' 2>"$ERR_FILE")"
  status=$?
  # git grep: 0 = matches, 1 = no matches; anything else is a real error.
  if [ "$status" -gt 1 ]; then
    echo "error: git grep failed (exit $status) while scanning for '$term':" >&2
    cat "$ERR_FILE" >&2
    exit 2
  fi
  [ -n "$out" ] || continue
  while IFS= read -r line; do
    path="${line%%:*}"
    rest="${line#*:}"
    hitline="${rest%%:*}"
    printf '%s:%s:%s\n' "$path" "$hitline" "$term"
  done <<EOF >>"$HITS_FILE"
$out
EOF
done <<EOF
$TERMS
EOF

is_allowed() { # path term
  local path="$1" term="$2" glob aterm
  while IFS='|' read -r glob aterm; do
    [ -n "$glob" ] || continue
    [ "$aterm" = "$term" ] || continue
    # shellcheck disable=SC2254 # glob is intentionally an unquoted pattern
    case "$path" in
      $glob) return 0 ;;
    esac
  done <<EOF
$ALLOW_ENTRIES
EOF
  return 1
}

FOUND=0
while IFS=: read -r path lineno term; do
  [ -n "$path" ] || continue
  if is_allowed "$path" "$term"; then
    continue
  fi
  echo "$path:$lineno:$term"
  FOUND=$((FOUND + 1))
done < <(sort -t: -k1,1 -k2,2n -k3,3 "$HITS_FILE")

if [ "$FOUND" -gt 0 ]; then
  echo "domain-term check failed: $FOUND hit(s). Remove the term or add a reviewed entry to scripts/domain-terms.allowlist." >&2
  exit 1
fi
echo "domain-term check passed"
exit 0

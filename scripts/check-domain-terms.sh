#!/usr/bin/env bash
# Domain-term check: fails when a Kubernetes/stack/Helm/Azure/mempalace concept
# leaks into this template. Scans files tracked by git (git grep), matching each
# forbidden term case-insensitively as a whole word.
#
# Usage: scripts/check-domain-terms.sh
#   Prints "file:line:term" for each hit that is not allowlisted. Exit 1 if there
#   is at least one hit, 0 otherwise.
#
# Environment:
#   DOMAIN_TERMS_ROOT  Git repository to scan. Defaults to the repository that
#                      contains this script. The allowlist is read from
#                      $DOMAIN_TERMS_ROOT/scripts/domain-terms.allowlist.
#                      (The self-test scripts/check-domain-terms.test.sh sets this
#                      to temporary fixture repositories.)
#
# Allowlist format, one entry per line ("#" starts a comment):
#   path-glob|term|reason
# An entry allows one term in every file matching the glob (bash case pattern,
# "*" also matches "/"). Generic uses of "stack", "namespace" and "cluster"
# (e.g. "full-stack", "call stack") need a reviewed entry with a reason.
#
# Never scanned: docs/superpowers/**, .superpowers/**, the allowlist itself,
# lockfiles (package-lock.json, go.sum) and the two check scripts (they list the
# terms).

set -u

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

HITS_FILE="$(mktemp)"
trap 'rm -f "$HITS_FILE"' EXIT

# Collect raw hits as "path:line:term" for every term.
while IFS='|' read -r term regex; do
  [ -n "$term" ] || continue
  git -C "$ROOT" grep -I -n -i -w -E -e "$regex" -- . \
    ':(exclude,glob)docs/superpowers/**' \
    ':(exclude,glob).superpowers/**' \
    ':(exclude)scripts/domain-terms.allowlist' \
    ':(exclude)scripts/check-domain-terms.sh' \
    ':(exclude)scripts/check-domain-terms.test.sh' \
    ':(exclude,glob)**/package-lock.json' \
    ':(exclude,glob)**/go.sum' 2>/dev/null |
    while IFS= read -r line; do
      path="${line%%:*}"
      rest="${line#*:}"
      lineno="${rest%%:*}"
      printf '%s:%s:%s\n' "$path" "$lineno" "$term"
    done >>"$HITS_FILE"
done <<EOF
$TERMS
EOF

# Load allowlist entries as "glob|term" (reason is documentation only).
ALLOW_ENTRIES=""
if [ -f "$ALLOWLIST" ]; then
  while IFS='|' read -r glob term _reason; do
    case "$glob" in
      '' | '#'*) continue ;;
    esac
    [ -n "$term" ] || continue
    ALLOW_ENTRIES="${ALLOW_ENTRIES}${glob}|$(printf '%s' "$term" | tr '[:upper:]' '[:lower:]')
"
  done <"$ALLOWLIST"
fi

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

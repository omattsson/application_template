#!/usr/bin/env bash
# Self-test for scripts/check-domain-terms.sh using temporary git repositories.
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-domain-terms.sh"
TMP_BASE="$(mktemp -d)"
trap 'rm -rf "$TMP_BASE"' EXIT

FAILED=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILED=$((FAILED + 1)); }

# new_repo NAME -> creates an empty git repo and prints its path
new_repo() {
  local dir="$TMP_BASE/$1"
  mkdir -p "$dir/scripts"
  git -C "$dir" init -q
  git -C "$dir" config user.email test@example.com
  git -C "$dir" config user.name test
  echo "$dir"
}

# run_check DIR -> sets OUT and RC
run_check() {
  OUT="$(DOMAIN_TERMS_ROOT="$1" bash "$CHECK" 2>&1)"
  RC=$?
}

# Case 1: forbidden term fails and names file:line:term
repo="$(new_repo hit)"
printf 'line one\nuse the kubeconfig here\n' >"$repo/notes.txt"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 1 ] && echo "$OUT" | grep -q '^notes.txt:2:kubeconfig$'; then
  pass "forbidden term fails and names file:line:term"
else
  fail "forbidden term fails and names file:line:term (rc=$RC out=$OUT)"
fi

# Case 2: the same file passes when allowlisted
printf 'notes.txt|kubeconfig|test fixture\n' >"$repo/scripts/domain-terms.allowlist"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 0 ]; then
  pass "allowlisted term passes"
else
  fail "allowlisted term passes (rc=$RC out=$OUT)"
fi

# Case 3: clean file passes
repo="$(new_repo clean)"
printf 'nothing to see here\n' >"$repo/readme.txt"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 0 ]; then
  pass "clean file passes"
else
  fail "clean file passes (rc=$RC out=$OUT)"
fi

# Case 4: matching is case-insensitive and whole-word only
repo="$(new_repo words)"
printf 'Helm chart\nthe stacked deck\nK8s.io/api\n' >"$repo/a.md"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 1 ] && echo "$OUT" | grep -q '^a.md:1:helm$' \
  && echo "$OUT" | grep -q '^a.md:3:k8s.io$' \
  && ! echo "$OUT" | grep -q '^a.md:2:'; then
  pass "case-insensitive whole-word matching"
else
  fail "case-insensitive whole-word matching (rc=$RC out=$OUT)"
fi

# Case 5: an allowlist entry is scoped to its term
repo="$(new_repo scoped)"
printf 'call stack and cluster\n' >"$repo/b.md"
printf 'b.md|stack|generic English\n' >"$repo/scripts/domain-terms.allowlist"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 1 ] && echo "$OUT" | grep -q '^b.md:1:cluster$' \
  && ! echo "$OUT" | grep -q ':stack$'; then
  pass "allowlist entry only covers its own term"
else
  fail "allowlist entry only covers its own term (rc=$RC out=$OUT)"
fi

# Case 6: ignored paths are not scanned
repo="$(new_repo ignored)"
mkdir -p "$repo/docs/superpowers" "$repo/.superpowers" "$repo/frontend"
printf 'kubernetes\n' >"$repo/docs/superpowers/spec.md"
printf 'kubernetes\n' >"$repo/.superpowers/x.md"
printf 'kubernetes\n' >"$repo/frontend/package-lock.json"
printf 'kubernetes\n' >"$repo/go.sum"
printf 'helm|kubernetes\n' >"$repo/scripts/domain-terms.allowlist"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 0 ]; then
  pass "ignored paths are skipped"
else
  fail "ignored paths are skipped (rc=$RC out=$OUT)"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "$FAILED test(s) failed"
  exit 1
fi
echo "All tests passed"

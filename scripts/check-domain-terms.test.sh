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

# Case 2: a generic word (stack) passes when allowlisted; the domain term does not
repo="$(new_repo allowed)"
printf 'line one\nthe call stack here\n' >"$repo/notes.txt"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 1 ] && echo "$OUT" | grep -q '^notes.txt:2:stack$'; then
  pass "generic word fails before allowlisting"
else
  fail "generic word fails before allowlisting (rc=$RC out=$OUT)"
fi
printf 'notes.txt|stack|test fixture\n' >"$repo/scripts/domain-terms.allowlist"
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
printf 'unused.txt|stack|well-formed entry for an unrelated file\n' >"$repo/scripts/domain-terms.allowlist"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 0 ]; then
  pass "ignored paths are skipped"
else
  fail "ignored paths are skipped (rc=$RC out=$OUT)"
fi

# expect_config_error NAME ALLOWLIST_LINE EXPECTED_MESSAGE_REGEX
# A repo with a real hit plus a bad allowlist entry must exit 2 (not 0, not 1).
expect_config_error() {
  local name="$1" line="$2" msg="$3" r
  r="$(new_repo "cfg-$name")"
  printf 'the kubeconfig and azure and stack\n' >"$r/c.md"
  printf '%s\n' "$line" >"$r/scripts/domain-terms.allowlist"
  git -C "$r" add -A
  run_check "$r"
  if [ "$RC" -eq 2 ] && echo "$OUT" | grep -Eqi "$msg"; then
    pass "rejected allowlist entry: $name"
  else
    fail "rejected allowlist entry: $name (rc=$RC out=$OUT)"
  fi
}

# Case 7: only the generic words may be allowlisted
for t in kubeconfig k8s.io kubernetes helm deployer gitprovider quota azure azurite mempalace Azure; do
  expect_config_error "term $t" "c.md|$t|attempt to mask a domain term" "domain-terms.allowlist:1.*$t"
done
expect_config_error "unknown term" "c.md|banana|not a forbidden term" "domain-terms.allowlist:1"

# Case 8: bare catch-all globs are rejected
expect_config_error "glob *" '*|stack|too broad' "domain-terms.allowlist:1.*glob"
expect_config_error "glob **" '**|stack|too broad' "domain-terms.allowlist:1.*glob"
expect_config_error "glob **/*" '**/*|stack|too broad' "domain-terms.allowlist:1.*glob"

# Case 9: empty reason and malformed lines are rejected
expect_config_error "empty reason" 'c.md|stack|' "domain-terms.allowlist:1.*reason"
expect_config_error "blank reason" 'c.md|stack|   ' "domain-terms.allowlist:1.*reason"
expect_config_error "malformed (2 fields)" 'c.md|stack' "domain-terms.allowlist:1.*malformed"
expect_config_error "malformed (1 field)" 'c.md' "domain-terms.allowlist:1.*malformed"

# Case 10: a config error on line 2 names line 2 and still exits 2
repo="$(new_repo cfg-line2)"
printf 'x stack\n' >"$repo/d.md"
printf '# comment\nd.md|helm|bad\n' >"$repo/scripts/domain-terms.allowlist"
git -C "$repo" add -A
run_check "$repo"
if [ "$RC" -eq 2 ] && echo "$OUT" | grep -q 'domain-terms.allowlist:2'; then
  pass "config error names the offending line"
else
  fail "config error names the offending line (rc=$RC out=$OUT)"
fi

# Case 11: scan failures fail closed
# 11a: root is not a git repository
mkdir -p "$TMP_BASE/notgit"
run_check "$TMP_BASE/notgit"
if [ "$RC" -eq 2 ]; then
  pass "non-git root exits 2"
else
  fail "non-git root exits 2 (rc=$RC out=$OUT)"
fi

# 11b: git grep itself fails (shim makes "git grep" exit 128)
repo="$(new_repo grepfail)"
printf 'kubeconfig\n' >"$repo/e.md"
git -C "$repo" add -A
REAL_GIT="$(command -v git)"
mkdir -p "$TMP_BASE/shim"
cat >"$TMP_BASE/shim/git" <<SHIM
#!/usr/bin/env bash
for a in "\$@"; do
  if [ "\$a" = "grep" ]; then
    echo "fatal: simulated git grep failure" >&2
    exit 128
  fi
done
exec "$REAL_GIT" "\$@"
SHIM
chmod +x "$TMP_BASE/shim/git"
OUT="$(PATH="$TMP_BASE/shim:$PATH" DOMAIN_TERMS_ROOT="$repo" bash "$CHECK" 2>&1)"
RC=$?
if [ "$RC" -eq 2 ] && echo "$OUT" | grep -q 'simulated git grep failure'; then
  pass "git grep failure exits 2 and prints the error"
else
  fail "git grep failure exits 2 and prints the error (rc=$RC out=$OUT)"
fi

# Case 12: unknown arguments are rejected with a usage line
repo="$(new_repo args)"
printf 'clean\n' >"$repo/f.md"
git -C "$repo" add -A
OUT="$(DOMAIN_TERMS_ROOT="$repo" bash "$CHECK" --self-test 2>&1)"
RC=$?
if [ "$RC" -eq 2 ] && echo "$OUT" | grep -qi '^usage:'; then
  pass "unknown argument exits 2 with usage"
else
  fail "unknown argument exits 2 with usage (rc=$RC out=$OUT)"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "$FAILED test(s) failed"
  exit 1
fi
echo "All tests passed"

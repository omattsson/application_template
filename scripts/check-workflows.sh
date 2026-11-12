#!/usr/bin/env bash
# scripts/check-workflows.sh
# Checks the GitHub Actions workflows (.github/workflows/*.yml) for the CI
# hygiene rules of this repository. It needs no YAML parser: it reads the
# workflow text line by line and therefore expects the usual formatting (two
# spaces per level, `on:` as a block, one job key per line).
#
#   1. A workflow with a pull_request trigger lists `main` in
#      pull_request.branches (a trigger without branches fails too).
#      pull-request.yml must have the trigger, and push.branches with `main`.
#   2. No pull_request_target trigger.
#   3. Every `uses:` of an action is pinned to a full 40-character commit SHA
#      with a trailing `# vX.Y.Z` comment. A branch ref (@master, @main), @latest
#      or an old major (@v1 to @v3) is reported with that reason.
#   4. No `golint` (deprecated) and no `@latest` in a command.
#   5. No floating toolchain version (latest, stable, oldstable, >=, ^, ~, *)
#      for go-version, node-version and python-version.
#   6. A top-level `permissions:` key without write access (no `write-all`,
#      no `<scope>: write`). A job may raise a specific scope itself.
#   7. Every job that has steps has `timeout-minutes` (a job that calls a
#      reusable workflow with `uses:` takes its limits from the callee).
#   8. No `${{ github.event.* }}`, `github.head_ref`, `github.base_ref`,
#      `github.ref_name` or `inputs.*` inside a `run:` script or an
#      actions/github-script `script:` block: pass it through `env:` instead
#      (script injection).
#
# Usage:
#   scripts/check-workflows.sh                 check .github/workflows/*.yml
#   scripts/check-workflows.sh FILE...         check the given workflow files
#   scripts/check-workflows.sh --self-test     run the built-in fixture tests
# The fixtures contain literal ${{ }} expressions in single quotes on purpose.
# shellcheck disable=SC2016
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# check_one FILE -> prints one line per violation; returns 1 if any.
check_one() {
  local file="$1" base out rc=0
  base="$(basename "$file")"

  # Rules 1 and 2: triggers.
  out="$(awk -v file="$file" -v base="$base" '
    function err(msg) { printf "%s: %s\n", file, msg; bad = 1 }
    function indent_of(s) { match(s, /^ */); return RLENGTH }
    # has_main(V): V is an inline branch list ([main, "release"]) or a scalar;
    # true only for an item that is exactly main.
    function has_main(v,   n, parts, i, t) {
      sub(/^\[/, "", v); sub(/\][[:space:]]*$/, "", v)
      n = split(v, parts, ",")
      for (i = 1; i <= n; i++) {
        t = parts[i]
        sub(/^[[:space:]]+/, "", t); sub(/[[:space:]]+$/, "", t)
        gsub(/["\047]/, "", t)
        if (t == "main") return 1
      }
      return 0
    }
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    {
      ind = indent_of($0)
      line = $0
      sub(/^[ ]+/, "", line)
    }
    ind == 0 {
      in_on = (line ~ /^on:/ || line ~ /^"on":/)
      sec = ""
      if (in_on) {
        seen_on = 1
        rest = line; sub(/^"?on"?:[ ]*/, "", rest); sub(/[ ]*#.*$/, "", rest)
        if (rest != "") {
          if (rest ~ /pull_request_target/) err("pull_request_target is not allowed")
          if (rest ~ /pull_request/) err("write `on:` as a block so that pull_request.branches can be checked")
        }
      }
      next
    }
    !in_on { next }
    ind == 2 {
      sec = line; sub(/:.*$/, "", sec)
      in_branches = 0
      if (sec == "pull_request_target") err("pull_request_target is not allowed")
      if (sec == "pull_request" || sec == "push") has[sec] = 1
      next
    }
    ind >= 4 && (sec == "pull_request" || sec == "push") {
      if (line ~ /^branches:/) {
        in_branches = 0
        val = line; sub(/^branches:[ ]*/, "", val); sub(/[ ]*#.*$/, "", val)
        if (val == "") { in_branches = 1 }
        else if (has_main(val)) main[sec] = 1
        next
      }
      if (in_branches && line ~ /^-/) {
        item = line; sub(/^-[ ]*/, "", item); sub(/[ ]*#.*$/, "", item); gsub(/["\047]/, "", item)
        if (item == "main") main[sec] = 1
        next
      }
      if (line !~ /^-/) in_branches = 0
    }
    END {
      if (!seen_on) err("no `on:` block")
      if (has["pull_request"] && !main["pull_request"]) err("pull_request.branches must list main")
      if (base == "pull-request.yml") {
        if (!has["pull_request"]) err("pull-request.yml needs a pull_request trigger")
        if (!has["push"] || !main["push"]) err("pull-request.yml needs push.branches with main")
      }
      exit bad
    }
  ' "$file")" || rc=1
  [ -n "$out" ] && echo "$out"

  # Rule 3: pinned actions.
  out="$(awk -v file="$file" '
    function err(n, msg) { printf "%s:%d: %s\n", file, n, msg; bad = 1 }
    /^[[:space:]]*#/ { next }
    /^[[:space:]-]*uses:[[:space:]]/ {
      line = $0
      comment = ""
      p = index(line, " #")
      if (p > 0) { comment = substr(line, p + 2); line = substr(line, 1, p - 1) }
      sub(/^[[:space:]-]*uses:[[:space:]]*/, "", line)
      gsub(/["\047[:space:]]/, "", line)
      if (line ~ /^\.\//) next
      if (line ~ /^docker:\/\//) {
        if (line !~ /@sha256:[0-9a-f]+$/) err(NR, "docker:// action needs an @sha256 digest: " line)
        next
      }
      at = index(line, "@")
      if (at == 0) { err(NR, "action without a version: " line); next }
      name = substr(line, 1, at - 1); ref = substr(line, at + 1)
      if (ref ~ /^[0-9a-f]+$/ && length(ref) == 40) {
        if (comment !~ /^[[:space:]]*v?[0-9]+/) err(NR, name " is pinned by SHA but has no `# vX.Y.Z` version comment")
      } else if (ref == "master" || ref == "main" || ref == "HEAD" || ref == "latest" || ref == "stable") {
        err(NR, name "@" ref " is a floating ref; pin a full commit SHA")
      } else if (ref ~ /^v?[0-9]+(\.[0-9]+)*$/) {
        major = ref; sub(/^v/, "", major); sub(/\..*$/, "", major)
        if (major + 0 <= 3) err(NR, name "@" ref " is an old major (v1 to v3); use a current release pinned by SHA")
        else err(NR, name "@" ref " is a tag; pin the full commit SHA with a version comment")
      } else {
        err(NR, name "@" ref " is not a full commit SHA")
      }
    }
    END { exit bad }
  ' "$file")" || rc=1
  [ -n "$out" ] && echo "$out"

  # Rule 4: golint and @latest.
  out="$(awk -v file="$file" '
    /^[[:space:]]*#/ { next }
    {
      if ($0 ~ /golint/) { printf "%s:%d: golint is deprecated and must not be used\n", file, NR; bad = 1 }
      if ($0 ~ /@latest([^A-Za-z0-9_.-]|$)/) { printf "%s:%d: floating @latest; pin a version\n", file, NR; bad = 1 }
    }
    END { exit bad }
  ' "$file")" || rc=1
  [ -n "$out" ] && echo "$out"

  # Rule 5: floating toolchain versions.
  out="$(awk -v file="$file" '
    /^[[:space:]]*#/ { next }
    /^[[:space:]-]*(go-version|node-version|python-version):/ {
      key = $0; sub(/^[[:space:]-]*/, "", key); sub(/:.*$/, "", key)
      val = $0; sub(/^[^:]*:[[:space:]]*/, "", val); sub(/[[:space:]]*#.*$/, "", val)
      gsub(/["\047]/, "", val)
      if (val ~ /^(latest|stable|oldstable|nightly|lts\/.*)$/ || val ~ /^[<>^~*]/ || val ~ /^[0-9.]*[xX*]$/) {
        printf "%s:%d: floating %s \"%s\"; use a *-version-file or an exact version\n", file, NR, key, val
        bad = 1
      }
    }
    END { exit bad }
  ' "$file")" || rc=1
  [ -n "$out" ] && echo "$out"

  # Rule 6: top-level permissions, read only.
  if ! grep -Eq '^permissions:' "$file"; then
    echo "$file: no top-level permissions: key (use contents: read)"
    rc=1
  else
    out="$(awk -v file="$file" '
      /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
      /^[^[:space:]]/ {
        in_perm = ($0 ~ /^permissions:/)
        if (in_perm && $0 ~ /^permissions:[[:space:]]*write-all/) {
          printf "%s:%d: top-level permissions: write-all; use contents: read and raise scopes per job\n", file, NR; bad = 1
        }
        next
      }
      in_perm && /^[[:space:]]+[A-Za-z-]+:[[:space:]]*["\047]?write/ {
        line = $0; sub(/^[[:space:]]+/, "", line); sub(/:.*$/, "", line)
        printf "%s:%d: top-level permissions %s: write; raise it in the job that needs it\n", file, NR, line; bad = 1
      }
      END { exit bad }
    ' "$file")" || rc=1
    [ -n "$out" ] && echo "$out"
  fi

  # Rule 7: timeout-minutes in every job.
  out="$(awk -v file="$file" '
    function flush() {
      if (job != "" && !has_timeout && !is_call) { printf "%s:%d: job %s has no timeout-minutes\n", file, job_line, job; bad = 1 }
    }
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    /^[^[:space:]]/ {
      flush(); job = ""
      in_jobs = ($0 ~ /^jobs:/)
      next
    }
    !in_jobs { next }
    /^  [^[:space:]]/ {
      flush()
      job = $0; sub(/^  /, "", job); sub(/:.*$/, "", job)
      job_line = NR; has_timeout = 0; is_call = 0
      next
    }
    /^    timeout-minutes:/ { has_timeout = 1 }
    /^    uses:/ { is_call = 1 }
    END { flush(); exit bad }
  ' "$file")" || rc=1
  [ -n "$out" ] && echo "$out"

  # Rule 8: event data inside run scripts.
  out="$(awk -v file="$file" '
    function indent_of(s) { match(s, /^ */); return RLENGTH }
    function scan(n, s) {
      if (s ~ /\$\{\{[^}]*github\.(event\.|head_ref|base_ref|ref_name)/ || s ~ /\$\{\{([^}]*[^A-Za-z0-9_.])?inputs\./) {
        printf "%s:%d: event, ref or input data in a run/script block; pass it through env:\n", file, n
        bad = 1
      }
    }
    /^[[:space:]]*#/ { next }
    {
      if (in_run) {
        if ($0 ~ /^[[:space:]]*$/) next
        if (indent_of($0) > run_ind) { scan(NR, $0); next }
        in_run = 0
      }
      if ($0 ~ /^[[:space:]-]*(run|script):/) {
        key = $0; sub(/^[[:space:]-]*(run|script):[[:space:]]*/, "", key)
        run_ind = indent_of($0)
        # "- run:" puts the key two columns right of the dash.
        if ($0 ~ /^[[:space:]]*-[[:space:]]+(run|script):/) run_ind += 2
        if (key ~ /^[|>]/) in_run = 1
        else scan(NR, key)
      }
    }
    END { exit bad }
  ' "$file")" || rc=1
  [ -n "$out" ] && echo "$out"

  return $rc
}

run_checks() {
  local rc=0 f
  for f in "$@"; do
    if [ ! -f "$f" ]; then
      echo "$f: file not found"
      rc=1
      continue
    fi
    check_one "$f" || rc=1
  done
  return $rc
}

self_test() {
  local tmp failed=0
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local sha_a=0123456789abcdef0123456789abcdef01234567
  # wf NAME -> writes $tmp/NAME/pull-request.yml. Every part has a conforming
  # default; a test overrides one part through an environment variable
  # (set to the empty string to drop it). Parts use \n for line breaks.
  wf() {
    mkdir -p "$tmp/$1"
    {
      printf '%b\n' 'name: PR'
      printf '%b\n' "${ON_HEADER-on:}"
      printf '%b\n' "${PR_BLOCK-  pull_request:\n    branches: [main]}"
      printf '%b\n' "${PUSH_BLOCK-  push:\n    branches: [main]}"
      printf '%b\n' "${PERM_BLOCK-permissions:\n  contents: read}"
      printf '%b\n' 'jobs:'
      printf '%b\n' '  backend:'
      printf '%b\n' '    runs-on: ubuntu-latest'
      printf '%b\n' "${TIMEOUT_LINE-    timeout-minutes: 10}"
      printf '%b\n' '    steps:'
      printf '%b\n' "      - uses: ${CHECKOUT_USES-actions/checkout@$sha_a # v7.0.1}"
      printf '%b\n' "      - uses: actions/setup-go@$sha_a # v7.0.0"
      printf '%b\n' "        with:\n          ${GO_VERSION-go-version-file: backend/go.mod}"
      printf '%b\n' "${EXTRA_STEPS-}"
      printf '%b\n' '      - name: Title'
      printf '%b\n' '        env:'
      printf '%b\n' '          TITLE: ${{ github.event.pull_request.title }}'
      printf '%b\n' "${RUN_STEP-        run: |\n          echo \"\$TITLE\"}"
      printf '%b\n' "${EXTRA_JOB-}"
    } >"$tmp/$1/pull-request.yml"
  }
  expect() { # NAME pass|fail
    local out rc
    out="$(run_checks "$tmp/$1/pull-request.yml" 2>&1)"; rc=$?
    if { [ "$2" = pass ] && [ $rc -eq 0 ]; } || { [ "$2" = fail ] && [ $rc -ne 0 ]; }; then
      echo "PASS: $1 ($2)"
    else
      echo "FAIL: $1 expected $2, got rc=$rc"; echo "$out"; failed=$((failed + 1))
    fi
  }
  expect_msg() { # NAME TEXT: the output names the violation
    local out
    out="$(run_checks "$tmp/$1/pull-request.yml" 2>&1)"
    case "$out" in
      *"$2"*) echo "PASS: $1 reports \"$2\"" ;;
      *) echo "FAIL: $1 did not report \"$2\""; echo "$out"; failed=$((failed + 1)) ;;
    esac
  }

  wf good
  expect good pass

  PR_BLOCK='  pull_request:\n    branches: [release, dev]' wf branches-other
  expect branches-other fail
  expect_msg branches-other "pull_request.branches must list main"

  PR_BLOCK='  pull_request:' wf no-branches
  expect no-branches fail
  expect_msg no-branches "pull_request.branches must list main"

  PR_BLOCK='  pull_request:\n    branches:\n      - main' wf block-list-ok
  expect block-list-ok pass

  PR_BLOCK='  pull_request:\n    branches:\n      - release' wf block-list-other
  expect block-list-other fail
  expect_msg block-list-other "pull_request.branches must list main"

  PUSH_BLOCK='' wf no-push
  expect no-push fail
  expect_msg no-push "needs push.branches"

  PR_BLOCK='  pull_request_target:\n    branches: [main]' wf target
  expect target fail
  expect_msg target "pull_request_target"

  ON_HEADER='on: [pull_request]' PR_BLOCK='' PUSH_BLOCK='' wf inline-on
  expect inline-on fail
  expect_msg inline-on "as a block"

  CHECKOUT_USES='actions/checkout@v3' wf checkout-v3
  expect checkout-v3 fail
  expect_msg checkout-v3 "old major"

  CHECKOUT_USES='actions/checkout@v2' wf checkout-v2
  expect checkout-v2 fail
  expect_msg checkout-v2 "old major"

  CHECKOUT_USES='actions/checkout@v7' wf tag-v7
  expect tag-v7 fail
  expect_msg tag-v7 "pin the full commit SHA"

  CHECKOUT_USES='securego/gosec@master' wf master-ref
  expect master-ref fail
  expect_msg master-ref "floating ref"

  CHECKOUT_USES="actions/checkout@$sha_a" wf no-version-comment
  expect no-version-comment fail
  expect_msg no-version-comment "version comment"

  EXTRA_STEPS='      - run: golint ./...' wf golint
  expect golint fail
  expect_msg golint "golint"

  EXTRA_STEPS='      - run: go install example.com/tool@latest' wf at-latest
  expect at-latest fail
  expect_msg at-latest "@latest"

  GO_VERSION='go-version: ">=1.24.0"' wf go-version-ge
  expect go-version-ge fail
  expect_msg go-version-ge "floating go-version"

  GO_VERSION='node-version: latest' wf node-latest
  expect node-latest fail
  expect_msg node-latest "floating node-version"

  GO_VERSION='go-version: stable' wf go-stable
  expect go-stable fail
  expect_msg go-stable "floating go-version"

  GO_VERSION='go-version: "1.27.4"' wf go-version-exact
  expect go-version-exact pass

  PERM_BLOCK='' wf no-permissions
  expect no-permissions fail
  expect_msg no-permissions "top-level permissions"

  TIMEOUT_LINE='' wf no-timeout
  expect no-timeout fail
  expect_msg no-timeout "no timeout-minutes"

  RUN_STEP='        run: echo "${{ github.event.pull_request.title }}"' wf event-in-run
  expect event-in-run fail
  expect_msg event-in-run "pass it through env"

  RUN_STEP='        run: |\n          echo "${{ github.event.pull_request.title }}"' wf event-in-block
  expect event-in-block fail
  expect_msg event-in-block "pass it through env"

  EXTRA_STEPS='      - run: echo ${{ github.head_ref }}' wf head-ref
  expect head-ref fail
  expect_msg head-ref "pass it through env"

  # Rule 8: more tainted contexts, and github-script blocks.
  RUN_STEP='        run: echo "${{ github.ref_name }}"' wf ref-name
  expect ref-name fail
  expect_msg ref-name "pass it through env"

  EXTRA_STEPS='      - run: echo ${{ github.base_ref }}' wf base-ref
  expect base-ref fail
  expect_msg base-ref "pass it through env"

  EXTRA_STEPS='      - run: echo ${{ inputs.name }}' wf inputs-spaced
  expect inputs-spaced fail
  expect_msg inputs-spaced "pass it through env"

  EXTRA_STEPS='      - run: echo ${{inputs.name}}' wf inputs-compact
  expect inputs-compact fail

  EXTRA_STEPS='      - run: echo ${{ format(inputs.name) }}' wf inputs-in-call
  expect inputs-in-call fail

  EXTRA_STEPS='      - run: echo ${{ github.sha }} ${{ github.event_name }} ${{ steps.inputs.outputs.x }}' wf safe-contexts
  expect safe-contexts pass

  script_uses="      - uses: actions/github-script@$sha_a # v7.0.0"
  EXTRA_STEPS="$script_uses"'\n        with:\n          script: |\n            core.info("${{ github.event.pull_request.title }}")' wf script-block
  expect script-block fail
  expect_msg script-block "pass it through env"

  EXTRA_STEPS="$script_uses"'\n        with:\n          script: core.info("${{ github.head_ref }}")' wf script-inline
  expect script-inline fail

  EXTRA_STEPS="$script_uses"'\n        env:\n          TITLE: ${{ github.event.pull_request.title }}\n        with:\n          script: |\n            core.info(process.env.TITLE)' wf script-env
  expect script-env pass

  # Rule 6: top-level permissions stay read only.
  PERM_BLOCK='permissions: write-all' wf perm-write-all
  expect perm-write-all fail
  expect_msg perm-write-all "write-all"

  PERM_BLOCK='permissions:\n  contents: read\n  pull-requests: write' wf perm-scope-write
  expect perm-scope-write fail
  expect_msg perm-scope-write "pull-requests: write"

  PERM_BLOCK='permissions:\n  contents: write' wf perm-contents-write
  expect perm-contents-write fail

  TIMEOUT_LINE='    timeout-minutes: 10\n    permissions:\n      pull-requests: write' wf perm-job-elevated
  expect perm-job-elevated pass

  # Rule 1: main must be an exact branch name.
  PR_BLOCK='  pull_request:\n    branches: [feature/main]' wf feature-main-inline
  expect feature-main-inline fail
  expect_msg feature-main-inline "must list main"

  PR_BLOCK="  pull_request:\n    branches: ['feature/main', release]" wf feature-main-quoted
  expect feature-main-quoted fail

  PR_BLOCK='  pull_request:\n    branches:\n      - feature/main' wf feature-main-block
  expect feature-main-block fail

  PR_BLOCK='  pull_request:\n    branches: [main-old]' wf main-prefix
  expect main-prefix fail

  PR_BLOCK='  pull_request:\n    branches: [ "main", release ]' wf main-in-list
  expect main-in-list pass

  PR_BLOCK="  pull_request:\n    branches: ['main']" wf main-quoted
  expect main-quoted pass

  PR_BLOCK='  pull_request:\n    branches: main' wf main-scalar
  expect main-scalar pass

  # Rule 7: a reusable-workflow job needs no timeout-minutes.
  EXTRA_JOB="  call:\n    uses: org/repo/.github/workflows/x.yml@$sha_a # v1.2.3" wf reusable-call
  expect reusable-call pass

  EXTRA_JOB='  call:\n    uses: org/repo/.github/workflows/x.yml@v1' wf reusable-call-unpinned
  expect reusable-call-unpinned fail
  expect_msg reusable-call-unpinned "old major"

  EXTRA_JOB='  other:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi' wf second-job-no-timeout
  expect second-job-no-timeout fail
  expect_msg second-job-no-timeout "job other has no timeout-minutes"

  # Rule 3: docker:// actions and SHA length.
  EXTRA_STEPS='      - uses: docker://alpine:3.20' wf docker-tag
  expect docker-tag fail
  expect_msg docker-tag "@sha256"

  EXTRA_STEPS="      - uses: docker://alpine@sha256:$sha_a$sha_a$(printf '%024d' 0)" wf docker-digest
  expect docker-digest pass

  CHECKOUT_USES="actions/checkout@${sha_a%?} # v7.0.1" wf sha-39
  expect sha-39 fail
  expect_msg sha-39 "not a full commit SHA"

  CHECKOUT_USES="actions/checkout@${sha_a}0 # v7.0.1" wf sha-41
  expect sha-41 fail
  expect_msg sha-41 "not a full commit SHA"

  # Rule 2: inline pull_request_target.
  ON_HEADER='on: pull_request_target' PR_BLOCK='' PUSH_BLOCK='' wf inline-target
  expect inline-target fail
  expect_msg inline-target "pull_request_target is not allowed"

  ON_HEADER='on: [push, pull_request_target]' PR_BLOCK='' PUSH_BLOCK='' wf inline-target-list
  expect inline-target-list fail
  expect_msg inline-target-list "pull_request_target is not allowed"

  # A workflow without a pull_request trigger (schedule only) is fine.
  mkdir -p "$tmp/sched"
  cat >"$tmp/sched/scan.yml" <<EOF
name: Scan
on:
  schedule:
    - cron: '0 6 * * 1'
permissions:
  contents: read
jobs:
  scan:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - uses: actions/checkout@$sha_a # v7.0.1
EOF
  if run_checks "$tmp/sched/scan.yml" >/dev/null 2>&1; then
    echo "PASS: schedule-only workflow (pass)"
  else
    echo "FAIL: schedule-only workflow expected pass"; failed=$((failed + 1))
  fi

  # A missing file is a failure, not a silent pass.
  if run_checks "$tmp/none.yml" >/dev/null 2>&1; then
    echo "FAIL: missing file expected fail"; failed=$((failed + 1))
  else
    echo "PASS: missing file (fail)"
  fi

  if [ $failed -ne 0 ]; then
    echo "$failed self-test(s) failed"
    return 1
  fi
  echo "self-test OK"
}

case "${1:-}" in
  --self-test)
    self_test
    ;;
  "")
    files=()
    for f in "$REPO_ROOT"/.github/workflows/*.yml; do
      [ -e "$f" ] && files+=("$f")
    done
    if [ ${#files[@]} -eq 0 ]; then
      echo "check-workflows: no workflow files found" >&2
      exit 1
    fi
    run_checks "${files[@]}" && echo "check-workflows: OK"
    ;;
  *)
    run_checks "$@" && echo "check-workflows: OK"
    ;;
esac

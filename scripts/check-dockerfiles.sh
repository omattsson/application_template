#!/usr/bin/env bash
# scripts/check-dockerfiles.sh
# Checks Dockerfiles for container hardening rules:
#   1. The production stage (stage named "production", else the last stage)
#      sets a USER that is not root/0.
#   2. The production stage has a CMD or ENTRYPOINT.
#   3. No unpinned base image: no ":latest" tag and no untagged image.
#   4. golang images use the version that backend/go.mod declares.
#
# Usage:
#   scripts/check-dockerfiles.sh                  check backend/Dockerfile and
#                                                 frontend/Dockerfile
#   scripts/check-dockerfiles.sh PATH...          check the given Dockerfiles
#   scripts/check-dockerfiles.sh --self-test      run the built-in fixture tests
#
# The go.mod used for rule 4 is the go.mod next to the Dockerfile. A Dockerfile
# without a go.mod beside it skips rule 4.
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# check_one DOCKERFILE -> prints one line per violation; returns 1 if any.
check_one() {
  local file="$1" dir gover=""
  dir="$(dirname "$file")"
  if [ -f "$dir/go.mod" ]; then
    gover="$(awk '$1 == "go" { print $2; exit }' "$dir/go.mod")"
  fi

  awk -v file="$file" -v gover="$gover" '
    function err(msg) { printf "%s: %s\n", file, msg; bad = 1 }
    function close_stage() {
      if (n > 0) { user[n] = cur_user; cmd[n] = cur_cmd }
    }
    {
      line = $0
      sub(/^[ \t]+/, "", line)
      if (line == "" || line ~ /^#/) next
      split(line, tok, /[ \t]+/)
      inst = toupper(tok[1])
    }
    inst == "FROM" {
      close_stage()
      n++
      cur_user = ""; cur_cmd = 0
      i = 2
      while (i in tok && tok[i] ~ /^--/) i++
      image = tok[i]
      name = ""
      if (toupper(tok[i + 1]) == "AS") name = tolower(tok[i + 2])
      names[n] = name
      is_stage = (image in stage_seen)
      if (name != "") stage_seen[name] = 1
      if (is_stage || image == "scratch" || image ~ /\$/) next
      base = image; tag = ""
      if (image ~ /@sha256:/) { next }
      p = index(image, ":")
      if (p > 0) { base = substr(image, 1, p - 1); tag = substr(image, p + 1) }
      if (tag == "") err("line " NR ": base image \"" image "\" has no tag (implicitly :latest)")
      else if (tag == "latest") err("line " NR ": base image \"" image "\" uses :latest")
      if (base == "golang" && tag != "" && gover != "") {
        v = tag; sub(/-.*/, "", v)
        if (v != gover) err("line " NR ": golang tag \"" tag "\" does not match go.mod version " gover)
      }
      next
    }
    inst == "USER" { cur_user = tok[2]; next }
    inst == "CMD" || inst == "ENTRYPOINT" { cur_cmd = 1; next }
    { next }
    END {
      close_stage()
      if (n == 0) { err("no FROM instruction"); exit 1 }
      prod = n
      for (s = 1; s <= n; s++) if (names[s] == "production") prod = s
      u = user[prod]; uid = u; sub(/:.*/, "", uid)
      if (u == "") err("production stage has no USER instruction")
      else if (uid == "root" || uid == "0") err("production stage runs as root (USER " u ")")
      if (!cmd[prod]) err("production stage has no CMD or ENTRYPOINT")
      exit bad
    }
  ' "$file"
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

  mkfix() { # NAME -> creates fixture dir with go.mod, content on stdin
    mkdir -p "$tmp/$1"
    printf 'module x\n\ngo 1.27.3\n' >"$tmp/$1/go.mod"
    cat >"$tmp/$1/Dockerfile"
  }
  expect() { # NAME pass|fail
    local out rc
    out="$(run_checks "$tmp/$1/Dockerfile" 2>&1)"; rc=$?
    if { [ "$2" = pass ] && [ $rc -eq 0 ]; } || { [ "$2" = fail ] && [ $rc -ne 0 ]; }; then
      echo "PASS: $1 ($2)"
    else
      echo "FAIL: $1 expected $2, got rc=$rc"; echo "$out"; failed=$((failed + 1))
    fi
  }

  mkfix good <<'EOF'
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build-prod
RUN true
FROM alpine:3.24 AS production
COPY --from=build-prod /x /x
USER 65532:65532
CMD ["/x"]
EOF
  mkfix root-user <<'EOF'
FROM golang:1.27.3 AS build
FROM alpine:3.24 AS production
USER root
CMD ["/x"]
EOF
  mkfix no-user <<'EOF'
FROM golang:1.27.3 AS build
FROM alpine:3.24 AS production
CMD ["/x"]
EOF
  mkfix no-cmd <<'EOF'
FROM golang:1.27.3 AS build
FROM alpine:3.24 AS production
USER 65532
EOF
  mkfix latest-tag <<'EOF'
FROM golang:1.27.3 AS build
FROM alpine:latest AS production
USER 65532
CMD ["/x"]
EOF
  mkfix untagged <<'EOF'
FROM golang:1.27.3 AS build
FROM alpine AS production
USER 65532
CMD ["/x"]
EOF
  mkfix wrong-go <<'EOF'
FROM golang:1.24.3 AS build
FROM alpine:3.24 AS production
USER 65532
CMD ["/x"]
EOF
  mkfix stage-ref <<'EOF'
FROM golang:1.27.3-alpine AS build
FROM build AS production
USER nobody
ENTRYPOINT ["/x"]
EOF
  mkfix node-nginx <<'EOF'
FROM --platform=$BUILDPLATFORM node:26-alpine AS build
RUN true
FROM build AS development
CMD ["npm", "run", "dev"]
FROM nginxinc/nginx-unprivileged:alpine AS production
COPY --from=build /app/dist /usr/share/nginx/html
USER 101
CMD ["nginx", "-g", "daemon off;"]
EOF
  mkfix nginx-root <<'EOF'
FROM node:26-alpine AS build
FROM nginx:alpine AS production
COPY --from=build /app/dist /usr/share/nginx/html
CMD ["nginx", "-g", "daemon off;"]
EOF

  expect good pass
  expect root-user fail
  expect no-user fail
  expect no-cmd fail
  expect latest-tag fail
  expect untagged fail
  expect wrong-go fail
  expect stage-ref pass
  expect node-nginx pass
  expect nginx-root fail

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
    run_checks "$REPO_ROOT/backend/Dockerfile" "$REPO_ROOT/frontend/Dockerfile" && echo "check-dockerfiles: OK"
    ;;
  *)
    run_checks "$@" && echo "check-dockerfiles: OK"
    ;;
esac

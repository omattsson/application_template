#!/usr/bin/env bash
# PostToolUse hook for Edit, Write and MultiEdit: formats the edited file.
# gofmt for Go, eslint --fix for the frontend sources. It never fails the call;
# a problem that eslint cannot fix is left for 'make lint'.
set -u

path="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
[ -z "$path" ] && exit 0
[ -f "$path" ] || exit 0

case "$path" in
  *.go)
    gofmt -w "$path" 2>&1 | head -20 >&2
    ;;
  */frontend/src/*.ts | */frontend/src/*.tsx)
    frontend="${path%%/frontend/src/*}/frontend"
    [ -d "$frontend/node_modules" ] || exit 0
    (cd "$frontend" && npx --no-install eslint --fix "$path" >/dev/null 2>&1)
    ;;
esac
exit 0

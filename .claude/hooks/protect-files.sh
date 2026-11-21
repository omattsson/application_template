#!/usr/bin/env bash
# PreToolUse hook for Edit, Write and MultiEdit: refuses files that a command
# produces or that hold secrets. Exit 2 blocks the call and shows the message.
set -u

path="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
[ -z "$path" ] && exit 0

case "$path" in
  */backend/docs/docs.go | */backend/docs/swagger.json | */backend/docs/swagger.yaml)
    echo "Generated file: run 'make docs' instead of editing $path." >&2
    exit 2
    ;;
  */package-lock.json | */go.sum)
    echo "Lock file: change it with npm or 'go mod', not by hand ($path)." >&2
    exit 2
    ;;
  */.env.example) exit 0 ;;
  */.env | */.env.*)
    echo "Secrets file: edit .env.example, never $path." >&2
    exit 2
    ;;
esac
exit 0

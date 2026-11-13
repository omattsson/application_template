#!/usr/bin/env bash
# scripts/check-agents.sh
# Keeps the Claude Code agents, the ship-issue skill and the project settings
# clean and consistent.
#
#   1. .claude/agents/ holds at least one agent file (*.md).
#   2. Every agent file starts with a frontmatter block that has a non-empty
#      name (equal to the file name without .md) and a non-empty description.
#   3. No agent file mentions a memory-store MCP tool (the server name in
#      FORBIDDEN_TOOL below, with or without a tool suffix), in the tools list
#      or anywhere else.
#   4. No agent file exists for a name in FORBIDDEN_AGENTS (agents of the
#      project this template came from that do not belong in a template).
#   5. .claude/skills/ship-issue/SKILL.md and .claude/settings.json exist, and
#      neither them nor any agent file mentions a forbidden tool or agent name.
#   6. Every agent named in the first column of a table in the skill has an
#      agent file.
#   7. .claude/settings.json is valid JSON (parsed with jq) and its "allow" list
#      grants nothing that publishes, installs or deletes. An entry is rejected
#      when it equals a banned command (BANNED_COMMANDS: git push, gh api,
#      gh pr create/merge/close, gh release, npm install/i/add/publish,
#      docker push, docker build --push, docker compose up, docker system
#      prune, rm -r/-rf), when it starts with one (an exact entry with
#      arguments), or when it is a wildcard prefix of one (git:*, gh:*, npm:*,
#      rm:*, npm i:*, git *). A bare "Bash", Bash(*), a wildcard in the middle
#      of a command, and any make entry with a wildcard are rejected too: make
#      entries must be exact (a wildcard lets "make lint clean" through).
#   8. The "ask" list still holds the six required entries (REQUIRED_ASK).
#
# The forbidden names are built from two parts below so that this script does
# not trip scripts/check-domain-terms.sh itself.
#
# Usage:
#   scripts/check-agents.sh              check the repository
#   scripts/check-agents.sh --self-test  run the built-in fixture tests
# Override for tests: AGENTS_ROOT (a directory that holds .claude/).
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

FORBIDDEN_TOOL="mcp__mem""palace"
BANNED_COMMANDS=("git push" "gh api" "gh pr create" "gh pr merge" "gh pr close" "gh release" "npm install" "npm i" "npm add" "npm publish" "docker push" "docker build --push" "docker compose up" "docker system prune" "rm -rf" "rm -r")
REQUIRED_ASK=("Bash(git push:*)" "Bash(gh api:*)" "Bash(gh pr create:*)" "Bash(gh pr merge:*)" "Bash(gh pr close:*)" "Bash(npm install:*)")
FORBIDDEN_AGENTS=("git-""provider" "hel""m-values")

# frontmatter_value FILE KEY -> the value of KEY in the first frontmatter block
# (empty if the block or the key is missing).
frontmatter_value() {
  awk -v key="$2" '
    NR == 1 { if ($0 != "---") exit; infm = 1; next }
    infm && $0 == "---" { exit }
    infm {
      prefix = key ":"
      if (index($0, prefix) == 1) {
        v = substr($0, length(prefix) + 1)
        sub(/^[[:space:]]+/, "", v)
        sub(/[[:space:]]+$/, "", v)
        print v
        exit
      }
    }
  ' "$1"
}

# has_frontmatter FILE -> 0 if line 1 is --- and the closing --- is within the
# next 30 lines (a --- far down in the body does not close the block).
has_frontmatter() {
  [ "$(sed -n '1p' "$1")" = "---" ] && sed -n '2,31p' "$1" | grep -qx -- '---'
}

# grant_violation ENTRY -> prints the reason and returns 0 if the allow entry
# grants a banned command; returns 1 for an acceptable entry.
grant_violation() {
  local entry="$1" cmd banned wild=0
  case "$entry" in
    Bash)
      echo "a bare Bash entry grants every command"
      return 0
      ;;
    Bash\(*\))
      cmd="${entry#Bash(}"
      cmd="${cmd%)}"
      ;;
    *) return 1 ;; # another tool: out of scope
  esac
  case "$cmd" in
    *:\*)
      cmd="${cmd%:\*}"
      wild=1
      ;;
    *\*)
      cmd="${cmd%\*}"
      wild=1
      ;;
  esac
  cmd="${cmd%"${cmd##*[![:space:]]}"}"
  if [ -z "$cmd" ]; then
    echo "an empty or all-matching command grants every command"
    return 0
  fi
  case "$cmd" in
    *\**)
      echo "a wildcard in the middle of a command cannot be checked"
      return 0
      ;;
    make | make\ *)
      if [ "$wild" -eq 1 ]; then
        echo "make entries must be exact (no wildcard): a wildcard also runs make clean or make prune"
        return 0
      fi
      ;;
    *release* | *publish*)
      echo "a release or publish command"
      return 0
      ;;
  esac
  for banned in "${BANNED_COMMANDS[@]}"; do
    case "$cmd" in
      "$banned" | "$banned "*)
        echo "grants '$banned'"
        return 0
        ;;
    esac
    if [ "$wild" -eq 1 ]; then
      case "$banned" in
        "$cmd"*)
          echo "covers '$banned'"
          return 0
          ;;
      esac
    fi
  done
  return 1
}

# run_checks ROOT -> prints one line per problem; returns 1 if any.
run_checks() {
  local root="$1" rc=0 f base name desc found=0 forbidden
  local agents_dir="$root/.claude/agents"
  local skill="$root/.claude/skills/ship-issue/SKILL.md"
  local settings="$root/.claude/settings.json"

  for f in "$agents_dir"/*.md; do
    [ -f "$f" ] || continue
    found=1
    base="$(basename "$f" .md)"

    if ! has_frontmatter "$f"; then
      echo "$f: no frontmatter block (--- on line 1 and a closing ---)"
      rc=1
      continue
    fi
    name="$(frontmatter_value "$f" name)"
    desc="$(frontmatter_value "$f" description)"
    if [ -z "$name" ]; then
      echo "$f: frontmatter has no name"
      rc=1
    elif [ "$name" != "$base" ]; then
      echo "$f: frontmatter name '$name' differs from the file name '$base'"
      rc=1
    fi
    if [ -z "$desc" ]; then
      echo "$f: frontmatter has no description"
      rc=1
    fi
    if grep -qF -- "$FORBIDDEN_TOOL" "$f"; then
      echo "$f: mentions a memory-store MCP tool ($FORBIDDEN_TOOL...)"
      rc=1
    fi
    for forbidden in "${FORBIDDEN_AGENTS[@]}"; do
      if grep -qF -- "$forbidden" "$f"; then
        echo "$f: mentions the agent '$forbidden', which does not belong in this template"
        rc=1
      fi
    done
  done
  if [ "$found" -eq 0 ]; then
    echo "$agents_dir: no agent files found"
    rc=1
  fi

  for forbidden in "${FORBIDDEN_AGENTS[@]}"; do
    if [ -e "$agents_dir/$forbidden.md" ]; then
      echo "$agents_dir/$forbidden.md: this agent does not belong in this template"
      rc=1
    fi
  done

  for f in "$skill" "$settings"; do
    if [ ! -f "$f" ]; then
      echo "$f: file not found"
      rc=1
      continue
    fi
    if grep -qF -- "$FORBIDDEN_TOOL" "$f"; then
      echo "$f: mentions a memory-store MCP tool ($FORBIDDEN_TOOL...)"
      rc=1
    fi
    for forbidden in "${FORBIDDEN_AGENTS[@]}"; do
      if grep -qF -- "$forbidden" "$f"; then
        echo "$f: mentions the agent '$forbidden', which does not belong in this template"
        rc=1
      fi
    done
  done

  # Rule 6: a table row of the skill names an agent that exists.
  if [ -f "$skill" ]; then
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      if [ ! -f "$agents_dir/$name.md" ]; then
        echo "$skill: names the agent '$name' but $agents_dir/$name.md does not exist"
        rc=1
      fi
    done < <(sed -nE 's/^\| ([a-z][a-z-]*) \|.*/\1/p' "$skill")
  fi

  # Rules 7 and 8: the allow list grants nothing broad, the ask list is intact.
  if [ -f "$settings" ]; then
    if ! command -v jq >/dev/null 2>&1; then
      echo "$settings: jq is required to check the permissions"
      rc=1
    elif ! jq -e . "$settings" >/dev/null 2>&1; then
      echo "$settings: not valid JSON"
      rc=1
    else
      local entries reason required
      entries="$(jq -r '(.permissions.allow // [])[] | tostring' "$settings")"
      if [ -z "$entries" ]; then
        echo "$settings: no \"allow\" list found"
        rc=1
      fi
      while IFS= read -r name; do
        [ -n "$name" ] || continue
        if reason="$(grant_violation "$name")"; then
          echo "$settings: the allow entry \"$name\" is too broad: $reason"
          rc=1
        fi
      done <<EOF
$entries
EOF
      for required in "${REQUIRED_ASK[@]}"; do
        if ! jq -e --arg e "$required" '(.permissions.ask // []) | index($e)' "$settings" >/dev/null 2>&1; then
          echo "$settings: the ask list lacks \"$required\""
          rc=1
        fi
      done
    fi
  fi

  return "$rc"
}

self_test() {
  local tmp failed=0
  local ASK_ENTRIES e k entries sep
  ASK_ENTRIES="$(printf '"%s",' "${REQUIRED_ASK[@]}")"
  ASK_ENTRIES="${ASK_ENTRIES%,}"
  tmp="$(mktemp -d)" || return 1

  # mk_fixture NAME: a passing fixture under $tmp/NAME.
  mk_fixture() {
    local d="$tmp/$1/.claude"
    mkdir -p "$d/agents" "$d/skills/ship-issue"
    cat >"$d/agents/alpha.md" <<'MD'
---
name: alpha
description: First agent.
tools: Read, Glob, Grep, Bash
---

Body.
MD
    cat >"$d/agents/beta.md" <<'MD'
---
name: beta
description: Second agent.
tools: Read, Edit
model: haiku
---

Body.
MD
    cat >"$d/skills/ship-issue/SKILL.md" <<'MD'
---
name: ship-issue
description: Fixture skill.
---

| Agent | Use for |
| --- | --- |
| alpha | one thing |
| beta | another |
MD
    cat >"$d/settings.json" <<'JSON'
{
  "permissions": {
    "allow": [
      "Bash(make lint)",
      "Bash(git add:*)"
    ],
    "ask": [
      "Bash(git push:*)",
      "Bash(gh api:*)",
      "Bash(gh pr create:*)",
      "Bash(gh pr merge:*)",
      "Bash(gh pr close:*)",
      "Bash(npm install:*)"
    ]
  }
}
JSON
  }

  # expect NAME pass|fail
  expect() {
    local d="$tmp/$1" rc out
    out="$(run_checks "$d" 2>&1)"
    rc=$?
    if { [ "$2" = pass ] && [ "$rc" -eq 0 ]; } || { [ "$2" = fail ] && [ "$rc" -eq 1 ]; }; then
      echo "PASS: $1 ($2)"
      # Show the problem lines of a fixture that must fail (fixture paths cut).
      [ "$2" = fail ] && printf '%s\n' "$out" | sed "s|$tmp/||g; s|^|      |"
    else
      echo "FAIL: $1 expected $2, got rc=$rc"
      [ -n "$out" ] && echo "$out"
      failed=$((failed + 1))
    fi
  }

  mk_fixture good
  expect good pass

  mk_fixture no-agents
  rm "$tmp/no-agents/.claude/agents/"*.md
  expect no-agents fail

  mk_fixture no-frontmatter
  printf 'Body only.\n' >"$tmp/no-frontmatter/.claude/agents/alpha.md"
  expect no-frontmatter fail

  mk_fixture no-name
  sed -i.bak '/^name:/d' "$tmp/no-name/.claude/agents/alpha.md"
  expect no-name fail

  mk_fixture wrong-name
  sed -i.bak 's/^name: alpha/name: gamma/' "$tmp/wrong-name/.claude/agents/alpha.md"
  expect wrong-name fail

  mk_fixture no-description
  sed -i.bak '/^description:/d' "$tmp/no-description/.claude/agents/alpha.md"
  expect no-description fail

  mk_fixture empty-description
  sed -i.bak 's/^description:.*/description:/' "$tmp/empty-description/.claude/agents/alpha.md"
  expect empty-description fail

  # A key that only appears in the body does not count as frontmatter.
  mk_fixture description-in-body
  printf -- '---\nname: alpha\n---\n\ndescription: in the body\n' >"$tmp/description-in-body/.claude/agents/alpha.md"
  expect description-in-body fail

  mk_fixture memory-tool
  sed -i.bak "s/^tools:.*/tools: Read, ${FORBIDDEN_TOOL}__search/" "$tmp/memory-tool/.claude/agents/alpha.md"
  expect memory-tool fail

  mk_fixture memory-tool-in-body
  printf 'Call %s__search first.\n' "$FORBIDDEN_TOOL" >>"$tmp/memory-tool-in-body/.claude/agents/beta.md"
  expect memory-tool-in-body fail

  mk_fixture forbidden-agent-file
  printf -- '---\nname: %s\ndescription: x\n---\n' "${FORBIDDEN_AGENTS[0]}" >"$tmp/forbidden-agent-file/.claude/agents/${FORBIDDEN_AGENTS[0]}.md"
  expect forbidden-agent-file fail

  mk_fixture forbidden-in-skill
  printf '\nSee %s.\n' "${FORBIDDEN_AGENTS[1]}" >>"$tmp/forbidden-in-skill/.claude/skills/ship-issue/SKILL.md"
  expect forbidden-in-skill fail

  mk_fixture forbidden-in-settings
  sed -i.bak "s/make lint/make ${FORBIDDEN_AGENTS[1]}/" "$tmp/forbidden-in-settings/.claude/settings.json"
  expect forbidden-in-settings fail

  mk_fixture memory-tool-in-settings
  sed -i.bak "s/make lint/${FORBIDDEN_TOOL}__search/" "$tmp/memory-tool-in-settings/.claude/settings.json"
  expect memory-tool-in-settings fail

  mk_fixture skill-names-missing-agent
  printf '| gamma | not ported |\n' >>"$tmp/skill-names-missing-agent/.claude/skills/ship-issue/SKILL.md"
  expect skill-names-missing-agent fail

  mk_fixture no-skill
  rm "$tmp/no-skill/.claude/skills/ship-issue/SKILL.md"
  expect no-skill fail

  mk_fixture no-settings
  rm "$tmp/no-settings/.claude/settings.json"
  expect no-settings fail

  mk_fixture allow-push
  sed -i.bak 's/"Bash(git add:\*)"/"Bash(git push:*)"/' "$tmp/allow-push/.claude/settings.json"
  expect allow-push fail

  mk_fixture allow-merge
  sed -i.bak 's/"Bash(git add:\*)"/"Bash(gh pr merge:*)"/' "$tmp/allow-merge/.claude/settings.json"
  expect allow-merge fail

  mk_fixture allow-rm
  sed -i.bak 's/"Bash(git add:\*)"/"Bash(rm -rf:*)"/' "$tmp/allow-rm/.claude/settings.json"
  expect allow-rm fail

  mk_fixture allow-npm-install
  sed -i.bak 's/"Bash(git add:\*)"/"Bash(npm install:*)"/' "$tmp/allow-npm-install/.claude/settings.json"
  expect allow-npm-install fail

  # write_settings FILE ASK_ENTRIES ALLOW_ENTRIES... : a settings.json with one
  # entry per line (ASK_ENTRIES and the allow entries are JSON text).
  write_settings() {
    local file="$1" ask="$2" entries="" sep="" e
    shift 2
    for e in "$@"; do
      entries="$entries$sep
      $e"
      sep=","
    done
    printf '{\n  "permissions": {\n    "allow": [%s\n    ],\n    "ask": [\n      %s\n    ]\n  }\n}\n' "$entries" "$ask" >"$file"
  }

  # settings NAME ALLOW_ENTRIES... : replace the settings.json of fixture NAME
  # by one with these allow entries and the required ask entries.
  settings() {
    local name="$1"
    shift
    write_settings "$tmp/$name/.claude/settings.json" "$ASK_ENTRIES" "$@"
  }

  # new_settings NAME PASS|FAIL ALLOW_ENTRIES... : a fixture of its own.
  new_settings() {
    local name="$1" want="$2"
    shift 2
    mk_fixture "$name"
    settings "$name" "$@"
    expect "$name" "$want"
  }

  new_settings allow-gh-api fail '"Bash(gh api:*)"'
  new_settings allow-gh-api-exact fail '"Bash(gh api repos/x/y)"'
  new_settings allow-pr-create fail '"Bash(gh pr create:*)"'
  new_settings allow-pr-close fail '"Bash(gh pr close:*)"'
  new_settings allow-release fail '"Bash(gh release create:*)"'
  new_settings allow-publish fail '"Bash(npm publish:*)"'
  new_settings allow-docker-push fail '"Bash(docker push:*)"'
  new_settings allow-docker-build fail '"Bash(docker build:*)"'
  new_settings allow-star fail '"Bash(*)"'
  new_settings allow-bare-bash fail '"Bash"'
  new_settings allow-git-wild fail '"Bash(git:*)"'
  new_settings allow-git-space-star fail '"Bash(git *)"'
  new_settings allow-gh-wild fail '"Bash(gh:*)"'
  new_settings allow-npm-wild fail '"Bash(npm:*)"'
  new_settings allow-npm-i fail '"Bash(npm i:*)"'
  new_settings allow-rm-wild fail '"Bash(rm:*)"'
  new_settings allow-rm-exact fail '"Bash(rm -rf build)"'
  new_settings allow-make-wild fail '"Bash(make:*)"'
  new_settings allow-make-prefix fail '"Bash(make lint:*)"'
  new_settings allow-middle-star fail '"Bash(git * push)"'
  # An entry with a closing bracket must not hide the entries after it.
  new_settings allow-bracket-entry fail '"Bash(git add:*)"' '"Bash(echo ]:*)"' '"Bash(git push:*)"'
  new_settings allow-exact-make pass '"Bash(make lint)"' '"Bash(make test-backend-unit)"' '"Bash(git add:*)"'

  # The first allow entry on the line of "allow": [ is read too.
  mk_fixture allow-same-line
  printf '{\n  "permissions": {\n    "allow": ["Bash(git push:*)",\n      "Bash(make lint)",\n      "Bash(git add:*)"\n    ],\n    "ask": [%s]\n  }\n}\n' "$ASK_ENTRIES" >"$tmp/allow-same-line/.claude/settings.json"
  expect allow-same-line fail

  mk_fixture settings-not-json
  printf '{"permissions": {"allow": [\n' >"$tmp/settings-not-json/.claude/settings.json"
  expect settings-not-json fail

  # Each required ask entry is needed: drop one at a time.
  for e in "${REQUIRED_ASK[@]}"; do
    entries="" sep=""
    for k in "${REQUIRED_ASK[@]}"; do
      [ "$k" = "$e" ] && continue
      entries="$entries$sep\"$k\""
      sep=","
    done
    mk_fixture "ask-without-$e"
    write_settings "$tmp/ask-without-$e/.claude/settings.json" "$entries" '"Bash(make lint)"'
    expect "ask-without-$e" fail
  done

  mk_fixture no-ask-list
  printf '{\n  "permissions": {\n    "allow": [\n      "Bash(make lint)"\n    ]\n  }\n}\n' >"$tmp/no-ask-list/.claude/settings.json"
  expect no-ask-list fail

  # The memory-store server name alone (no tool suffix) is rejected too.
  mk_fixture memory-server-name
  printf 'Uses %s only.\n' "${FORBIDDEN_TOOL%__}" >>"$tmp/memory-server-name/.claude/agents/alpha.md"
  expect memory-server-name fail

  # A closing --- far away in the body does not close the frontmatter.
  mk_fixture unclosed-frontmatter
  {
    printf -- '---\nname: alpha\ndescription: x\n'
    for _ in $(seq 1 40); do printf 'text\n'; done
    printf -- '---\n'
  } >"$tmp/unclosed-frontmatter/.claude/agents/alpha.md"
  expect unclosed-frontmatter fail

  # git push on the ask list is the intended place and passes (good fixture).

  rm -rf "$tmp"
  if [ "$failed" -ne 0 ]; then
    echo "$failed self-test(s) failed"
    return 1
  fi
  echo "self-test OK"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

if [ "$#" -gt 0 ]; then
  echo "usage: scripts/check-agents.sh [--self-test]" >&2
  exit 2
fi

if run_checks "${AGENTS_ROOT:-$REPO_ROOT}"; then
  echo "agents OK"
else
  exit 1
fi

#!/usr/bin/env bash
# scripts/check-agents.sh
# Keeps the Claude Code agents, the ship-issue skill and the project settings
# clean and consistent.
#
#   1. .claude/agents/ holds at least one agent file (*.md).
#   2. Every agent file starts with a frontmatter block that has a non-empty
#      name (equal to the file name without .md) and a non-empty description.
#   3. No agent file mentions a memory-store MCP tool (the tool prefix in
#      FORBIDDEN_TOOL below), in the tools list or anywhere else.
#   4. No agent file exists for a name in FORBIDDEN_AGENTS (agents of the
#      project this template came from that do not belong in a template).
#   5. .claude/skills/ship-issue/SKILL.md and .claude/settings.json exist, and
#      neither them nor any agent file mentions a forbidden tool or agent name.
#   6. Every agent named in the first column of a table in the skill has an
#      agent file.
#   7. The "allow" list of .claude/settings.json grants nothing that publishes
#      or deletes: no git push, no gh api/pr create/pr merge/pr close, no
#      npm install, no rm, no release or publish command. Those stay on the
#      "ask" list or are not listed.
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

FORBIDDEN_TOOL="mcp__mem""palace__"
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

# has_frontmatter FILE -> 0 if line 1 is --- and a closing --- follows.
has_frontmatter() {
  [ "$(sed -n '1p' "$1")" = "---" ] && sed -n '2,$p' "$1" | grep -qx -- '---'
}

# allow_entries SETTINGS -> the lines of the "allow" array, one per line.
allow_entries() {
  awk '
    /"allow"[[:space:]]*:[[:space:]]*\[/ { inallow = 1; next }
    inallow && /\]/ { exit }
    inallow { print }
  ' "$1"
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
      echo "$f: mentions a memory-store MCP tool ($FORBIDDEN_TOOL*)"
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
      echo "$f: mentions a memory-store MCP tool ($FORBIDDEN_TOOL*)"
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

  # Rule 7: nothing that publishes or deletes on the allow list.
  if [ -f "$settings" ]; then
    if [ -z "$(allow_entries "$settings")" ]; then
      echo "$settings: no \"allow\" list found"
      rc=1
    fi
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      echo "$settings: the allow list grants a publishing or deleting command: ${name#"${name%%[![:space:]]*}"}"
      rc=1
    done < <(allow_entries "$settings" | grep -E 'git push|gh api|gh pr (create|merge|close)|npm install|rm |release|publish|Bash\(\*' || true)
  fi

  return "$rc"
}

self_test() {
  local tmp failed=0
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
      "Bash(make lint:*)",
      "Bash(git add:*)"
    ],
    "ask": [
      "Bash(git push:*)",
      "Bash(gh pr merge:*)"
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
  sed -i.bak "s/^tools:.*/tools: Read, ${FORBIDDEN_TOOL}search/" "$tmp/memory-tool/.claude/agents/alpha.md"
  expect memory-tool fail

  mk_fixture memory-tool-in-body
  printf 'Call %ssearch first.\n' "$FORBIDDEN_TOOL" >>"$tmp/memory-tool-in-body/.claude/agents/beta.md"
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
  sed -i.bak "s/make lint/${FORBIDDEN_TOOL}search/" "$tmp/memory-tool-in-settings/.claude/settings.json"
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

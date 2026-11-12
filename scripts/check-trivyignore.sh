#!/bin/sh
# Checks the Trivy ignore file (default .trivyignore, override with TRIVYIGNORE).
# Every entry line must
#   - have a "#" comment line directly above it that names the reason, and
#   - carry exactly one "exp:YYYY-MM-DD" date that is at most 90 days from today.
# Blank lines are skipped. A missing or empty file passes.
# Prints "file:line: reason" for each violation and exits 1 if there is any.
#
# Self-test: scripts/check-trivyignore.sh --self-test

# The date that is $1 days from today (GNU date on Linux runners, BSD date on macOS).
days_from_today() {
  date -d "+$1 days" +%F 2>/dev/null || date -v+"$1"d +%F
}

check_file() {
  file="$1"
  [ -f "$file" ] || return 0

  limit=$(days_from_today 90) || return 2
  today=$(date +%F)

  status=0
  lineno=0
  prev_comment=0

  while IFS= read -r line || [ -n "$line" ]; do
    lineno=$((lineno + 1))
    trimmed=$(printf '%s' "$line" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')

    if [ -z "$trimmed" ]; then
      prev_comment=0
      continue
    fi

    case "$trimmed" in
      '#'*)
        # Only a comment with text counts as a reason.
        if [ -n "$(printf '%s' "$trimmed" | sed 's/^#*[[:space:]]*//')" ]; then
          prev_comment=1
        else
          prev_comment=0
        fi
        continue
        ;;
    esac

    if [ "$prev_comment" -ne 1 ]; then
      echo "$file:$lineno: entry has no '#' reason comment on the line above" >&2
      status=1
    fi
    prev_comment=0

    # Trivy reads the first exp: token, so a second one could hide a long date.
    exp_count=$(printf '%s\n' "$trimmed" | grep -o 'exp:' | wc -l | tr -d '[:space:]')
    if [ "$exp_count" -gt 1 ]; then
      echo "$file:$lineno: entry has more than one exp: date" >&2
      status=1
      continue
    fi

    exp=$(printf '%s' "$trimmed" | sed -n 's/.*[[:space:]]exp:\([^[:space:]]*\).*/\1/p')
    if [ -z "$exp" ]; then
      echo "$file:$lineno: entry has no exp:YYYY-MM-DD date" >&2
      status=1
      continue
    fi

    # Form check, then a real calendar date (round trip through date).
    case "$exp" in
      [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
      *)
        echo "$file:$lineno: exp date '$exp' is not in the form YYYY-MM-DD" >&2
        status=1
        continue
        ;;
    esac
    norm=$(date -d "$exp" +%F 2>/dev/null || date -j -f %F "$exp" +%F 2>/dev/null)
    if [ "$norm" != "$exp" ]; then
      echo "$file:$lineno: exp date '$exp' is not a valid date" >&2
      status=1
      continue
    fi

    # ISO dates compare as numbers once the dashes are gone (YYYYMMDD).
    if [ "$(printf '%s' "$exp" | tr -d -)" -gt "$(printf '%s' "$limit" | tr -d -)" ]; then
      echo "$file:$lineno: exp date $exp is more than 90 days from today ($today); latest allowed is $limit" >&2
      status=1
    fi
  done < "$file"

  return "$status"
}

self_test() {
  tmp=$(mktemp -d) || return 1
  failed=0
  ok_date=$(days_from_today 30)
  far_date=$(days_from_today 200)

  # expect NAME pass|fail ; the fixture is on stdin
  expect() {
    cat >"$tmp/$1"
    check_file "$tmp/$1" 2>/dev/null
    rc=$?
    if { [ "$2" = pass ] && [ "$rc" -eq 0 ]; } || { [ "$2" = fail ] && [ "$rc" -eq 1 ]; }; then
      echo "PASS: $1 ($2)"
    else
      echo "FAIL: $1 expected $2, got rc=$rc"
      failed=$((failed + 1))
    fi
  }

  printf '' | expect empty pass
  printf '# header only\n' | expect header-only pass
  printf '# reason\nCVE-2026-1 exp:%s\n' "$ok_date" | expect good pass
  printf 'CVE-2026-1 exp:%s\n' "$ok_date" | expect no-reason fail
  printf '# reason\nCVE-2026-1\n' | expect no-date fail
  printf '# reason\nCVE-2026-1 exp:%s\n' "$far_date" | expect too-far fail
  printf '# reason\nCVE-2026-1 exp:2026-02-30\n' | expect bad-date fail
  printf '# reason\nCVE-2026-1 exp:%s exp:%s\n' "$ok_date" "$far_date" | expect two-dates fail
  printf '#\nCVE-2026-1 exp:%s\n' "$ok_date" | expect empty-comment fail

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

check_file "${TRIVYIGNORE:-.trivyignore}"
exit $?

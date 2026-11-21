---
name: check-pr
description: Check the CI of a pull request, summarize each failing job with its cause, and re-run only the failed jobs after the user agrees. Use to check CI on a pull request.
argument-hint: "[pull request number, default: the current branch]"
disable-model-invocation: true
---

# Check a pull request

1. Find the pull request: the number in the arguments, else `gh pr view --json number,url,headRefName,mergeStateStatus`.
2. `gh pr checks <number>`. Group the checks as passed, failed, pending and skipped. Required checks are listed in `RELEASING.md`.
3. For each failed check, read only the failing step: `gh run view <run-id> --log-failed`. When the log is over about 5 KB, write it to a file and use `ask_local` for the list of failures, then verify each one in the real log.
4. Report one line per failed job: job, failing step, error text, and your reading of the cause: **ours** (the change broke it) or **not ours** (a flaky test, a runner or tooling problem, a failure in code the change did not touch). Check the second kind by comparing with the latest run on `main` (`gh run list --branch main --workflow <name> --limit 3`).
5. Pending checks: report the count and stop. Do not poll in a loop.
6. A failure that is ours: fix it on the branch, run the matching local check, and stop before the push.
   A failure that is not ours: ask the user before `gh run rerun <run-id> --failed`. Re-run once. When it fails again, report it. Do not fix unrelated code.
7. Never merge. When everything is green, say so and name the pull request's merge state.

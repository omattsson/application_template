---
name: ship-issue
description: Spec, build, verify and review a GitHub issue or feature in this repository by starting the specialist agents (go-api-developer, frontend-developer, devops-engineer, qa-engineer, code-reviewer) from the main session. Runs without stopping and asks the user only before a push or when a decision is theirs. Use for multi-step work across backend, frontend, tests, and infrastructure.
argument-hint: "<issue number or feature description>"
---

# Ship an issue

You are the tech lead in the main session. You run the whole flow yourself: spec, plan, build, verify, review, commit. Keep going without the user unless a rule in "Ask the user" applies. Always stop before a push.

Start the specialist agents directly. A planning subagent would repeat the work of the main session, and the results of every agent that it started would stay in its context.

## Ask the user

Stop and ask when:

- the request allows two readings that lead to different behavior, a different API, or different data;
- the change alters the auth or security model (sessions, tokens, roles, API keys, OIDC, the audit log);
- a migration rewrites or drops data;
- the change adds a dependency (a Go module, an npm package, a base image);
- the work grows beyond the spec;
- a check fails in code that the change did not touch, and fails again after one re-run (a flaky test, a CI or tooling problem). Report it. Do not fix unrelated code on the side;
- the review loop stops making progress: a finding comes back after its fix, or a fix round ends with as many findings to fix as it started with.

Ask all open questions at once (`AskUserQuestion`), each with its options and your recommendation first. Decide everything else yourself and write the decision under Assumptions in the spec. Names, file layout and test cases are never a reason to stop.

## 1. Spec

1. Read the issue: `gh issue view <number> --comments`. `CLAUDE.md` is loaded already. Read a `docs/claude/*.md` file only for the area that the issue touches: `auth-sessions.md` (login, sessions, roles, audit), `operations.md` (configuration, shutdown, replicas), `structure.md` (where files live). `EXTENDING.md` is the guide for a new API resource and page.
2. Branch from an up-to-date `main`: `git fetch origin main`, then `git switch -c <type>/<short-name> origin/main`. Never commit on `main`. Use a worktree when another task is open in this checkout.
3. Write the spec to `.claude/work/<branch>.md`, with `/` in the branch name replaced by `-`. The directory is git-ignored, and the spec lets a new session resume the work.
   - **Goal**: one or two sentences.
   - **Acceptance criteria**: numbered, each one testable.
   - **Out of scope**.
   - **Assumptions**: the decisions you made yourself.
   - **Open questions**.
4. When an open question matches "Ask the user", ask now and write the answers into the spec. Otherwise go on.

## 2. Plan

1. Choose a workflow sequence below. Leave out steps that the change does not need.
2. Small change (one package, a few files, no new route or migration): do it in the main session, without agents. An agent costs a fresh context and a summary round trip.
3. Add the plan to the spec: numbered steps, one agent for each step, the acceptance criteria that each step covers, and the checks to run.

## 3. Build

Give each agent a self-contained brief. The agent does not see this conversation.

- The path of the spec, the goal, and the acceptance criteria of its step.
- The files and functions to start from, and the facts that you already found. The agent should not search for them again.
- The branch or worktree to work in.
- Test first: a failing test, then the code. For a bug, a test that fails because of the bug, and a fix of the root cause, not of the symptom.
- The checks to run (see Verify).

The agents end with a fixed report: status, files changed, checks, assumptions, questions. Answer a question yourself when the spec settles it, otherwise ask the user. Write one line per finished step into the spec.

Start independent steps in parallel (for example backend and frontend tests of different files). Start dependent steps in order.

## Team

| Agent | Use for |
| --- | --- |
| go-api-developer | Models, repositories, migrations, handlers, routes, auth, Swagger, backend tests |
| frontend-developer | Pages, components, API services, routing, WebSocket, frontend tests |
| devops-engineer | Docker, Compose, nginx, Makefile, CI/CD workflows |
| qa-engineer | Test strategy, coverage gaps, integration and end-to-end tests |
| code-reviewer | The review loop |
| security-reviewer | Security review of a change to auth, sessions, tokens, roles, API keys, OIDC, MFA, password reset or the audit log |

## Implementation order (backend and frontend)

1. Models and repositories.
2. Handlers and routes.
3. API client (`frontend/src/api/client.ts`).
4. UI pages.
5. Tests, written before the code they cover.

## Workflow sequences

Every sequence ends with the review loop (step 5).

### New API resource (backend and frontend)

1. go-api-developer: model, validation, migration, repository, handler, routes, audit route entry, Swagger, backend tests (`EXTENDING.md`).
2. frontend-developer: API service, page, routing, navigation, frontend tests.
3. qa-engineer: coverage gaps across both, and the end-to-end spec.

### Auth or OpenID Connect (OIDC) change

Confirm the change with the user in the spec step: it alters the auth model.

1. go-api-developer: auth handler or middleware, session store, OIDC provider (`docs/claude/auth-sessions.md`).
2. qa-engineer: all auth paths (login, refresh, OIDC callback, API key, disabled user), with auth on and off.
3. frontend-developer: Login page, AuthCallback, AuthContext.
4. The review loop focuses on security: the panel's B reviewer is a security-reviewer instead of a code-reviewer.

### Configuration, Docker, or CI change

1. devops-engineer: Compose, Dockerfiles, nginx, workflows, Makefile. A new environment variable also goes into both `.env.example` files and `operations.md`.
2. go-api-developer: the config package, when the change adds or renames a variable.
3. The review loop adds `make check-workflows` and `make check-env-docs`.

### Bug fix

1. Write a test that fails because of the bug (qa-engineer, or in the main session for a small fix). Run it and see it fail.
2. go-api-developer or frontend-developer: find the root cause and fix it, unless the main session already can. The test passes.

### Backend-only or frontend-only change

1. The matching developer agent builds the change, with tests.
2. Add qa-engineer only when the review finds coverage gaps.

## 4. Verify

Run the checks that match the change yourself. Report a check as passing only when you ran it in this session and saw it pass.

- Backend: `make test-backend-unit`. After a database change, `make test-backend-all`. After a handler change, `make docs` and commit the regenerated files.
- Frontend: `cd frontend && npm test`, then `make lint`.
- Behavior across the whole application: `make test-e2e`, and `make test-e2e-auth` for a change that touches auth.
- Always: `make lint` (go vet, ESLint, workflow, env docs, and agent checks), `make lint-docs` when you changed a Markdown file, and `scripts/check-domain-terms.sh`.
- A change to `.github/workflows/`: `make check-workflows`. A change to a Dockerfile: `scripts/check-dockerfiles.sh`. A change to `.claude/`: `make check-agents`.

## 5. Review loop

A small change (see step 2 of Plan) or a change of dependency files only (`go.mod`, `go.sum`, `package-lock.json`) gets one code-reviewer: start it with the path of the spec, the branch and the diff base (`origin/main`). `VERDICT: CHANGES REQUIRED` sends its Critical and Important findings to the agent that owns the code, then a re-review of the fix diff follows, until `VERDICT: APPROVE`.

Every other change gets a panel of three reviewers that vote:

1. Start three code-reviewer agents in parallel. Each gets the path of the spec, the branch, the diff base (`origin/main`) and one focus:
   - A: correctness and the acceptance criteria of the spec;
   - B: security and auth;
   - C: tests, project patterns and scalability.
2. Merge the three reports into one list. Drop duplicates, keep the highest severity, and number the findings F1, F2, and so on.
3. Send the merged list to the same three reviewers and ask each one to vote on every finding: `FIX`, `LATER` or `INVALID`, with a reason. Continue the running agents (for example with `SendMessage`), so they keep their context. Start new ones only when that is not possible.
4. Decide each finding:
   - two or three `FIX` votes: fix it;
   - a Critical finding: fix it unless two reviewers vote `INVALID`;
   - anything else: a Suggestion for the hand-over.
5. Nothing to fix: go to step 6. Otherwise send the findings to fix, with their IDs, to the agent that owns the code. Then the three reviewers re-review the fix diff only, and vote on the fixed findings and on any new ones.
6. Repeat until no finding is left to fix. There is no fixed number of rounds. When a round makes no progress (see "Ask the user"), stop and ask the user, with the open findings and their votes.

For both:

- Do not apply Suggestions (nit or low). List them for the user in step 6.
- A large diff: after the last round, run a simplification pass on the diff (the `simplify` skill when your Claude Code has it). Keep only the changes after which every check of Verify still passes.

## 6. Commit and hand over

1. Commit on the branch with a conventional commit message.
2. Stop and show the user:
   - the path of the spec, and each acceptance criterion as met or not met;
   - `git diff --stat origin/main...HEAD`;
   - the checks you ran, one line each;
   - the review: one reviewer or the panel, rounds, findings fixed, the votes on findings that were not unanimous, and Suggestions left;
   - your assumptions and the risks.
3. Ask whether to push and open a pull request. Do both only after a yes. `git push` and `gh pr create` ask for permission as well.
4. A pull request targets `main`. Its title must be a conventional commit title, because `.github/workflows/pr-title.yml` enforces it: `type: subject` or `type(scope): subject`, with a type of `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`, or `deps`. Write the title as the commit subject you want in the history.
5. Never merge the pull request, and never merge a branch into `main`. A person reviews and merges. Branch protection and the required checks are in `RELEASING.md`.
6. When the context of this session is past about 40%, hand over before the next large step: run `/handoff` if you have that skill, or `/compact` with a focus on the remaining work. The spec holds the state.

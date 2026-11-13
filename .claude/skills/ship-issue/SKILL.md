---
name: ship-issue
description: Plan a GitHub issue or feature in this repository and carry it out by starting the specialist agents (go-api-developer, frontend-developer, devops-engineer, qa-engineer, code-reviewer) from the main session. Use for multi-step work across backend, frontend, tests, and infrastructure.
argument-hint: "<issue number or feature description>"
---

# Ship an issue

You are the tech lead in the main session. Plan the work, start the specialist agents directly, and track the result. A planning subagent would repeat the work of the main session, and the results of every agent that it started would stay in its context.

## Plan

1. Read the issue: `gh issue view <number>`. `CLAUDE.md` is loaded already. Read a `docs/claude/*.md` file only for the area that the issue touches: `auth-sessions.md` (login, sessions, roles, audit), `operations.md` (configuration, shutdown, replicas), `structure.md` (where files live). `EXTENDING.md` is the guide for a new API resource and page.
2. Branch from an up-to-date `main`: `git fetch origin main`, then `git switch -c <type>/<short-name> origin/main`. Never commit on `main`. Use a worktree when another task is open in this checkout.
3. Choose a workflow sequence below. Leave out steps that the change does not need.
4. Small change (one package, a few files, no new route or migration): do it in the main session, without agents. An agent costs a fresh context and a summary round trip.
5. Write a short numbered plan: one agent for each step, with acceptance criteria. Show it to the user before the first agent starts when the scope is not clear.

## Delegate

Give each agent a self-contained brief. The agent does not see this conversation.

- The goal, the acceptance criteria, and the issue number.
- The files and functions to start from, and the facts that you already found. The agent should not search for them again.
- The branch or worktree to work in.
- The rule to work test first (a failing test, then the code), and which checks to run (see Verify).
- Ask for a short report: files changed, tests run with their results, and open questions. No full diffs and no full command output.

Start independent steps in parallel (for example backend and frontend tests of different files). Start dependent steps in order.

## Team

| Agent | Use for |
| --- | --- |
| go-api-developer | Models, repositories, migrations, handlers, routes, auth, Swagger, backend tests |
| frontend-developer | Pages, components, API services, routing, WebSocket, frontend tests |
| devops-engineer | Docker, Compose, nginx, Makefile, CI/CD workflows |
| qa-engineer | Test strategy, coverage gaps, integration and end-to-end tests |
| code-reviewer | Review of finished work, security review |

## Implementation order (backend and frontend)

1. Models and repositories.
2. Handlers and routes.
3. API client (`frontend/src/api/client.ts`).
4. UI pages.
5. Tests, written before the code they cover.

## Workflow sequences

### New API resource (backend and frontend)

1. go-api-developer: model, validation, migration, repository, handler, routes, audit route entry, Swagger, backend tests (`EXTENDING.md`).
2. frontend-developer: API service, page, routing, navigation, frontend tests.
3. qa-engineer: coverage gaps across both, and the end-to-end spec.
4. code-reviewer: full review.

### Auth or OpenID Connect (OIDC) change

1. go-api-developer: auth handler or middleware, session store, OIDC provider (`docs/claude/auth-sessions.md`).
2. qa-engineer: all auth paths (login, refresh, OIDC callback, API key, disabled user), with auth on and off.
3. frontend-developer: Login page, AuthCallback, AuthContext.
4. code-reviewer: security-focused review.

### Configuration, Docker, or CI change

1. devops-engineer: Compose, Dockerfiles, nginx, workflows, Makefile. A new environment variable also goes into both `.env.example` files and `operations.md`.
2. go-api-developer: the config package, when the change adds or renames a variable.
3. code-reviewer: review, with `make check-workflows` and `make check-env-docs`.

### Bug fix

1. Write a test that fails because of the bug (qa-engineer, or in the main session for a small fix). Run it and see it fail.
2. go-api-developer or frontend-developer: fix the bug, unless the main session already can. The test passes.
3. code-reviewer: review the fix.

### Backend-only or frontend-only change

1. The matching developer agent builds the change, with tests.
2. code-reviewer: review. Add qa-engineer only when the review finds coverage gaps.

## Verify

Run the checks that match the change before you report done:

- Backend: `make test-backend-unit`. After a database change, `make test-backend-all`. After a handler change, `make docs` and commit the regenerated files.
- Frontend: `cd frontend && npm test`, then `make lint`.
- Behavior across the whole application: `make test-e2e`, and `make test-e2e-auth` for a change that touches auth.
- Always: `make lint` (go vet, ESLint, workflow, env docs, and agent checks), `make lint-docs` when you changed a Markdown file, and `scripts/check-domain-terms.sh`.
- A change to `.github/workflows/`: `make check-workflows`. A change to a Dockerfile: `scripts/check-dockerfiles.sh`. A change to `.claude/`: `make check-agents`.

## Finish

1. Check the reports against the acceptance criteria. Run the checks above yourself before you report done.
2. Commit on the branch with a conventional commit message. Push and open a pull request only when the user asks.
3. A pull request targets `main`. Its title must be a conventional commit title, because `.github/workflows/pr-title.yml` enforces it: `type: subject` or `type(scope): subject`, with a type of `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`, or `deps`. Write the title as the commit subject you want in the history.
4. Never merge the pull request, and never merge a branch into `main`. A person reviews and merges. Branch protection and the required checks are in `RELEASING.md`.
5. When the context of this session is past about 40%, hand over before the next large step: run `/handoff` if you have that skill, or `/compact` with a focus on the remaining work.

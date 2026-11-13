# Releasing

How changes reach `main`, which checks run, how to protect the branch and how to cut a release. The template ships no release automation and no published images: a project built from it adds those to suit its registry and hosting.

## Versions

A release has a tag `vX.Y.Z` on a merge commit of `main`. Use semantic versioning: a breaking change of the API, the configuration or the database that needs an upgrade note raises the major (or, before 1.0, the minor) version, a feature the minor version, anything else the patch version.

Toolchain versions are single sources of truth:

- Go: `backend/go.mod` (`go 1.27.4`). CI reads it (`go-version-file`); the Dockerfiles use the same `golang:` tag and `scripts/check-dockerfiles.sh` fails when they differ. Change them together.
- Node: `frontend/package.json` `engines` (22.12 or later), `frontend/.nvmrc` (the major used by CI and the image).
- MySQL: 8.0.17 or later (`docker-compose.yml` runs `mysql:8.0`).

## Pull requests

- Open the pull request against `main` with a conventional commit title, for example `fix(ui): ...` or `feat(api): ...`. Allowed types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`, `deps`.
- The title check is its own workflow, `.github/workflows/pr-title.yml` (job name `PR title`). It is separate from `pull-request.yml` on purpose: it must also run when the title is edited, and a title edit must not cancel, skip or replace the real checks. It was a job of `pull-request.yml` earlier: **a repository whose branch protection requires the old check must select `PR title` again** in the list of required status checks.
- Dependabot (`.github/dependabot.yml`) opens weekly pull requests for Go modules, npm packages, GitHub Actions and Docker base images with the title prefixes `deps(go):`, `deps(npm):`, `deps(actions):` and `deps(docker):`.

## Checks

| Workflow | Jobs | Runs when |
|---|---|---|
| `pull-request.yml` | `Repository checks` (workflow hygiene, Dockerfile hardening, Trivy ignore list, environment variable docs, domain terms, shell scripts), `Backend` (vet, gosec, unit tests with the race detector, integration tests on MySQL with `-p 1`), `Frontend` (lint, build, unit tests), `Markdown lint` | every pull request to `main`, every push to `main` |
| `pr-title.yml` | `PR title` | pull request opened, edited, synchronized, reopened |
| `docker-build.yml` | `Build backend image`, `Build frontend image` (production target, not pushed) | changes under `backend/`, `frontend/` or the workflow |
| `security-scan.yml` | `.trivyignore expiry`, `Trivy (filesystem)`, `Trivy (<image> image)` | pull request, push to `main`, weekly |
| `codeql.yml` | `Analyze (go)`, `Analyze (javascript-typescript)` | pull request and push to `main` except Markdown and `docs/` only changes, weekly |
| `e2e.yml` | `Playwright (auth off, auth on, secure cookies)`: `make test-e2e`, `make test-e2e-auth`, `make test-e2e-auth E2E_SECURE_COOKIES=true` | changes under `frontend/`, `backend/`, the Compose file, the Makefile or the e2e script; push to `main`; weekly |

The same checks run locally: `make lint`, `make test`, `make test-backend-all`, `make test-e2e`, `make test-e2e-auth`, `make lint-docs`, `make gosec`, `scripts/check-domain-terms.sh`. The workflows call the Makefile targets, so a developer and CI run the same commands. `make check-workflows` enforces the rules for the workflow files themselves: pinned actions (a full commit SHA with the version in a comment), minimal `permissions`, a `timeout-minutes` for every job and the `main` trigger.

## Branch protection

Protect `main`: require a pull request, require status checks to pass and require the branch to be up to date.

Select as required checks only jobs that run on **every** pull request: `Repository checks`, `Backend`, `Frontend`, `Markdown lint` (workflow `Pull Request Validation`) and `PR title`.

Do **not** make `Analyze (go)`, `Analyze (javascript-typescript)`, the `Build ... image` jobs or the e2e job required. CodeQL skips documentation-only changes, and the image build and e2e workflows are filtered by path, so on a pull request that does not match, the check never runs. GitHub then waits for it forever and the pull request cannot be merged. If you want one of them required, remove the `paths` and `paths-ignore` filters from that workflow first.

`pull-request.yml` cancels an older run of the same pull request when a new commit arrives (`concurrency`), and its jobs have no `if:` conditions. Keep it that way: a skipped required job counts as a pass.

## Releasing

1. Make sure `main` is green (all workflows above, including the scheduled e2e and security scans).
2. Check the upgrade notes: migrations that the release adds (never edited after release; see `backend/CLAUDE.md`, "Migrations"), changed defaults, new or removed environment variables (`backend/.env.example` and `docs/claude/operations.md` list them all), changed minimum MySQL, Go or Node versions.
3. Open a release pull request `chore(release): vX.Y.Z` if the project keeps a version in a file, and merge it.
4. Create the GitHub release `vX.Y.Z` from the merge commit on `main`: write the notes from the merged pull request titles, grouped by type (the conventional title makes this mechanical), and put the upgrade notes at the top.
5. Build and publish the production images from the tag: `docker build --target production backend` and `frontend`. The backend image runs as uid 65532 on Alpine, the frontend as uid 101 (unprivileged nginx on port 8080). `docker-build.yml` only proves that they build; add a push job for your registry.
6. Roll out. The backend runs the migrations when it starts and the replicas wait for each other on a MySQL advisory lock (up to 5 minutes). Give the container a stop grace period longer than five times `SERVER_SHUTDOWN_TIMEOUT` (Compose: 170 s).

## Security fixes

An image scan finding that cannot be fixed at once goes into `.trivyignore` with a reason on the line above and an expiry of at most 90 days (`scripts/check-trivyignore.sh` enforces both). Report vulnerabilities as described in `.github/SECURITY.md`.
